// Command rzp-guard-operator is the human half of the recovery path.
//
// When a refund's outcome is ambiguous — the child died mid-flight, or the
// provider returned an error that cannot prove the refund did not execute — the
// action and its budget are locked IN_DOUBT. Nothing automatic can clear that,
// by design: only a person who has looked at Razorpay and knows whether the
// money moved.
//
// Until this command existed, that resolver was a library object reachable only
// from a unit test, which meant the recovery path the whole design depends on
// had no operator-facing half. An ordinary provider error would have left a
// merchant with a permanently stuck authorization and no supported way to clear
// it.
//
// It is also the human half of the FALSE-POSITIVE path, which for a long time
// had no half at all. The guard refuses 45% of legitimate refunds by its own
// published measurement, and the cost model for that assumes somebody unblocks
// them. Nobody could: the guard held a file-wide exclusive lock for its entire
// lifetime, so every operator action -- including merely listing what was stuck
// -- required stopping the payment proxy first. Ownership is a per-mandate
// lease now, this tool attaches without taking one, and queue/approve/decline
// work while the guard runs.
//
// WHAT STILL NEEDS THE GUARD STOPPED: anything that moves state the guard holds
// in memory. It restores a ledger at startup and decides from it, so resolving
// an IN_DOUBT action underneath it would leave it authorizing against a view
// that is no longer true. Those commands say so with the holder's pid rather
// than surfacing as a confusing lock error.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/harshith/rzp-guard/internal/lifecycle"
	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/opauth"
	"github.com/harshith/rzp-guard/internal/storage"
)

