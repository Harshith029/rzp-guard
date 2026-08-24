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

	child, err := newChild(ctx, keyID, keySecret)
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

	// Each pump runs in its own goroutine and reports why it ended. The
	// supervisor below returns on whichever boundary fires FIRST.
	//
	// A previous revision ran PumpAgent on the main goroutine. Cleanup fired
	// correctly on child death or SIGTERM, but main stayed blocked reading agent
	// stdin, so a dead child with an open agent stream left a hung proxy --
	// measured at 25s with a 25s-held stdin, and it would have been unbounded
	// with a real client. State cleanup beginning is not the same as the process
	// lifecycle being controlled.
	agentDone := make(chan error, 1)
	go func() { agentDone <- r.PumpAgent(os.Stdin) }()

	childDone := make(chan error, 1)
	go func() { childDone <- r.PumpChild(childOut) }()

	// parentInitiated records that WE ended the session, so a child exit that
	// follows from our own shutdown is not reported as a child failure.
	parentInitiated := false

	select {
	case err := <-agentDone:
		parentInitiated = true
		if err != nil {
			fmt.Fprintf(os.Stderr, "rzp-guard: agent pump: %v\n", err)
		}
		cleanup("agent stdin closed")
	case err := <-childDone:
		if err != nil {
			fmt.Fprintf(os.Stderr, "rzp-guard: child pump: %v\n", err)
		}
		cleanup("child stdout closed")
	case <-ctx.Done():
		parentInitiated = true
		cleanup("interrupted")
	}

	// Reap the child, but never block on it: after a signal or a dead child the
	// process must leave promptly whether or not docker is still winding down.
	waitErr := make(chan error, 1)
	go func() { waitErr <- child.Wait() }()
	select {
	case err := <-waitErr:
		// A child that failed on its own -- a crashed container, rejected
		// credentials -- must not be reported as a clean run. Discarding this
		// error let the CLI exit zero after the pinned container refused to
		// start, which only run.sh's verifier would have caught.
		if err != nil && !parentInitiated {
			return fmt.Errorf("child MCP server exited: %w", err)
		}
	case <-time.After(5 * time.Second):
		fmt.Fprintln(os.Stderr, "rzp-guard: child did not exit within 5s; leaving anyway")
	}
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
