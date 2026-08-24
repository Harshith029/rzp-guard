// Command rzp-guard is the authorization proxy in front of Razorpay's official
// MCP server.
//
// It speaks MCP over stdio to a calling agent and runs the official, unmodified
// razorpay/mcp container as a child process. A denied tools/call is answered
// here and its bytes are never written to the child's stdin.
//
// This is the single startup path: bootstrap.Open is the only way state is
// constructed, and cleanup is guaranteed on every exit route -- agent stdin EOF,
// child stdout EOF, child process exit, or a termination signal. An in-flight
// refund that never got an answer is promoted to IN_DOUBT before the process
// leaves, because a reservation nobody resolved may already have moved money.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/harshith/rzp-guard/internal/bootstrap"
	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/policy"
	"github.com/harshith/rzp-guard/internal/relay"
)

// DefaultToolsets narrows the CHILD's own surface. rzp-guard narrows further
// still, to reads plus create_refund, so the two boundaries are independent
// rather than one restating the other.
const DefaultToolsets = "payments,orders,refunds"

// envWithoutRazorpayKeys returns the environment with the Razorpay secrets
// removed, so a child is only ever given credentials explicitly.
func envWithoutRazorpayKeys() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "RAZORPAY_KEY_ID=") ||
			strings.HasPrefix(kv, "RAZORPAY_KEY_SECRET=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "rzp-guard: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		mandatePath = flag.String("mandate", "", "path to the merchant-issued mandate (required)")
		statePath   = flag.String("state", "rzp-guard.db", "durable state file")
		toolsets    = flag.String("toolsets", DefaultToolsets, "toolsets enabled on the child")
		childTee    = flag.String("child-tee", "", "record every byte written to child stdin (evidence)")
		decisionLog = flag.String("decision-log", "", "append-only JSONL decision log")
	)
	flag.Parse()

	if *mandatePath == "" {
		return fmt.Errorf("-mandate is required")
	}
	keyID, keySecret := os.Getenv("RAZORPAY_KEY_ID"), os.Getenv("RAZORPAY_KEY_SECRET")
	if keyID == "" || keySecret == "" {
		return fmt.Errorf("RAZORPAY_KEY_ID and RAZORPAY_KEY_SECRET must be set " +
			"(test-mode keys only; never production)")
	}
	if !strings.HasPrefix(keyID, "rzp_test") {
		// A live key here would move real money through a component whose
		// success path is still unverified. Refuse rather than warn.
		return fmt.Errorf("RAZORPAY_KEY_ID is not a test-mode key (expected rzp_test prefix); " +
			"rzp-guard refuses to run against live credentials")
	}

	raw, err := os.ReadFile(*mandatePath)
	if err != nil {
		return fmt.Errorf("read mandate: %w", err)
	}
	m, err := mandate.Load(raw)
	if err != nil {
		return err
	}

	boot, err := bootstrap.Open(*statePath, m, time.Now().UTC())
	if err != nil {
		return err
	}
	defer boot.Close()
	if len(boot.RecoveredInDoubt) > 0 {
		fmt.Fprintf(os.Stderr,
			"rzp-guard: recovered %d in-flight reservation(s) from a previous run as "+
				"IN_DOUBT; operator resolution required: %v\n",
			len(boot.RecoveredInDoubt), boot.RecoveredInDoubt)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	child, err := newChild(ctx, *toolsets, keyID, keySecret)
	if err != nil {
		return err
	}
	child.Stderr = os.Stderr

	childIn, err := child.StdinPipe()
	if err != nil {
		return fmt.Errorf("child stdin: %w", err)
	}
	childOut, err := child.StdoutPipe()
	if err != nil {
		return fmt.Errorf("child stdout: %w", err)
	}

	// Every byte the relay writes toward the child, recorded verbatim. This is
	// the evidence that a blocked call never crossed the boundary.
	var childWriter io.Writer = childIn
	if *childTee != "" {
		tee, err := os.Create(*childTee)
		if err != nil {
			return fmt.Errorf("child tee: %w", err)
		}
		defer tee.Close()
		childWriter = io.MultiWriter(childIn, tee)
	}

	sink, closeSink, err := decisionSink(*decisionLog)
	if err != nil {
		return err
	}
	defer closeSink()

	r := relay.New(boot.Guard, childWriter, os.Stdout, sink)

	if err := child.Start(); err != nil {
		return fmt.Errorf("start child (is Docker running?): %w", err)
	}

	// Cleanup runs exactly once, on whichever exit route fires first.
	var once sync.Once
	cleanup := func(reason string) {
		once.Do(func() {
			stranded := r.CloseInflight()
			if len(stranded) > 0 {
				fmt.Fprintf(os.Stderr,
					"rzp-guard: %s with %d unresolved refund(s); marked IN_DOUBT and "+
						"held for operator resolution: %v\n",
					reason, len(stranded), stranded)
			}
			_ = childIn.Close()
		})
	}
	defer cleanup("shutdown")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Child stdout EOF means the server is gone: nothing further can answer
		// an outstanding refund.
		if err := r.PumpChild(childOut); err != nil {
			fmt.Fprintf(os.Stderr, "rzp-guard: child pump: %v\n", err)
		}
		cleanup("child stdout closed")
	}()

	go func() {
		<-ctx.Done()
		cleanup("interrupted")
	}()

	// Agent stdin EOF ends the session.
	if err := r.PumpAgent(os.Stdin); err != nil {
		fmt.Fprintf(os.Stderr, "rzp-guard: agent pump: %v\n", err)
	}
	cleanup("agent stdin closed")

	wg.Wait()
	// Child exit is the last route; the deferred cleanup already ran, so this
	// only reaps the process.
	_ = child.Wait()
	return nil
}

// decisionSink appends one JSON object per decision. Attacker-controlled text
// is never written raw: only the rule, the matched action and the guard's own
// reason string go to disk.
func decisionSink(path string) (relay.DecisionSink, func(), error) {
	if path == "" {
		return nil, func() {}, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, func() {}, fmt.Errorf("decision log: %w", err)
	}
	var mu sync.Mutex
	enc := json.NewEncoder(f)
	sink := func(d policy.Decision, id json.RawMessage) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(map[string]any{
			"at":                time.Now().UTC().Format(time.RFC3339Nano),
			"jsonrpc_id":        string(id),
			"tool":              d.Tool,
			"allowed":           d.Allowed,
			"rule":              d.Rule,
			"reason":            d.Reason,
			"matched_action_id": d.MatchedActionID,
			"receipt":           d.Receipt,
			"authorized_paise":  d.AuthorizedPaise,
		})
	}
	return sink, func() { _ = f.Close() }, nil
}