const usage = `rzp-guard-operator — resolve refunds whose outcome is unknown

  rzp-guard-operator -mandate M -state S init -out NEW-FILE
        DEPLOYMENT STEP, run once BEFORE the guard is ever started.
        Refuses to print to a non-terminal. -out must NOT already exist and the
        token is written and fsynced BEFORE the credential is committed, so a
        failed delivery never locks recovery out. If delivery cannot be proven
        durable (terminal output, or a platform that cannot fsync a directory)
        the credential is NOT committed unless -accept-delivery-risk is given.

  rzp-guard-operator -mandate M -state S rotate -operator <who> -reason <text>
        replace it, authenticated with the CURRENT token, audited

  rzp-guard-operator -mandate M -state S list
        show every action locked IN_DOUBT, with the receipt to look up

  rzp-guard-operator -mandate M -state S audit
        show every operator resolution already recorded

  rzp-guard-operator -mandate M -state S resolve <action_id> \
        -outcome landed|not-landed -operator <who> -reason <text>
        record a human's finding and unlock the action

ALL commands except init require RZP_GUARD_OPERATOR_TOKEN and -operator,
including list and audit: they disclose payment ids, receipts, amounts and
audit reasons.

WHAT NEEDS THE GUARD STOPPED, AND WHAT DOES NOT.
  Runs beside a live guard:  list, audit, queue, approve, decline
  Needs the guard stopped:   resolve, rotate, init
The line is not read versus write. It is whether the command moves state the
guard is holding in its own memory: a live guard serves decisions from a ledger
it restored at startup, so resolving an action underneath it would leave it
authorizing against a view that is no longer true. Grants and denial
resolutions are read from the database at the moment they are needed, so they
are safe to write while it runs -- which is the whole point, because a refund
wrongly refused is only worth unblocking while someone is still waiting for it.

Set RZP_GUARD_OPERATOR_TOKEN to the generated token to authorise resolve/rotate.
The guard never reads or writes this credential; only this command does.

'landed' means you found the refund in Razorpay. 'not-landed' means you
confirmed it is absent AND that absence is trustworthy — a pending refund or a
failed lookup is NOT evidence of absence.
`

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "rzp-guard-operator: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		mandatePath = flag.String("mandate", "", "path to the mandate (required)")
		statePath   = flag.String("state", "rzp-guard.db", "durable state file")
		outcome     = flag.String("outcome", "", "landed | not-landed (resolve only)")
		operator    = flag.String("operator", "", "who is resolving (resolve only)")
		reason      = flag.String("reason", "", "what you checked and found (resolve only)")
		asJSON      = flag.Bool("json", false, "machine-readable output")
		out         = flag.String("out", "", "init/rotate: NEW file to write the token to (must not exist)")
		allMandates = flag.Bool("all", false, "queue: every mandate in this state file, not just this one")
		queueState  = flag.String("state-filter", "OPEN", "queue: OPEN | APPROVED | DECLINED, or empty for all")
		ttl         = flag.String("ttl", "", "approve: how long the grant lives (default 15m, ceiling 1h)")
	)
	allowUnprot := allowUnprotectedFlag()
	acceptRisk := acceptRiskFlag()
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage+unprotectedHelp+ephemeralHelp+riskHelp) }

	// Go's flag package stops parsing at the first non-flag argument, so
	// "resolve rfa_x -outcome landed" would leave -outcome unparsed and silently
	// defaulted. Lift the command and its operand out of the argument list
	// first, then parse what remains as flags, so order does not matter.
	rest := make([]string, 0, len(os.Args))
	var positional []string
	for i := 1; i < len(os.Args); i++ {
		a := os.Args[i]
		if strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			// A flag of the form "-name value" consumes the next token.
			if !strings.Contains(a, "=") && i+1 < len(os.Args) &&
				!strings.HasPrefix(os.Args[i+1], "-") {
				i++
				rest = append(rest, os.Args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	if err := flag.CommandLine.Parse(rest); err != nil {
		return err
	}

	args := positional
	if len(args) == 0 {
		flag.Usage()
		return errors.New("no command given")
	}

	// Merchant-side key handling runs BEFORE the state file is opened and before
	// the operator credential is checked. Signing a mandate is not an operation on
	// the guard's durable state, and requiring the state file to do it would force
	// the signing key onto the guard host -- the one place it must not be.
	// See mandatesign.go.
	switch args[0] {
	case "mandate-keygen":
		return cmdMandateKeygen(args[1:])
	case "mandate-sign":
		return cmdMandateSign(args[1:])
	}

	if *mandatePath == "" {
		return errors.New("-mandate is required")
	}

	raw, err := os.ReadFile(*mandatePath)
	if err != nil {
		return fmt.Errorf("read mandate: %w", err)
	}
	m, err := mandate.Load(raw)
	if err != nil {
		return err
	}

	// ATTACH, do not lease.
	//
	// This tool used to call storage.Open, which took the same file-wide
	// exclusive lock the guard held for its entire lifetime -- so every operator
	// action, including merely listing what was stuck, required stopping the
	// guard first. A support desk will not stop a payment proxy to unstick one
	// refund, which is why the published false-positive cost model's "a human
	// unblocks it" assumption had nothing behind it.
	//
	// Attaching reads and writes the same file without taking a lease. What is
	// permitted while a guard is live is decided per command, from the lease
	// itself: reading is always safe under WAL, issuing an operator grant is safe
	// because the guard reads grants from the database rather than from memory,
	// and anything that mutates state a live guard holds in memory is refused
	// with an explanation rather than a lock error.
	store, err := storage.Attach(*statePath, m.MandateID)
	if err != nil {
		return fmt.Errorf("could not open the state file: %w", err)
	}
	defer store.Close()

	// WHICH COMMANDS MAY RUN WHILE A GUARD IS LIVE.
	//
	// The distinction is not read versus write. It is whether the command
	// mutates state the guard is holding in its own memory.
	//
	// A live guard restored an in-memory ledger at startup and serves every
	// decision from it. Moving an action's state underneath that -- resolving an
	// IN_DOUBT refund, say -- leaves the guard authorizing against a view of the
	// world that is no longer true, and it would not find out until its next
	// restart. So those commands still require the guard to be stopped, and now
	// they say so with the pid rather than with a lock error.
	//
	// Everything else may run concurrently. Reads are safe under WAL. Issuing an
	// operator grant is safe for a specific structural reason: the guard reads
	// grants from the DATABASE on the refusal path, not from a snapshot taken at
	// startup, so a grant written now is visible to a guard that started an hour
	// ago. That is what makes unblocking a wrongly-refused refund possible while
	// the refund is still worth unblocking.
	lease, _, err := store.LeaseFor(m.MandateID)
	if err != nil {
		return err
	}
	if lease.Live && mutatesGuardState(args[0]) {
		return fmt.Errorf(
			"%q changes state a running guard is holding in memory, so it needs the "+
				"guard stopped.\n  %s is held by pid %d on %s (last heartbeat %s ago).\n"+
				"  Stop it, run this, restart.\n"+
				"  Commands that DO work right now: list, audit, queue, approve, decline.",
			args[0], m.MandateID, lease.PID, lease.Host,
			time.Since(lease.Heartbeat).Truncate(time.Second))
	}

	// init is the only command that runs without a credential, because it is
	// the one that creates it.
	if args[0] == "init" {
		return cmdInit(store, *out, *allowUnprot, *acceptRisk)
	}
	if args[0] == "init-ephemeral" {
		return initEphemeral(store)
	}

	// EVERY other command authenticates, including the read-only ones. list and
	// audit disclose payment ids, receipts, amounts, operator names and audit
	// reasons -- recovery evidence, not public data. Gating only mutation left
	// all of it readable by any local caller.
	token := os.Getenv("RZP_GUARD_OPERATOR_TOKEN")
	if token == "" {
		return errors.New("RZP_GUARD_OPERATOR_TOKEN is not set")
	}
	grant, err := authenticate(store, *operator, token)
	if err != nil {
		return err
	}

	switch args[0] {
	case "rotate":
		return cmdRotate(store, grant, *reason, *out, *allowUnprot, *acceptRisk)
	case "list":
		return cmdList(store, m, *asJSON)
	case "audit":
		return cmdAudit(store, *asJSON)
	case "resolve":
		if len(args) < 2 {
			return errors.New("resolve needs an action_id")
		}
		return cmdResolve(store, m, grant, args[1], *outcome, *reason)
	case "queue":
		return cmdQueue(store, *allMandates, *queueState, *asJSON)
	case "approve":
		if len(args) < 2 {
			return errors.New("approve needs a denial id; run `queue` for the ids")
		}
		d, terr := parseTTL(*ttl)
		if terr != nil {
			return terr
		}
		return cmdApprove(store, grant, args[1], *reason, d, lease)
	case "decline":
		if len(args) < 2 {
			return errors.New("decline needs a denial id; run `queue` for the ids")
		}
		return cmdDecline(store, grant, args[1], *reason)
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// authenticate verifies a presented token and returns an unforgeable Grant.
//
// THERE IS DELIBERATELY NO ATTEMPT COUNTER OR LOCKOUT HERE, and the reasoning
// is worth stating because its absence looks like an oversight.
//
// The KDF is the rate limit, measured rather than assumed
// (opauth.BenchmarkVerify): one verification costs 36.6 ms and allocates
// 64 MiB. That caps an attacker at ~27 guesses per second per core against a
// token of 256 bits from crypto/rand. Online guessing is not a threat model,
// it is arithmetic. The rejection path measures 35.0 ms -- the same within
// noise -- so the constant-time compare leaves no timing oracle either.
//
// A counter would also not defend the real threat. Reading the verifier
// requires the state file, and anyone holding the state file can attack it
// OFFLINE at their own pace; a counter stored in that same file is not a
// boundary against someone who already owns it. That is why README limit 6
// says the operator token is a second factor on top of filesystem ownership
// rather than an independent one.
//
// And it would cost something real. This CLI is the ONLY path to resolve an
// IN_DOUBT refund. A lockout on it means a mistyped token during an incident
// can deny the legitimate operator access to stuck money at exactly the moment
// they need it. Trading a genuine availability risk for no security gain is a
// bad trade, so it is not made.
//
// This changes if the credential ever becomes human-chosen. A passphrase would
// make guessing feasible and a counter necessary -- which is a reason to keep
// generating tokens rather than accepting them.
func authenticate(store *storage.Store, subject, token string) (opauth.Grant, error) {
	stored, configured, _, err := store.OperatorVerifier()
	if err != nil {
		return opauth.Grant{}, err
	}
	if !configured {
		return opauth.Grant{}, errors.New("no operator credential exists for this " +
			"state file. Run init once, as a deployment step, before the guard is started")
	}
	if subject == "" {
		return opauth.Grant{}, errors.New("-operator is required: every authenticated " +
			"action is attributed in the audit trail")
	}
	return opauth.Authenticate(subject, token, stored)
}

// cmdInit generates the credential. It refuses if one already exists: a second
// init must not be able to silently replace it, which is how the restart bypass
// worked when the guard rewrote the credential on every start.
func cmdInit(store *storage.Store, outPath string, allowUnprotected, acceptRisk bool) error {
	token, err := opauth.NewToken()
	if err != nil {
		return err
	}
	verifier, err := opauth.Verifier(token)
	if err != nil {
		return err
	}

	// FAIL CLOSED ON UNPROVABLE DELIVERY.
	//
	// A previous revision detected that delivery could not be made durable and
	// then committed the credential anyway, after printing a warning. Warning
	// after taking the unsafe action is not fail-closed: a power loss still
	// produces a state file with a recovery authority nobody can exercise, which
	// is the exact outcome this code exists to prevent.
	//
	// So: commit only when the token has been durably delivered, or when an
	// operator has explicitly accepted the risk with -accept-delivery-risk.
	if outPath == "" {
		// Terminal output cannot be proven delivered. A disconnect, an unreadable
		// scrollback, or a closed window after the commit leaves no token.
		if !acceptRisk {
			return errors.New("terminal delivery cannot be proven, so committing a " +
				"credential after printing one is not supported for deployment: a " +
				"disconnect or lost scrollback would leave recovery permanently " +
				"impossible. Use -out on a platform that can fsync a directory, or " +
				"pass -accept-delivery-risk to accept that outcome explicitly")
		}
		if !opauth.StdoutIsTerminal() {
			return errors.New("refusing to print a credential to a non-terminal")
		}
		fmt.Fprintln(os.Stderr, "rzp-guard-operator: UNSUPPORTED FOR DEPLOYMENT -- "+
			"committing a credential whose delivery cannot be proven.")
		if err := store.InitOperatorVerifier(verifier); err != nil {
			return err
		}
		fmt.Println("Operator credential created. Shown ONCE and not recoverable:")
		fmt.Printf("\n    %s\n\n", token)
		return nil
	}

	// Deliver first, commit second, and undo the delivery if the commit fails.
	durable, err := opauth.WriteTokenExclusive(outPath, token, allowUnprotected)
	if err != nil {
		return err
	}
	if !durable && !acceptRisk {
		os.Remove(outPath)
		return fmt.Errorf("this platform cannot fsync a directory, so the token "+
			"file's directory entry is not crash-durable and delivery cannot be "+
			"proven. REFUSING to commit the credential: a power loss here would "+
			"leave %s recoverable by nobody. Provision on a platform that supports "+
			"directory fsync, use an OS secret store, or pass -accept-delivery-risk "+
			"to accept that outcome explicitly", outPath)
	}
	if !durable {
		fmt.Fprintln(os.Stderr, "rzp-guard-operator: UNSUPPORTED FOR DEPLOYMENT -- "+
			"directory entry is not crash-durable on this platform.")
	}
	if err := store.InitOperatorVerifier(verifier); err != nil {
		os.Remove(outPath)
		return err
	}
	fmt.Printf("Operator credential created and written to %s.\n", outPath)
	fmt.Println("Move it somewhere a human can reach during an incident, then delete it.")
	return nil
}

// cmdRotate replaces the credential, authenticated by the CURRENT token.
func cmdRotate(store *storage.Store, grant opauth.Grant, reason, outPath string,
	allowUnprotected, acceptRisk bool) error {
	if err := checkAuditText(grant.Subject(), reason); err != nil {
		return err
	}
	next, err := opauth.NewToken()
	if err != nil {
		return err
	}
	verifier, err := opauth.Verifier(next)
	if err != nil {
		return err
	}

	// The same rule, and it matters more here: rotating on unprovable delivery
	// destroys a WORKING credential as well as failing to hand over the new one.
	if outPath == "" {
		if !acceptRisk {
			return errors.New("terminal delivery cannot be proven, so rotating would " +
				"risk invalidating a working credential while the replacement reaches " +
				"nobody. Use -out on a platform that can fsync a directory, or pass " +
				"-accept-delivery-risk to accept that outcome explicitly")
		}
		if !opauth.StdoutIsTerminal() {
			return errors.New("refusing to print a rotated credential to a non-terminal")
		}
		fmt.Fprintln(os.Stderr, "rzp-guard-operator: UNSUPPORTED FOR DEPLOYMENT -- "+
			"rotating with delivery that cannot be proven.")
		if err := store.RotateOperatorVerifier(verifier, grant.Subject(), reason); err != nil {
			return err
		}
		fmt.Println("Operator credential rotated and audited. New token, shown ONCE:")
		fmt.Printf("\n    %s\n\n", next)
		return nil
	}

	durable, err := opauth.WriteTokenExclusive(outPath, next, allowUnprotected)
	if err != nil {
		return err
	}
	if !durable && !acceptRisk {
		os.Remove(outPath)
		return errors.New("this platform cannot fsync a directory, so delivery of the " +
			"new token cannot be proven. REFUSING to rotate: a crash here would " +
			"invalidate the working credential while the replacement reached nobody")
	}
	if err := store.RotateOperatorVerifier(verifier, grant.Subject(), reason); err != nil {
		os.Remove(outPath)
		return err
	}
	fmt.Printf("Operator credential rotated and audited; new token written to %s.\n", outPath)
	fmt.Println("The previous token no longer authenticates.")
	return nil
}

// checkAuditText bounds the free text that reaches the durable audit trail.
//
// It arrives from a local CLI, but "local" is not "trusted": the audit trail is
// read by humans and may later be rendered. Control characters are refused and
// length is capped, so the record cannot be spoofed with embedded newlines or
// inflated without bound.
func checkAuditText(operator, reason string) error {
	if strings.TrimSpace(operator) == "" || strings.TrimSpace(reason) == "" {
		return errors.New("-operator and -reason are required: an unaudited resolution " +
			"of a possibly-completed refund is not an acceptable operation")
	}
	for label, v := range map[string]string{"-operator": operator, "-reason": reason} {
		if len(v) > 512 {
			return fmt.Errorf("%s is longer than 512 characters", label)
		}
		for _, r := range v {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf("%s contains a control character; the audit trail is "+
					"read by humans and must not be spoofable with embedded newlines", label)
			}
		}
	}
	return nil
}

// paymentFor resolves an action id back to its payment through the mandate,
// which is where that binding lives.
func paymentFor(m *mandate.Mandate, actionID string) string {
	for _, a := range m.AuthorizedRefundActions {
		if a.ActionID == actionID {
			return a.PaymentID
		}
	}
	return "(not in this mandate)"
}

func cmdList(store *storage.Store, m *mandate.Mandate, asJSON bool) error {
	rows, err := store.ActionsInState(string(lifecycle.InDoubt))
	if err != nil {
		return err
	}
	if asJSON {
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			out = append(out, map[string]any{
				"action_id": r.ActionID, "payment_id": paymentFor(m, r.ActionID),
				"receipt": r.Receipt, "amount_paise": r.AmountPaise,
				"state": r.State, "updated_at": r.UpdatedAt,
			})
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	// RESERVED is normally momentary: it lasts from the moment a refund is
	// authorized to the moment the child's reply settles it. An action still
	// sitting there means the reply never came, or the commit that should have
	// recorded it failed to persist -- budget encumbered against a refund that
	// may already have happened, and nothing to resolve because it is not
	// IN_DOUBT.
	//
	// It used to be invisible here. RecoverStartup does promote a stranded
	// RESERVED to IN_DOUBT, but only at the next restart, so on a long-running
	// guard the operator had no way to see one at all.
	held, herr := store.ActionsInState(string(lifecycle.Reserved))
	if herr != nil {
		return herr
	}

	if len(rows) == 0 && len(held) == 0 {
		fmt.Println("No actions are IN_DOUBT and none are held RESERVED. Nothing to resolve.")
		return nil
	}
	if len(rows) == 0 {
		fmt.Println("No actions are IN_DOUBT.")
		printHeld(held, m)
		return nil
	}
	fmt.Printf("%d action(s) locked IN_DOUBT — money may or may not have moved.\n\n", len(rows))
	for _, r := range rows {
		fmt.Printf("  action    %s\n", r.ActionID)
		fmt.Printf("  payment   %s\n", paymentFor(m, r.ActionID))
		fmt.Printf("  amount    %d paise\n", r.AmountPaise)
		fmt.Printf("  receipt   %s   <- search Razorpay for this\n", r.Receipt)
		fmt.Printf("  since     %s\n\n", r.UpdatedAt)
	}
	fmt.Println("Look each receipt up in the Razorpay dashboard, then:")
	fmt.Println("  rzp-guard-operator ... resolve <action_id> -outcome landed|not-landed \\")
	fmt.Println("      -operator you@merchant -reason \"what you saw\"")
	printHeld(held, m)
	return nil
}

// printHeld reports actions stuck in RESERVED.
//
// They cannot be resolved from here -- resolve accepts only IN_DOUBT -- so this
// says what the state means and what to do about it, rather than offering a
// command that would fail.
func printHeld(held []storage.ActionRow, m *mandate.Mandate) {
	if len(held) == 0 {
		return
	}
	fmt.Printf("\n%d action(s) held RESERVED. That state is normally momentary:\n", len(held))
	fmt.Println("the refund was authorized and the child's reply never settled it.")
	fmt.Println("Budget stays encumbered until something does.")
	fmt.Println()
	for _, r := range held {
		fmt.Printf("  action    %s\n", r.ActionID)
		fmt.Printf("  payment   %s\n", paymentFor(m, r.ActionID))
		fmt.Printf("  amount    %d paise\n", r.AmountPaise)
		fmt.Printf("  receipt   %s   <- search Razorpay for this\n", r.Receipt)
		fmt.Printf("  since     %s\n\n", r.UpdatedAt)
	}
	fmt.Println("If one has sat here longer than a few seconds the guard never")
	fmt.Println("recorded an outcome. Restarting it promotes these to IN_DOUBT,")
	fmt.Println("which is resolvable -- check the receipt in Razorpay first.")
}

func cmdAudit(store *storage.Store, asJSON bool) error {
	rows, err := store.AuditTrail()
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Println("No operator resolutions recorded.")
		return nil
	}
	for _, r := range rows {
		fmt.Printf("  %s  %s  %s  %s -> %s  refund_landed=%v\n      %s\n",
			r.At, r.Actor, r.ActionID, r.From, r.To, r.RefundLanded, r.Reason)
	}
	return nil
}

func cmdResolve(store *storage.Store, m *mandate.Mandate, grant opauth.Grant,
	actionID, outcome, reason string) error {
	var landed bool
	switch outcome {
	case "landed":
		landed = true
	case "not-landed":
		landed = false
	default:
		return errors.New(`-outcome must be "landed" or "not-landed"`)
	}
	if err := checkAuditText(grant.Subject(), reason); err != nil {
		return err
	}

	// Rebuild the ledger from durable state and go through the SAME
	// lifecycle.ResolveInDoubt the unit tests exercise, so the grant check, the
	// state-machine guard and the atomic audit write are the tested code paths
	// rather than a second implementation.
	snap, err := store.Snapshot()
	if err != nil {
		return err
	}
	led := lifecycle.NewLedger(m.Limits.MaxCumulativePaise, store)
	led.Restore(snap.States, snap.Amounts)

	if got := led.State(actionID); got != lifecycle.InDoubt {
		return fmt.Errorf("%s is %s, not IN_DOUBT — nothing to resolve", actionID, got)
	}
	if err := lifecycle.ResolveInDoubt(grant, led, store, actionID, landed, reason); err != nil {
		return err
	}

	final := led.State(actionID)
	fmt.Printf("%s resolved: IN_DOUBT -> %s\n", actionID, final)
	if landed {
		fmt.Printf("  Recorded as SPENT. %d paise stays counted against the cumulative cap.\n",
			snap.Amounts[actionID])
	} else {
		fmt.Printf("  Budget released and the action is available again.\n")
	}
	fmt.Printf("  Audited at %s by %s\n", time.Now().UTC().Format(time.RFC3339), grant.Subject())
	return nil
}

// mutatesGuardState reports whether a command changes state a running guard
// holds in memory.
//
// It is a DENY-BY-DEFAULT list: an unrecognised command is treated as mutating,
// so a new command added later needs a deliberate decision to be allowed to run
// beside a live guard. The alternative -- allow-by-default -- means the next
// command anyone adds is concurrent-safe by accident, and finds out otherwise
// during an incident.
func mutatesGuardState(cmd string) bool {
	switch cmd {
	case "list", "audit", "queue", "approve", "decline":
		// Reads, plus the two that write only to tables the guard never caches:
		// denial resolutions and operator grants. The guard consults both from
		// the database at the moment it needs them.
		return false
	default:
		// resolve, rotate, init, init-ephemeral, and anything added later.
		return true
	}
}
