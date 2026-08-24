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
	"crypto/sha256"
	"encoding/hex"
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

// drainGrace is how long an ending session waits for replies to calls already
// forwarded, before locking whatever is still outstanding.
const drainGrace = 10 * time.Second

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

	// Configure operator auth from the guard's own environment. This is the
	// TRUSTED source; the operator CLI later presents a token that must hash to
	// this value. Without it, resolution is impossible rather than unauthenticated.
	if tok := os.Getenv("RZP_GUARD_OPERATOR_TOKEN"); tok != "" {
		if len(tok) < 16 {
			return fmt.Errorf("RZP_GUARD_OPERATOR_TOKEN must be at least 16 characters")
		}
		sum := sha256.Sum256([]byte(tok))
		if err := boot.Store.SetOperatorTokenHash(hex.EncodeToString(sum[:])); err != nil {
			return err
		}
	}

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

	// finalize cancels the child and locks any unresolved refund. It runs at
	// most once, on whichever boundary fires first.
	var once sync.Once
	finalize := func(reason string) {
		once.Do(func() {
			// Cancel on EVERY route, not via a deferred cancel that would only
			// run after the reap timeout: a child ignoring closed stdin would
			// otherwise outlive the session.
			stop()
			stranded := r.CloseInflight()
			if len(stranded) > 0 {
				fmt.Fprintf(os.Stderr,
					"rzp-guard: %s with %d unresolved refund(s); marked IN_DOUBT and "+
						"held for operator resolution: %v\n",
					reason, len(stranded), stranded)
			}
		})
	}
	defer finalize("shutdown")

	// Reap in the background from the start, so the child's TRUE exit status is
	// captured before finalize cancels its context. Reading it afterwards would
	// report our own cancellation as a child failure -- which it did: a stub
	// child that exited cleanly was surfaced as "signal: killed".
	waitErr := make(chan error, 1)
	go func() { waitErr <- child.Wait() }()

	agentDone := make(chan error, 1)
	go func() { agentDone <- r.PumpAgent(os.Stdin) }()

	childDone := make(chan error, 1)
	go func() { childDone <- r.PumpChild(childOut) }()

	var (
		parentInitiated bool
		childExit       error
		haveChildExit   bool
	)

	select {
	case err := <-agentDone:
		// The agent has no more REQUESTS, but replies to already-forwarded calls
		// may still be arriving. Closing the child's stdin says "no more work";
		// killing it here would discard an in-flight refund result and turn a
		// resolvable action into IN_DOUBT for nothing. Drain, then finalize.
		parentInitiated = true
		if err != nil {
			fmt.Fprintf(os.Stderr, "rzp-guard: agent pump: %v\n", err)
		}
		_ = childIn.Close()
		select {
		case <-childDone:
		case <-time.After(drainGrace):
			fmt.Fprintf(os.Stderr,
				"rzp-guard: child still had work in flight after %s; locking it\n", drainGrace)
		}
		finalize("agent stdin closed")

	case err := <-childDone:
		if err != nil {
			fmt.Fprintf(os.Stderr, "rzp-guard: child pump: %v\n", err)
		}
		// Capture the real exit status BEFORE cancelling.
		select {
		case childExit = <-waitErr:
			haveChildExit = true
		case <-time.After(2 * time.Second):
		}
		finalize("child stdout closed")

	case <-ctx.Done():
		parentInitiated = true
		finalize("interrupted")
	}

	if !haveChildExit {
		select {
		case childExit = <-waitErr:
			haveChildExit = true
		case <-time.After(5 * time.Second):
			fmt.Fprintln(os.Stderr, "rzp-guard: child did not exit within 5s; leaving anyway")
		}
	}
	// A child that failed on its own -- a crashed container, rejected
	// credentials -- must not be reported as a clean run. Discarding this error
	// let the CLI exit zero after the pinned container refused to start.
	if haveChildExit && childExit != nil && !parentInitiated {
		return fmt.Errorf("child MCP server exited: %w", childExit)
	}
	return nil
}

// decisionSink appends one JSON object per decision.
//
// HONEST NOTE ABOUT CONTENT: the guard's `reason` string embeds agent-supplied
// values -- "no authorized refund action exists for pay_SYN99999999999" quotes
// the payment id straight from the request. An earlier comment here claimed
// attacker-controlled text was never written, which was simply false.
//
// JSON encoding prevents syntax injection into the log, but it does not make
// the content trustworthy. Free-text fields an attacker fully controls (notes,
// description, customer_name) are NOT logged at all; identifiers that appear in
// a reason are truncated below. Anything rendering this log must still escape
// it -- treat every string here as hostile.
// clip bounds any field that can carry agent-supplied content, so a hostile
// argument cannot inflate the log without bound.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

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
			"jsonrpc_id":        clip(string(id), 64),
			"tool":              clip(d.Tool, 64),
			"allowed":           d.Allowed,
			"rule":              d.Rule,
			"reason":            clip(d.Reason, 512),
			"matched_action_id": d.MatchedActionID,
			"receipt":           d.Receipt,
			"authorized_paise":  d.AuthorizedPaise,
		})
	}
	return sink, func() { _ = f.Close() }, nil
}
