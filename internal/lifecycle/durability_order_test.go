package lifecycle

import (
	"errors"
	"testing"
)

// The relay discards the error from every transition out of RESERVED:
//
//	_ = r.guard.MarkInDoubt(p.actionID)
//	_ = r.guard.Commit(p.actionID)
//	_ = r.guard.ReleaseConfirmedRejection(d.MatchedActionID)
//
// That is safe, but only because of an ordering invariant here: the durable
// write happens FIRST and the in-memory entry is mutated only after it
// succeeds. A failed write leaves memory and the database agreeing on RESERVED,
// which RecoverStartup promotes to IN_DOUBT so a human looks.
//
// TestFailedDurableWriteLeavesNoClaim covers Reserve and Commit. It does not
// cover the other two transitions, and it does not check the budget -- which is
// the part that costs money. Releasing an action in memory while the database
// still holds it RESERVED would hand the freed paise back to the cumulative cap
// and let them be spent a second time.
//
// Reorder transition() to mutate the entry before the write and these fail.
func TestAFailedDurableWriteStrandsNoBudget(t *testing.T) {
	diskGone := errors.New("disk full")

	for _, tc := range []struct {
		name string
		move func(*Ledger) error
	}{
		{"MarkInDoubt", func(l *Ledger) error { return l.MarkInDoubt("rfa_001") }},
		{"ReleaseConfirmedRejection", func(l *Ledger) error {
			return l.ReleaseConfirmedRejection("rfa_001")
		}},
		{"Commit", func(l *Ledger) error { return l.Commit("rfa_001") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &recordingStore{}
			l := NewLedger(100000, st)
			if err := l.Reserve("rfa_001", "rzpg_aaaaaaaaaaaa", 30000); err != nil {
				t.Fatalf("setup reserve: %v", err)
			}

			st.setStateErr = diskGone // the disk goes away mid-flight
			err := tc.move(l)
			if err == nil {
				t.Fatal("the transition reported success although nothing was written")
			}
			if !errors.Is(err, diskGone) {
				t.Fatalf("the underlying write failure was not preserved, so a "+
					"caller cannot tell a storage outage from a rejected "+
					"transition: %v", err)
			}
			if got := l.State("rfa_001"); got != Reserved {
				t.Fatalf("state moved to %s on a failed write; memory and the "+
					"database now disagree, and the relay discards this error", got)
			}
			if got := l.Encumbered(); got != 30000 {
				t.Fatalf("encumbered is %d after a failed write, want 30000 still "+
					"held -- budget the database still reserves must not be "+
					"handed back to the cap", got)
			}
			if got := l.Committed(); got != 0 {
				t.Fatalf("committed is %d after a failed write, want 0", got)
			}
		})
	}
}

// The same action twice in one call would be checked once against the cap and
// consumed once, while the caller believes it paid for two -- so the ledger
// refuses it outright.
//
// Nothing upstream can currently produce such a set: combineExact walks each
// action at most once, and ReceiptForSet rejects duplicates before this is
// reached. That is exactly why this test exists. A mutation deleting the check
// went unnoticed by every other test, which means the guarantee was resting on
// two callers happening to be careful rather than on the ledger enforcing it.
func TestReserveManyRefusesTheSameActionTwice(t *testing.T) {
	st := &recordingStore{}
	l := NewLedger(100000, st)

	err := l.ReserveMany("rzpg_aaaaaaaaaaaa", []Reservation{
		{ActionID: "rfa_001", AmountPaise: 10000},
		{ActionID: "rfa_001", AmountPaise: 10000},
	})
	if err == nil {
		t.Fatal("reserved the same action twice in one call: it would be counted " +
			"once against the cumulative cap while the caller believes two " +
			"separate authorizations were spent")
	}
	if got := l.State("rfa_001"); got != Available {
		t.Fatalf("state is %s after a refused duplicate, want AVAILABLE", got)
	}
	if got := l.Encumbered(); got != 0 {
		t.Fatalf("a refused duplicate encumbered %d paise", got)
	}
	// And nothing may have been written durably.
	if len(st.reserves) != 0 {
		t.Fatalf("a refused duplicate reached the durable layer: %v", st.reserves)
	}
}

// An empty set is a caller bug, not a no-op refund.
func TestReserveManyRefusesAnEmptySet(t *testing.T) {
	l := NewLedger(100000, &recordingStore{})
	if err := l.ReserveMany("rzpg_aaaaaaaaaaaa", nil); err == nil {
		t.Fatal("an empty reservation set was accepted")
	}
}
