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
	"github.com/harshith/rzp-guard/internal/buildinfo"
	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/mandateauth"
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

// alertToken prefixes every line reporting money in an unknown state.
//
// It is a fixed, greppable string on purpose. This project has no metrics
// endpoint and no alerting integration, so the realistic operational path is a
// log rule -- and a log rule needs one token that appears on every such event
// and on nothing else. Both routes to IN_DOUBT emit it: a transition during the
// session, and reservations recovered from a previous process at startup.
//
// Without this an IN_DOUBT refund was discoverable only by someone choosing to
// run `rzp-guard-operator list`. Money whose fate is unknown should not wait on
// somebody's curiosity.
const alertToken = "RZP_GUARD_ALERT"

func run() error {
	var (
		mandatePath = flag.String("mandate", "", "path to the merchant-issued mandate (required)")
		statePath   = flag.String("state", "rzp-guard.db", "durable state file")
		childTee    = flag.String("child-tee", "", "record every byte written to child stdin (evidence)")
		decisionLog = flag.String("decision-log", "", "append-only JSONL decision log")
		statusFile  = flag.String("status-file", "", "publish a lock-free status document here (see status.go)")
		statusEvery = flag.Duration("status-interval", 5*time.Second, "how often to refresh -status-file")
		showVersion = flag.Bool("version", false, "print build identity and exit")
		mandateKey  = flag.String("mandate-pubkey", "", "hex ed25519 public key of the "+
			"merchant that issues mandates. When set, the mandate MUST carry a valid "+
			"detached signature at <mandate>.sig or the guard refuses to start. When "+
			"unset, the mandate is trusted as-is and a warning says so.")
	)
	flag.Parse()

	// Answered before every other check, including the credential ones: asking a
	// binary what it is must not require a working environment. During an
	// incident this is the first question and it should never fail.
	if *showVersion {
		fmt.Println(buildinfo.String("rzp-guard"))
		fmt.Printf("  child:    %s\n  toolsets: %s\n", PinnedImage, Toolsets)
		return nil
	}

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

	// Establish that the merchant issued this authority BEFORE parsing it. The
	// guard's other checks all assume the mandate is genuine; nothing until now
	// established that. Verification covers the exact bytes on disk, so no
	// re-serialisation sits between what was signed and what is enforced.
	pub, err := mandateauth.LoadPublicKey(*mandateKey)
	if err != nil {
		return err
	}
	auth, err := mandateauth.Verify(*mandatePath, raw, pub)
	if err != nil {
		return fmt.Errorf("mandate authenticity: %w", err)
	}
	if auth.Verified {
		fmt.Fprintf(os.Stderr, "rzp-guard: mandate signature verified (key %s)\n", auth.KeyID)
	} else {
		fmt.Fprintln(os.Stderr, "rzp-guard: WARNING: "+auth.Warning)
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

	// The guard deliberately has NO path that writes the operator credential.
	// It used to record RZP_GUARD_OPERATOR_TOKEN on every start, which meant
	// anyone able to relaunch the process could install their own token and then
	// resolve locked refunds without knowing the real one. Credential setup and
	// rotation live in rzp-guard-operator, and rotation requires the current
	// token.

	// Refuse to run against a state file with no recovery credential.
	//
	// Otherwise the guard silently CREATES an unprovisioned state file, and
	// whoever runs init first afterwards becomes the recovery authority --
	// authority established by a race rather than by deployment. Provisioning is
	// an explicit step: run the operator init command once, before first start.
	_, configured, ephemeral, err := boot.Store.OperatorVerifier()
	if err != nil {
		return err
	}
	if ephemeral {
		// A fixture credential satisfies "configured" while being unrecoverable
		// by anyone. Accepting it would mean an allowed refund could land in
		// IN_DOUBT with no possible operator resolution -- the guard's own
		// recovery guarantee, defeated by its own test setup.
		return fmt.Errorf("state file %q was provisioned by a TEST FIXTURE whose "+
			"operator token was discarded; no human can resolve an IN_DOUBT refund "+
			"in it. Refusing to run. Provision a real credential with "+
			"rzp-guard-operator init", *statePath)
	}
	if !configured {
		return fmt.Errorf("state file %q has no operator recovery credential; run "+
			"rzp-guard-operator init first, as a deployment step. The guard will not "+
			"establish that authority itself", *statePath)
	}

	if *statusFile != "" {
		sw := newStatusWriter(*statusFile, boot.Guard, m.MandateID)
		stopStatus := make(chan struct{})
		go sw.run(stopStatus, *statusEvery)
		// Registered BEFORE `defer finalize` below. Defers run LIFO, so finalize
		// runs first and this closes afterwards -- meaning the last document on
		// disk includes whatever finalize just marked IN_DOUBT. Registered the
		// other way round, the final status would omit exactly the refunds an
		// operator most needs to find.
		defer close(stopStatus)
	}

	// Unresolved work belonging to OTHER mandates in this state file.
	//
	// The storage layer used to REFUSE to open a file whose previous mandate had
	// unresolved actions, because every query is scoped by mandate and opening
	// under a new one hid them permanently. A file may now hold several mandates
	// on purpose -- that is what lets ten merchants share one queue, one operator
	// credential and one alert sink -- so the guarantee moved from refusing to
	// reporting. It is reported at every start, not once, which is strictly more
	// than the refusal ever did.
	for mid, ids := range boot.StrandedElsewhere {
		fmt.Fprintf(os.Stderr,
			"%s OTHER_MANDATE_UNRESOLVED mandate=%s actions=%v reason=%q\n",
			alertToken, mid, ids,
			"another mandate in this state file has refunds awaiting an operator")
	}

	if len(boot.RecoveredInDoubt) > 0 {
		for _, id := range boot.RecoveredInDoubt {
			fmt.Fprintf(os.Stderr,
				"%s IN_DOUBT action=%s reason=%q\n", alertToken, id,
				"in flight when the previous process died; the refund may have landed")
		}
		fmt.Fprintf(os.Stderr,
			"rzp-guard: recovered %d in-flight reservation(s) from a previous run as "+
				"IN_DOUBT; operator resolution required: %v\n",
			len(boot.RecoveredInDoubt), boot.RecoveredInDoubt)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Losing the mandate lease mid-session means another process now believes it
	// owns this mandate's ledger. Two ledgers over one mandate is the condition
	// the lease exists to prevent, and the only correct response is to stop
	// forwarding: this process's in-memory view of what is consumed is no longer
	// authoritative, so every further decision it makes could double-spend.
	//
	// Cancelling the context is what stops it. The child is torn down and the
	// ordinary shutdown path runs, which marks anything in flight IN_DOUBT --
	// the conservative direction, and the right one, because a refund forwarded
	// under a lease this process no longer held is exactly the ambiguous case.
	boot.OnLeaseLost(func(err error) {
		fmt.Fprintf(os.Stderr,
			"%s LEASE_LOST reason=%q\n", alertToken, err.Error())
		fmt.Fprintln(os.Stderr, "rzp-guard: another process holds this mandate's "+
			"lease; refusing to keep forwarding against a ledger that is no longer ours")
		stop()
	})

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
	var alertMu sync.Mutex

	var childWriter io.Writer = childIn
	if *childTee != "" {
		// 0600, not os.Create's 0666. This file records every byte sent toward
		// the provider -- payment ids, amounts, receipts -- and a world-readable
		// copy of that on a shared host is the kind of evidence file that
		// becomes the incident.
		tee, err := os.OpenFile(*childTee, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return fmt.Errorf("child tee: %w", err)
		}
		defer tee.Close()
		// NOT io.MultiWriter: it returns the byte count of the writer that
		// failed, so a failing tee after a successful child write reports
		// zero, which the relay reads as "nothing was dispatched" and
		// releases the authorization on. See internal/relay/child_tee.go.
		//
		// A broken audit copy goes to the SAME channel an operator watches for
		// IN_DOUBT, under its own event name. It is not an action transition --
		// no money is stuck and nothing needs resolving -- but the guard has
		// stopped being able to prove what crossed the boundary, and that is
		// not a thing to leave in an unread stderr line.
		childWriter = relay.NewChildTee(childIn, tee, func(err error) {
			alertMu.Lock()
			defer alertMu.Unlock()
			fmt.Fprintf(os.Stderr, "%s AUDIT_BROKEN file=%q reason=%q\n",
				alertToken, *childTee, err.Error())
		})
	}

	sink, closeSink, err := decisionSink(*decisionLog)
	if err != nil {
		return err
	}
	defer closeSink()

	r := relay.New(boot.Guard, childWriter, os.Stdout, sink)

	// Every mid-session IN_DOUBT transition, on stderr, one line each.
	// Deliberately NOT the decision log: that records authorization decisions,
	// and this is an outcome. Conflating them would bury the event that needs a
	// human among thousands that do not.
	r.SetAlerter(func(actionID, reason string) {
		alertMu.Lock()
		defer alertMu.Unlock()
		fmt.Fprintf(os.Stderr, "%s IN_DOUBT action=%s reason=%q\n",
			alertToken, actionID, reason)
	})

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
			// The child finished during the drain window. Nothing has cancelled
			// it yet, so a non-zero status here is a GENUINE child failure --
			// rejected credentials, a crash -- and must not be written off as a
			// clean parent-initiated shutdown just because the agent went first.
			select {
			case childExit = <-waitErr:
				haveChildExit = true
				if childExit != nil {
					parentInitiated = false
				}
			case <-time.After(2 * time.Second):
			}
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
			// The FULL set. One refund may consume several actions, and a forensic
			// record naming only the first would understate what a call spent.
			// matched_action_id stays the first of them so existing readers of this
			// log keep working.
			"matched_action_ids": d.MatchedActionIDs,
			"receipt":            d.Receipt,
			"authorized_paise":   d.AuthorizedPaise,
		})
	}
	return sink, func() { _ = f.Close() }, nil
}
