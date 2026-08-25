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
// OPERATIONAL CONSTRAINT, stated plainly: the guard holds an EXCLUSIVE lock on
// the state file for its lifetime, which is what prevents two guards from each
// enforcing the cumulative cap against their own in-memory ledger. So this tool
// requires the guard to be stopped. The workflow is: stop the guard, resolve,
// restart. That is a real limitation, not a hidden one, and it is reported
// clearly rather than surfacing as a confusing lock error.
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

The guard must be STOPPED: it holds an exclusive lock on the state file.
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

	store, err := storage.Open(*statePath, m.MandateID)
	if err != nil {
		return fmt.Errorf("could not take the state file — is the guard still "+
			"running? It holds an exclusive lock for its lifetime, so stop it "+
			"before resolving.\n  underlying: %w", err)
	}
	defer store.Close()

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
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// authenticate verifies a presented token and returns an unforgeable Grant.
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
	if len(rows) == 0 {
		fmt.Println("No actions are IN_DOUBT. Nothing to resolve.")
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
	return nil
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

	// Rebuild the ledger from durable state and go through the SAME Console the
	// unit tests exercise, so the token check, the state-machine guard and the
	// atomic audit write are the tested code paths rather than a second
	// implementation.
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
