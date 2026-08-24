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
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/harshith/rzp-guard/internal/lifecycle"
	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/storage"
)

const usage = `rzp-guard-operator — resolve refunds whose outcome is unknown

  rzp-guard-operator -mandate M -state S list
        show every action locked IN_DOUBT, with the receipt to look up

  rzp-guard-operator -mandate M -state S audit
        show every operator resolution already recorded

  rzp-guard-operator -mandate M -state S resolve <action_id> \
        -outcome landed|not-landed -operator <who> -reason <text>
        record a human's finding and unlock the action

The guard must be STOPPED: it holds an exclusive lock on the state file.
Set RZP_GUARD_OPERATOR_TOKEN (>= 16 chars) to authorise a resolve.

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
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }

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

	switch args[0] {
	case "list":
		return cmdList(store, m, *asJSON)
	case "audit":
		return cmdAudit(store, *asJSON)
	case "resolve":
		if len(args) < 2 {
			return errors.New("resolve needs an action_id")
		}
		return cmdResolve(store, m, args[1], *outcome, *operator, *reason)
	default:
		flag.Usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
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

func cmdResolve(store *storage.Store, m *mandate.Mandate,
	actionID, outcome, operator, reason string) error {
	token := os.Getenv("RZP_GUARD_OPERATOR_TOKEN")
	if token == "" {
		return errors.New("RZP_GUARD_OPERATOR_TOKEN is not set")
	}

	// Compare against the hash the GUARD stored at launch. An earlier version
	// built the console with the same token it then checked, so the comparison
	// was against itself and any sufficiently long token was accepted -- caught
	// by its own end-to-end test, which resolved an action with a wrong token.
	stored, configured, err := store.OperatorTokenHash()
	if err != nil {
		return err
	}
	if !configured {
		return errors.New("no operator token is configured in this state file. " +
			"Start the guard once with RZP_GUARD_OPERATOR_TOKEN set, so the expected " +
			"value is recorded from a trusted source rather than from this command")
	}
	sum := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(stored)) != 1 {
		return errors.New("operator token rejected")
	}
	var landed bool
	switch outcome {
	case "landed":
		landed = true
	case "not-landed":
		landed = false
	default:
		return errors.New(`-outcome must be "landed" or "not-landed"`)
	}
	if strings.TrimSpace(operator) == "" || strings.TrimSpace(reason) == "" {
		return errors.New("-operator and -reason are required: an unaudited resolution " +
			"of a possibly-completed refund is not an acceptable operation")
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
	console, err := lifecycle.NewConsole(led, token, store)
	if err != nil {
		return err
	}
	if err := console.Resolve(token, operator, actionID, landed, reason); err != nil {
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
	fmt.Printf("  Audited at %s by %s\n", time.Now().UTC().Format(time.RFC3339), operator)
	return nil
}
