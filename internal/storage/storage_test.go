package storage

import (
	"path/filepath"
	"testing"
)

// A second guard process must not be able to open the same state file. Each
// process restores its own in-memory ledger and checks headroom locally, so two
// instances over one database would reserve different actions and jointly
// exceed the mandate cap. SetMaxOpenConns(1) only serializes writers inside one
// process; it says nothing across processes.
func TestSecondInstanceCannotOpenTheSameStateFile(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")

	first, err := Open(db, "mnd_test")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer first.Close()

	second, err := Open(db, "mnd_test")
	if err == nil {
		second.Close()
		t.Fatal("a second instance opened the same state file: two guards would " +
			"each enforce the cumulative cap against their own in-memory ledger")
	}
}

func TestOwnershipIsReleasedOnClose(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")
	first, err := Open(db, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(db, "mnd_test")
	if err != nil {
		t.Fatalf("clean restart was refused: %v", err)
	}
	second.Close()
}

// ON CONFLICT ... WHERE state='AVAILABLE' succeeds with zero rows changed when
// the action is already RESERVED/COMMITTED/IN_DOUBT. Without a RowsAffected
// check the caller treats that as success and marks the action reserved anyway.
func TestReserveFailsWhenTheRowIsNotAvailable(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")
	st, err := Open(db, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.Reserve("rfa_001", "rzpg_aaaaaaaaaaaa", 1000); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	// Still RESERVED: a second reservation must be refused, loudly.
	if err := st.Reserve("rfa_001", "rzpg_aaaaaaaaaaaa", 1000); err == nil {
		t.Fatal("re-reserving a RESERVED action reported success with zero rows changed")
	}

	if err := st.SetState("rfa_001", "RESERVED", "COMMITTED"); err != nil {
		t.Fatal(err)
	}
	if err := st.Reserve("rfa_001", "rzpg_aaaaaaaaaaaa", 1000); err == nil {
		t.Fatal("re-reserving a COMMITTED action reported success")
	}
}

// State transitions need the same expected-state discipline: an UPDATE that
// matches nothing must not look like a successful transition.
func TestSetStateFailsForAnUnknownAction(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")
	st, err := Open(db, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.SetState("rfa_does_not_exist", "RESERVED", "COMMITTED"); err == nil {
		t.Fatal("SetState on a non-existent row reported success")
	}
}

// A transition must assert the state it is moving FROM. Matching on
// (mandate, action) alone would let a stale caller move a COMMITTED action back
// to AVAILABLE, and RowsAffected == 1 would report success: it proves the row
// exists, not that the intended transition happened.
func TestStaleTransitionCannotReleaseACommittedAction(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")
	st, err := Open(db, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.Reserve("rfa_001", "rzpg_bbbbbbbbbbbb", 5000); err != nil {
		t.Fatal(err)
	}
	if err := st.SetState("rfa_001", "RESERVED", "COMMITTED"); err != nil {
		t.Fatal(err)
	}
	// A late duplicate reply tries the transition it thinks is pending.
	if err := st.SetState("rfa_001", "RESERVED", "AVAILABLE"); err == nil {
		t.Fatal("a stale RESERVED->AVAILABLE transition released a COMMITTED action")
	}
	snap, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.States["rfa_001"] != "COMMITTED" {
		t.Fatalf("state = %s, want COMMITTED", snap.States["rfa_001"])
	}
}

// The rate window has to survive a restart or it is not a security control.
func TestRateWindowSurvivesRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")

	st, err := Open(db, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 7; i++ {
		if err := st.RecordCall(nowUnixNano() + int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	st2, err := Open(db, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	times, err := st2.RecentCalls(nowUnixNano() - int64(60_000_000_000))
	if err != nil {
		t.Fatal(err)
	}
	if len(times) != 7 {
		t.Fatalf("recovered %d calls in the window, want 7: a restart would reset "+
			"max_calls_per_minute", len(times))
	}
}
