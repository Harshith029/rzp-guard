package storage

import (
	"path/filepath"
	"testing"
	"time"
)

// shortenLease makes the TTL testable. Without it, asserting that a heartbeat
// keeps a lease alive costs 15 seconds of wall clock in the fast lane, and a
// test that slow gets skipped, which is how this invariant would rot.
func shortenLease(t *testing.T, ttl, renew time.Duration) {
	t.Helper()
	oldTTL, oldRenew := leaseTTL, leaseRenew
	leaseTTL, leaseRenew = ttl, renew
	t.Cleanup(func() { leaseTTL, leaseRenew = oldTTL, oldRenew })
}

// THE INVARIANT THE HEARTBEAT EXISTS FOR.
//
// A lease that is never renewed goes stale on its own, and a second guard then
// takes over a mandate the first one is still forwarding refunds against. Two
// ledgers over one mandate is the exact bug the lease replaced the exclusive
// lock to keep preventing, and it would be reached here by simple omission --
// nothing crashes, nothing errors, the guard just keeps running past a deadline
// nobody was refreshing.
//
// So: with the heartbeat running, a lease must survive several TTLs.
func TestAHeartbeatKeepsALeaseAliveAcrossSeveralTTLs(t *testing.T) {
	shortenLease(t, 150*time.Millisecond, 30*time.Millisecond)

	path := filepath.Join(t.TempDir(), "beating.db")
	held, err := Open(path, "mnd_beat")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	stop := make(chan struct{})
	defer close(stop)
	go held.HeartbeatLoop(stop, func(error) {})

	time.Sleep(5 * leaseTTL)

	if _, err := Open(path, "mnd_beat"); err == nil {
		t.Fatalf("after %v of heartbeats a second guard took the lease; the first "+
			"one is still forwarding refunds against its own ledger", 5*leaseTTL)
	}
	if err := held.Renew(); err != nil {
		t.Fatalf("the holder lost its own lease while heartbeating: %v", err)
	}
}

// And the other half: WITHOUT a heartbeat the lease must actually lapse, or a
// crashed guard locks a merchant out permanently.
func TestWithoutAHeartbeatTheLeaseLapses(t *testing.T) {
	shortenLease(t, 100*time.Millisecond, 20*time.Millisecond)

	path := filepath.Join(t.TempDir(), "silent.db")
	abandoned, err := Open(path, "mnd_silent")
	if err != nil {
		t.Fatal(err)
	}
	// No heartbeat, and no release either: this is a crash.
	abandoned.holder = ""
	abandoned.Close()

	time.Sleep(3 * leaseTTL)

	taken, err := Open(path, "mnd_silent")
	if err != nil {
		t.Fatalf("a lease abandoned for %v was still not takeable: %v", 3*leaseTTL, err)
	}
	taken.Close()
}
