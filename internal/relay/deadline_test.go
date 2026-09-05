package relay

import (
	"strings"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/lifecycle"
	"github.com/harshith/rzp-guard/internal/mandate"
)

// THE HUNG CHILD. Every other failure in this design resolves into a state a
// human can act on; this one resolved into waiting, with the action RESERVED
// and its budget held until somebody restarted the process or happened to run
// the operator list. It was written down as F11 and declined for want of a
// defensible value.
//
// The value is defensible because of the DIRECTION. Expiry never releases an
// authorization -- release only on confirmed provider rejection, and a timeout
// is not evidence -- so a badly chosen deadline can only turn a slow provider
// into an operator's question, never a double spend.

const refundCall = `{"jsonrpc":"2.0","id":11,"method":"tools/call","params":` +
	`{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":20000}}}`

func TestAnUnansweredRefundIsLockedWhenItsDeadlinePasses(t *testing.T) {
	var seen []string
	g := newGuard(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`)
	r, child, _ := newRelay(t, g)
	r.SetAlerter(func(id, reason string) { seen = append(seen, id+": "+reason) })
	r.SetRefundDeadline(30 * time.Second)

	feed(t, r, refundCall)
	if len(child.Lines()) != 1 {
		t.Fatalf("the refund was not forwarded (%d lines to the child)", len(child.Lines()))
	}
	if st := g.State("rfa_001"); st != lifecycle.Reserved {
		t.Fatalf("action is %s, want RESERVED before the deadline", st)
	}

	// Before the deadline, nothing happens. A sweeper that locked eagerly would
	// be worse than no sweeper: it would turn a merely slow provider into an
	// operator's question on every call.
	if locked := r.SweepDeadlines(now.Add(29 * time.Second)); len(locked) != 0 {
		t.Fatalf("swept %v before the deadline", locked)
	}
	if st := g.State("rfa_001"); st != lifecycle.Reserved {
		t.Fatalf("action is %s after an early sweep, want RESERVED", st)
	}

	locked := r.SweepDeadlines(now.Add(31 * time.Second))
	if len(locked) != 1 || locked[0] != "rfa_001" {
		t.Fatalf("swept %v, want the overdue action", locked)
	}

	// IN_DOUBT, NOT AVAILABLE. The bytes reached the child, so the refund may
	// have executed; the deadline is a statement about this process, not about
	// Razorpay. Releasing here would hand budget back for money that moved.
	if st := g.State("rfa_001"); st != lifecycle.InDoubt {
		t.Fatalf("action is %s after its deadline, want IN_DOUBT", st)
	}
	if len(seen) != 1 {
		t.Fatalf("%d alerts raised, want 1: money in an unknown state must not "+
			"wait on somebody's curiosity", len(seen))
	}
	if !strings.Contains(seen[0], "deadline") {
		t.Errorf("the alert does not say why: %q", seen[0])
	}
}

// A refund that WAS answered must not be swept afterwards. Forgetting to clear
// the deadline would mark a committed refund IN_DOUBT minutes after it settled
// -- an operator question about money already reconciled.
func TestAnAnsweredRefundIsNotSweptLater(t *testing.T) {
	g := newGuard(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`)
	r, _, _ := newRelay(t, g)
	r.SetRefundDeadline(10 * time.Second)

	feed(t, r, refundCall)
	receipt, _ := mandate.ReceiptFor("mnd_test", "rfa_001")
	if err := r.PumpChild(strings.NewReader(
		refundReply("11", "pay_SYN0001", 20000, receipt) + "\n")); err != nil {
		t.Fatal(err)
	}
	if st := g.State("rfa_001"); st != lifecycle.Committed {
		t.Fatalf("action is %s after a matching reply, want COMMITTED", st)
	}

	if locked := r.SweepDeadlines(now.Add(time.Hour)); len(locked) != 0 {
		t.Fatalf("swept %v an hour after the refund settled", locked)
	}
	if st := g.State("rfa_001"); st != lifecycle.Committed {
		t.Fatalf("a settled refund was moved to %s by the sweeper", st)
	}
}

// The default is OFF, and it has to stay that way: turning a deadline on by
// default would change the behaviour of every existing deployment and every
// piece of committed evidence, on a value nobody has measured against a real
// provider latency distribution.
func TestWithNoDeadlineSetNothingIsEverSwept(t *testing.T) {
	g := newGuard(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`)
	r, _, _ := newRelay(t, g)

	feed(t, r, refundCall)
	if locked := r.SweepDeadlines(now.Add(365 * 24 * time.Hour)); locked != nil {
		t.Fatalf("swept %v with no deadline configured", locked)
	}
	if st := g.State("rfa_001"); st != lifecycle.Reserved {
		t.Fatalf("action is %s, want RESERVED: unbounded waiting is the documented "+
			"default and this build must not change it silently", st)
	}
}

// A read must never be swept. Reads are tracked in the same map so one cannot
// reuse a refund's id and have its success commit the refund, but they hold no
// authorization and there is nothing to lock.
func TestReadsAreNeverSwept(t *testing.T) {
	g := newGuard(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`)
	r, _, _ := newRelay(t, g)
	r.SetRefundDeadline(time.Second)

	feed(t, r, `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":`+
		`{"name":"fetch_payment","arguments":{"payment_id":"pay_SYN0001"}}}`)
	if locked := r.SweepDeadlines(now.Add(time.Hour)); len(locked) != 0 {
		t.Fatalf("swept %v for a read, which holds no authorization", locked)
	}
}
