package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
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

	start := time.Now()
	second, err := Open(db, "mnd_test")
	elapsed := time.Since(start)
	if err == nil {
		second.Close()
		t.Fatal("a second instance opened the same state file: two guards would " +
			"each enforce the cumulative cap against their own in-memory ledger")
	}
	// Refused as a named ownership conflict, not an anonymous failure: a caller
	// has to be able to tell contention from a corrupt or unreadable state file.
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("refused with %v, want ErrNotOwner", err)
	}
	// Refused at startup, not waited on. Adding a busy_timeout to the DSN would
	// turn this into a stall that surfaces mid-refund instead.
	if elapsed > 10*time.Second {
		t.Fatalf("the second open blocked for %v before being refused", elapsed)
	}
	// A rejected takeover must not cost the incumbent its lock, or the guard
	// would fail closed on every refund after someone else tried the door.
	if err := first.Reserve("rfa_001", "rzpg_aaaaaaaaaaaa", 5000); err != nil {
		t.Fatalf("the incumbent could not write after a rejected takeover: %v", err)
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
	// Still RESERVED: a second reservation must be refused, loudly. The upsert's
	// WHERE state = 'AVAILABLE' guard changes no row, so this is ErrNoRowChanged
	// and not a receipt collision -- the row already exists and is being updated,
	// not inserted a second time.
	if err := st.Reserve("rfa_001", "rzpg_aaaaaaaaaaaa", 1000); err == nil {
		t.Fatal("re-reserving a RESERVED action reported success with zero rows changed")
	} else if !errors.Is(err, ErrNoRowChanged) {
		t.Fatalf("refused with %v, want ErrNoRowChanged", err)
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
	} else if !errors.Is(err, ErrNoRowChanged) {
		t.Fatalf("refused with %v, want ErrNoRowChanged", err)
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

// A fixture credential must be distinguishable from a real one.
//
// init-ephemeral derives a verifier from a token that is immediately discarded.
// That satisfies "a credential is configured" while being impossible for any
// human to present, so a state file marked this way must never be accepted by
// the production guard: an allowed refund could land IN_DOUBT with no possible
// operator resolution. Without the marker, "configured" only proves a row
// exists -- not that recovery is possible.
func TestEphemeralVerifierIsDistinguishableFromAReal(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real.db")
	st, err := Open(real, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InitOperatorVerifier("argon2id$3$65536$4$c2FsdA$aGFzaA"); err != nil {
		t.Fatal(err)
	}
	_, configured, ephemeral, err := st.OperatorVerifier()
	st.Close()
	if err != nil || !configured {
		t.Fatalf("real credential not configured: %v", err)
	}
	if ephemeral {
		t.Fatal("a real credential was marked ephemeral")
	}

	eph := filepath.Join(t.TempDir(), "eph.db")
	st2, err := Open(eph, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.InitEphemeralVerifier("argon2id$3$65536$4$c2FsdA$aGFzaA"); err != nil {
		t.Fatal(err)
	}
	_, configured, ephemeral, err = st2.OperatorVerifier()
	st2.Close()
	if err != nil || !configured {
		t.Fatalf("ephemeral credential not configured: %v", err)
	}
	if !ephemeral {
		t.Fatal("an ephemeral fixture credential was NOT marked, so the guard would " +
			"accept a state file nobody can recover")
	}
}
