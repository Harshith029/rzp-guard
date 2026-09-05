package relay

import (
	"fmt"
	"sync"
	"time"
)

// THE HUNG CHILD, which was the one failure mode with no bounded outcome.
//
// FAILURES table, "Hung child, session open": *waits indefinitely. Action stays
// RESERVED, budget held. Recovery: restart, or spot it in operator list.* Every
// other failure in that table resolves itself into a state a human can act on;
// this one resolved into waiting. The requirement was written down as F11 and
// declined, because a timeout needs a defensible value and nobody had one.
//
// WHY IT IS DEFENSIBLE NOW, AND WHY THE DIRECTION MATTERS MORE THAN THE NUMBER.
//
// The governing rule of this design is RELEASE ONLY ON CONFIRMED PROVIDER
// REJECTION, and its corollary is that A TIMEOUT IS NOT EVIDENCE. Both survive
// here intact, because expiring a deadline does NOT release anything. It marks
// the actions IN_DOUBT and alerts, which is the same outcome the session
// already produces for a child that dies mid-flight -- reached sooner, and by a
// rule rather than by somebody noticing.
//
// So the value cannot cause a wrong release however badly it is chosen. Too
// short only converts a slow provider into an operator's question, which is
// exactly the trade this project makes everywhere else. That is what makes a
// number defensible without a production measurement: the cost of being wrong
// is bounded and lands on the conservative side.
//
// THE DEFAULT IS OFF, and that is deliberate too. Turning a deadline on by
// default would change the behaviour of every existing deployment and every
// piece of committed evidence, on a value nobody has measured against a real
// Razorpay latency distribution. -refund-timeout is an opt-in with a documented
// starting point, and OPERATIONS.md says what to watch to choose it.

// deadlines tracks when each in-flight refund must be answered by.
//
// Kept beside the inflight map rather than inside pending, because the sweeper
// needs to iterate by time and the relay's hot path needs to look up by id.
// Merging them would put a timestamp on every read's pending entry too, for a
// sweep that only ever cares about refunds.
type deadlines struct {
	mu  sync.Mutex
	due map[string]time.Time
}

func newDeadlines() *deadlines {
	return &deadlines{due: map[string]time.Time{}}
}

func (d *deadlines) set(id string, at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.due[id] = at
}

func (d *deadlines) clear(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.due, id)
}

// expired returns the ids whose deadline has passed, and forgets them.
func (d *deadlines) expired(now time.Time) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for id, at := range d.due {
		if !now.Before(at) {
			out = append(out, id)
			delete(d.due, id)
		}
	}
	return out
}

// SetRefundDeadline bounds how long a forwarded refund may go unanswered.
//
// Zero, the default, means no deadline and exactly the behaviour this relay has
// always had. Set it before any traffic.
func (r *Relay) SetRefundDeadline(d time.Duration) {
	if d <= 0 {
		return
	}
	r.refundDeadline = d
	if r.deadlines == nil {
		r.deadlines = newDeadlines()
	}
}

// SweepDeadlines locks every refund whose deadline has passed.
//
// It is called on a timer by the guard rather than run from a goroutine this
// package owns, for the same reason the alerter is injected: this package
// decides WHAT happens to an overdue refund, and the binary decides when to
// look. A relay that started its own ticker would also need to stop it, and a
// sweeper still running after CloseInflight is a second writer to the ledger
// during shutdown.
//
// Returns the actions it locked, so a caller can log them.
func (r *Relay) SweepDeadlines(now time.Time) []string {
	if r.deadlines == nil {
		return nil
	}
	var locked []string
	for _, id := range r.deadlines.expired(now) {
		r.mu.Lock()
		p, ok := r.inflight[id]
		if ok {
			delete(r.inflight, id)
		}
		r.mu.Unlock()
		if !ok || !p.isRefund {
			continue
		}
		// IN_DOUBT, never released. The bytes reached the child, so the refund
		// may have executed; the deadline says only that we stopped waiting for
		// the answer, which is a statement about this process and not about
		// Razorpay.
		r.markInDoubt(p.actionIDs, fmt.Sprintf(
			"no reply within the %s refund deadline; the call was forwarded, so it "+
				"may have executed. A timeout is not evidence of rejection, so this "+
				"is locked for an operator rather than released", r.refundDeadline))
		locked = append(locked, p.actionIDs...)
	}
	return locked
}
