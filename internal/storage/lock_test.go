package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Ownership is a LEASE now, not a file-wide exclusive lock, and that changes
// what "contended" means.
//
// Under locking_mode = EXCLUSIVE the answer to "may I own this?" was itself
// contended: simultaneous openers could all hold SHARED and none could upgrade,
// so a single attempt yielded ZERO owners rather than one, and Open needed a
// bounded retry with fresh connections to break it out.
//
// A lease is a conditional UPDATE. Its answer is available immediately and it is
// final: either the row moved, in which case this process owns the mandate, or
// it did not, in which case a live holder does. Retrying that would only make a
// clear refusal slow. The retry loop stays for genuine SQLite lock contention on
// the schema statement, which is a different thing and still transient.
//
// So the three properties asserted here are the ones that still have to hold:
// the answer is bounded, it is a NAMED ownership conflict rather than an
// anonymous failure, and losing a takeover never costs the incumbent anything.

func TestALeasedMandateRefusesASecondOwnerImmediatelyAndByName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held.db")
	incumbent, err := Open(path, "mnd_incumbent")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer incumbent.Close()

	start := time.Now()
	second, err := openWithDeadline(path, "mnd_incumbent", 2*time.Second)
	elapsed := time.Since(start)
	if err == nil {
		second.Close()
		t.Fatal("a second guard took a mandate another guard holds; both would " +
			"check the cumulative cap against their own in-memory ledger, so " +
			"between them they could spend past it")
	}
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("refused with %v, want ErrNotOwner: a caller must be able to "+
			"tell an ownership conflict from a corrupt or unreadable state file", err)
	}
	// A held lease is a decision, not contention. Waiting out a deadline for it
	// would turn a clear refusal into a slow one.
	if elapsed > time.Second {
		t.Fatalf("refusing a held lease took %v; it is being retried as though it "+
			"were transient lock contention", elapsed)
	}
	// The refusal has to be actionable during an incident. "Someone else has it"
	// is a dead end; naming the process is an instruction.
	for _, want := range []string{"mnd_incumbent", "pid"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}

	// Losing a takeover must not cost the incumbent its lease, or the guard
	// would fail closed on every refund after someone rattled the door.
	if err := incumbent.Reserve("rfa_001", "rzpg_aaaaaaaaaaaa", 5000); err != nil {
		t.Fatalf("the incumbent could not write after refusing a takeover: %v", err)
	}
	if err := incumbent.Renew(); err != nil {
		t.Fatalf("the incumbent could not renew after refusing a takeover: %v", err)
	}
}

// A clean shutdown releases the lease, so an ordinary restart does not wait out
// the TTL. Without this every deploy would stall for leaseTTL, which is the
// kind of cost that gets a safety mechanism turned off.
func TestACleanShutdownReleasesTheLeaseImmediately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cycle.db")
	first, err := Open(path, "mnd_cycle")
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	start := time.Now()
	second, err := Open(path, "mnd_cycle")
	if err != nil {
		t.Fatalf("restart after a clean shutdown was refused: %v", err)
	}
	defer second.Close()
	if elapsed := time.Since(start); elapsed > leaseTTL {
		t.Fatalf("restart took %v, at or past the %v TTL: the lease was not released",
			elapsed, leaseTTL)
	}
}

// A crashed guard leaves a lease nobody releases. It must expire, or one crash
// locks a merchant's refunds out permanently and the only recovery is editing
// the database by hand.
func TestAStaleLeaseIsTakenOver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crashed.db")
	crashed, err := Open(path, "mnd_crash")
	if err != nil {
		t.Fatal(err)
	}
	// Close the handle WITHOUT releasing, which is what a crash looks like from
	// the next process's side.
	crashed.holder = ""
	crashed.Close()

	if _, err := Open(path, "mnd_crash"); err == nil {
		t.Fatal("a fresh lease was taken while the crashed one was still within " +
			"its TTL; a guard that is merely slow would lose its ledger")
	}

	// Age the heartbeat past the TTL, the way wall-clock time would.
	aged, err := Attach(path, "mnd_crash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aged.db.Exec(
		`UPDATE owner_lease SET heartbeat_ns = ? WHERE mandate_id = 'mnd_crash'`,
		time.Now().Add(-2*leaseTTL).UnixNano()); err != nil {
		t.Fatal(err)
	}
	aged.Close()

	taken, err := Open(path, "mnd_crash")
	if err != nil {
		t.Fatalf("a lease stale for %v was not takeable: %v", 2*leaseTTL, err)
	}
	defer taken.Close()
}

// Renewal is conditional on the holder token. A process whose lease was taken
// over while it was stalled must NOT be able to reclaim it by heartbeating --
// it would leave two processes each believing they own the ledger, which is the
// exact condition the lease exists to prevent.
func TestADisplacedHolderCannotHeartbeatItsWayBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "displaced.db")
	old, err := Open(path, "mnd_displaced")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { old.holder = ""; old.Close() }()

	// Someone else takes over after the TTL lapses.
	if _, err := old.db.Exec(
		`UPDATE owner_lease SET heartbeat_ns = ? WHERE mandate_id = 'mnd_displaced'`,
		time.Now().Add(-2*leaseTTL).UnixNano()); err != nil {
		t.Fatal(err)
	}
	newer, err := Open(path, "mnd_displaced")
	if err != nil {
		t.Fatal(err)
	}
	defer newer.Close()

	if err := old.Renew(); err == nil {
		t.Fatal("the displaced process renewed a lease it no longer holds")
	} else if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("renewal failed with %v, want ErrNotOwner", err)
	}
	if err := newer.Renew(); err != nil {
		t.Fatalf("the real holder could not renew: %v", err)
	}
}

// An attached store leases nothing. That is what lets the operator work while a
// guard runs, and it must not accidentally displace the guard.
func TestAttachingTakesNoLease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attach.db")
	guard, err := Open(path, "mnd_attach")
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()

	op, err := Attach(path, "mnd_attach")
	if err != nil {
		t.Fatalf("the operator could not attach while a guard held the lease: %v", err)
	}
	defer op.Close()
	if !op.Attached() {
		t.Error("a store opened with Attach does not report itself as attached")
	}

	lease, found, err := op.LeaseFor("mnd_attach")
	if err != nil || !found {
		t.Fatalf("the operator cannot see the guard's lease: found=%v err=%v", found, err)
	}
	if !lease.Live {
		t.Error("a running guard's lease reads as not live")
	}
	// Attaching and then closing must not release the guard's lease.
	op.Close()
	if err := guard.Renew(); err != nil {
		t.Fatalf("the guard lost its lease to an attached reader: %v", err)
	}
}

// A retry must not delay an answer this build has already reached. A state file
// at an unsupported schema version is a decision, not contention.
func TestOpenDoesNotRetryADecisionItHasAlreadyMade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	s, err := Open(path, "mnd_version")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s.Close()
	setVersion(t, path, schemaVersion+1)

	start := time.Now()
	_, err = Open(path, "mnd_version")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("got %v, want ErrSchemaVersion", err)
	}
	if elapsed >= lockAcquireDeadline {
		t.Fatalf("a schema-version refusal took %v, at or past the %v lock "+
			"deadline: it is being retried as if it were contention",
			elapsed, lockAcquireDeadline)
	}
}
