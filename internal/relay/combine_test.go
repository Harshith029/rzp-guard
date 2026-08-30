package relay

import (
	"strings"
	"testing"

	"github.com/harshith/rzp-guard/internal/lifecycle"
)

// One forwarded refund may consume several actions. Whatever happens to that
// call happens to ALL of them: a call that half-commits, or half-freezes,
// leaves the ledger disagreeing with itself about a refund that either happened
// or did not.

const combinable = `[
 {"action_id":"itm_a","payment_id":"pay_SYN0001","amount_paise":18500},
 {"action_id":"itm_b","payment_id":"pay_SYN0001","amount_paise":19000}]`

const combinedCall = `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":` +
	`{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":37500}}}`

func combinedRelay(t *testing.T) (*Relay, *childRecorder, *alertRecorder) {
	t.Helper()
	g := newGuard(t, combinable)
	r, child, _ := newRelay(t, g)
	rec := &alertRecorder{}
	r.SetAlerter(rec.fn)
	return r, child, rec
}

// The combined call must reach the child once, carrying the combination's
// receipt and the canonical total.
func TestACombinedRefundIsForwardedOnceWithOneReceipt(t *testing.T) {
	r, child, _ := combinedRelay(t)
	feed(t, r, combinedCall)

	lines := child.Lines()
	if len(lines) != 1 {
		t.Fatalf("child received %d lines, want exactly 1:\n%s", len(lines), child.String())
	}
	if !strings.Contains(lines[0], `"amount":37500`) {
		t.Fatalf("forwarded amount is not the canonical total:\n%s", lines[0])
	}
	if !strings.Contains(lines[0], "rzpg_") {
		t.Fatalf("no receipt injected:\n%s", lines[0])
	}
	for _, id := range []string{"itm_a", "itm_b"} {
		if st := r.guard.State(id); st != lifecycle.Reserved {
			t.Fatalf("%s is %s, want RESERVED", id, st)
		}
	}
}

// An ambiguous reply must freeze EVERY action the call consumed. Freezing only
// one would leave the other spendable against money that may already have moved.
func TestAnAmbiguousCombinedRefundFreezesEveryAction(t *testing.T) {
	r, _, rec := combinedRelay(t)
	feed(t, r, combinedCall)

	if err := r.PumpChild(strings.NewReader(
		`{"jsonrpc":"2.0","id":7,"error":{"code":-32000,"message":"upstream"}}` + "\n")); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"itm_a", "itm_b"} {
		if st := r.guard.State(id); st != lifecycle.InDoubt {
			t.Fatalf("%s is %s after an ambiguous combined refund, want IN_DOUBT; "+
				"an action left AVAILABLE here could be spent again against a "+
				"refund that may already have executed", id, st)
		}
	}
	// And the operator must be told about BOTH, or they resolve one and leave
	// the other held indefinitely.
	if rec.count() != 2 {
		t.Fatalf("got %d alerts for a 2-action refund, want 2: %v", rec.count(), rec.got)
	}
	seen := map[string]bool{}
	for _, id := range rec.ids {
		seen[id] = true
	}
	if !seen["itm_a"] || !seen["itm_b"] {
		t.Fatalf("alerts named %v, want both actions", rec.ids)
	}
}

// A matching reply must settle every action together.
func TestACombinedRefundCommitsEveryAction(t *testing.T) {
	r, _, rec := combinedRelay(t)
	feed(t, r, combinedCall)

	rcpt := inflightReceipt(t, r, "7")
	reply := `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":` +
		`"{\"entity\":\"refund\",\"id\":\"rfnd_ok\",\"payment_id\":\"pay_SYN0001\",` +
		`\"amount\":37500,\"receipt\":\"` + rcpt + `\"}"}]}}`
	if err := r.PumpChild(strings.NewReader(reply + "\n")); err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"itm_a", "itm_b"} {
		if st := r.guard.State(id); st != lifecycle.Committed {
			t.Fatalf("%s is %s after a matching reply, want COMMITTED", id, st)
		}
	}
	if rec.count() != 0 {
		t.Fatalf("a successful combined refund raised %d alert(s): %v", rec.count(), rec.got)
	}
}

// Session end with a combined call outstanding: every action is stranded and
// reported, not just the first.
func TestCloseInflightStrandsEveryActionOfACombinedCall(t *testing.T) {
	r, _, rec := combinedRelay(t)
	feed(t, r, combinedCall)

	stranded := r.CloseInflight()
	if len(stranded) != 2 {
		t.Fatalf("stranded %v, want both actions", stranded)
	}
	for _, id := range []string{"itm_a", "itm_b"} {
		if st := r.guard.State(id); st != lifecycle.InDoubt {
			t.Fatalf("%s is %s, want IN_DOUBT", id, st)
		}
	}
	if rec.count() != 2 {
		t.Fatalf("got %d alerts, want one per stranded action: %v", rec.count(), rec.got)
	}
}
