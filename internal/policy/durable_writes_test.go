package policy

import (
	"testing"

	"github.com/harshith/rzp-guard/internal/lifecycle"
)

// countingStore records how many DURABLE WRITES one authorized refund performs.
//
// It implements both Persister and RateReserver, so the ledger picks the
// combined path exactly as the real store does, and the counters say which one
// it actually took.
type countingStore struct {
	reserves  int // ReserveMany: a reservation with no rate slot
	combined  int // ReserveManyWithCall: reservation and rate slot, one commit
	rateCalls int // RecordCall: a separate rate-window commit
	states    int
}

func (c *countingStore) Reserve(actionID, receipt string, amountPaise int64) error {
	return c.ReserveMany(receipt, []lifecycle.Reservation{{ActionID: actionID, AmountPaise: amountPaise}})
}
func (c *countingStore) ReserveMany(string, []lifecycle.Reservation) error {
	c.reserves++
	return nil
}
func (c *countingStore) ReserveManyWithCall(string, []lifecycle.Reservation, int64) error {
	c.combined++
	return nil
}
func (c *countingStore) SetState(string, string, string) error { c.states++; return nil }
func (c *countingStore) SetStateMany([]string, string, string) error {
	c.states++
	return nil
}
func (c *countingStore) RecordCall(int64) error             { c.rateCalls++; return nil }
func (c *countingStore) RecentCalls(int64) ([]int64, error) { return nil, nil }

// ONE COMMIT PER AUTHORIZED REFUND.
//
// This is a performance property expressed as a correctness one, and it is
// worth pinning as a test rather than a benchmark because the failure is
// silent: splitting the two writes again would still pass every functional
// test, still be durable, and simply cost twice the fsync -- 4.9ms against
// 2.5ms measured on this hardware, which is the difference between roughly 200
// and roughly 400 authorized refunds per second per process.
//
// It is also the ordering property. Two commits can half-succeed, and the only
// repair available was a rollback the code itself described as best-effort. One
// commit removes the state instead of compensating for it.
func TestAnAuthorizedRefundPerformsOneDurableWrite(t *testing.T) {
	store := &countingStore{}
	g := NewWithStore(mustMandate(t,
		`[{"action_id":"rfa_1","payment_id":"`+payA+`","amount_paise":1000}]`), store)

	d := g.Decide(RefundTool, map[string]any{"payment_id": payA, "amount": int64(1000)}, now)
	if !d.Allowed {
		t.Fatalf("precondition: %s", d.Reason)
	}

	if store.combined != 1 {
		t.Errorf("combined reserve+rate writes = %d, want 1", store.combined)
	}
	if store.reserves != 0 {
		t.Errorf("plain reserves = %d, want 0: the forward path must take the "+
			"combined transaction, not the fallback", store.reserves)
	}
	if store.rateCalls != 0 {
		t.Errorf("separate rate-window commits = %d, want 0. Each one is an extra "+
			"fsync on the money path and a second fact that can disagree with the "+
			"first", store.rateCalls)
	}
}

// A store that CANNOT do the combined write must still work.
//
// Every test double in this repository implements Persister and nothing else,
// and widening that interface would have forced a durable-write method onto
// each of them -- where the usual stub, returning nil, is a write on a money
// path silently not happening.
func TestAPersisterWithoutTheCombinedWriteStillWorks(t *testing.T) {
	// Deliberately typed as the narrow interface, so the ledger's type assertion
	// for RateReserver fails and it takes the fallback.
	var store lifecycle.Persister = &fallbackOnly{}
	g := NewWithLedger(mustMandate(t,
		`[{"action_id":"rfa_1","payment_id":"`+payA+`","amount_paise":1000}]`),
		lifecycle.NewLedger(500000, store))

	if d := g.Decide(RefundTool,
		map[string]any{"payment_id": payA, "amount": int64(1000)}, now); !d.Allowed {
		t.Fatalf("a store without the combined write could not authorize a refund: %s", d.Reason)
	}
	if got := store.(*fallbackOnly).reserves; got != 1 {
		t.Errorf("plain reserves = %d, want 1", got)
	}
}

// fallbackOnly implements Persister and NOT RateReserver.
type fallbackOnly struct{ reserves int }

func (f *fallbackOnly) Reserve(string, string, int64) error { f.reserves++; return nil }
func (f *fallbackOnly) ReserveMany(string, []lifecycle.Reservation) error {
	f.reserves++
	return nil
}
func (f *fallbackOnly) SetState(string, string, string) error       { return nil }
func (f *fallbackOnly) SetStateMany([]string, string, string) error { return nil }
