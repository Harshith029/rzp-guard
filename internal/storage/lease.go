package storage

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"
)

// leaseTTL is how long a lease stays valid without a heartbeat.
//
// It is the only cost of moving off the exclusive lock: after a CRASH, the next
// guard for that mandate waits this long, because a stale heartbeat and a
// process that is merely busy are not distinguishable from the outside. A clean
// shutdown releases the lease, so a normal restart is instant and the wait
// applies only to a real crash -- which already needs an operator, since
// recovery promotes the in-flight reservation to IN_DOUBT.
//
// Chosen against leaseRenew rather than against a stopwatch: three missed
// heartbeats. Shorter risks a takeover from a guard that was merely descheduled
// during a long fsync, which would produce the two-ledger bug the lease exists
// to prevent -- the one failure direction that costs money rather than time.
// A var, not a const, so tests can shorten it -- the same reason
// lockAcquireDeadline is one. Nothing outside this package can set it, and a
// test that shortened it in production would be a test that could not compile
// there.
var leaseTTL = 15 * time.Second

// leaseRenew is how often the holder refreshes. Well inside leaseTTL so a
// single slow write, or one scheduling hiccup, does not look like a death.
var leaseRenew = 4 * time.Second

// ErrLeaseHeld means another live process holds this mandate's lease. It wraps
// ErrNotOwner so every existing caller that checks for an ownership conflict
// keeps working unchanged -- the mechanism moved, the answer did not.
var ErrLeaseHeld = fmt.Errorf("%w: a live guard holds this mandate's lease", ErrNotOwner)

// Lease is who holds a mandate right now, for an operator to read.
type Lease struct {
	MandateID  string
	Host       string
	PID        int
	AcquiredAt string
	Heartbeat  time.Time
	Live       bool
}

func newHolderToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("storage: lease token: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// acquireLease takes the lease for one mandate, or refuses.
//
// ATOMICITY IS THE POINT. The whole decision is one statement whose WHERE
// clause carries the condition, so two processes racing cannot both read "no
// live lease" and both write. A read-then-write version of this would be a
// textbook check-then-act race over money, and it would pass every
// single-process test.
func acquireLease(db *sql.DB, mandateID string) (string, error) {
	holder, err := newHolderToken()
	if err != nil {
		return "", err
	}
	host, _ := os.Hostname()
	now := time.Now().UTC()
	cutoff := now.Add(-leaseTTL).UnixNano()

	res, err := db.Exec(
		`INSERT INTO owner_lease (mandate_id, holder, host, pid, acquired_at, heartbeat_ns)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(mandate_id) DO UPDATE SET
		   holder = excluded.holder, host = excluded.host, pid = excluded.pid,
		   acquired_at = excluded.acquired_at, heartbeat_ns = excluded.heartbeat_ns
		 WHERE owner_lease.heartbeat_ns < ?`,
		mandateID, holder, host, os.Getpid(),
		now.Format(time.RFC3339Nano), now.UnixNano(), cutoff)
	if err != nil {
		if lockContended(err) {
			return "", fmt.Errorf("%w (%v)", errLockContended, err)
		}
		return "", fmt.Errorf("storage: acquire lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", fmt.Errorf("storage: acquire lease: rows affected: %w", err)
	}
	if n != 1 {
		// Nothing changed, so a live lease exists. Report WHO holds it: during an
		// incident "another guard has it" is a dead end, and "pid 4711 on
		// host-a, last seen 2s ago" is an instruction.
		cur, _, lerr := readLease(db, mandateID, now)
		if lerr != nil {
			return "", fmt.Errorf("%w (and the holder could not be read: %v)", ErrLeaseHeld, lerr)
		}
		return "", fmt.Errorf(
			"%w: %s is held by pid %d on %s, last heartbeat %s ago (lease expires "+
				"after %s without one). Two guards over one mandate would each check "+
				"the cumulative cap against their own in-memory ledger, so between "+
				"them they could spend past it",
			ErrLeaseHeld, mandateID, cur.PID, cur.Host,
			now.Sub(cur.Heartbeat).Truncate(time.Millisecond), leaseTTL)
	}
	return holder, nil
}

func readLease(db *sql.DB, mandateID string, now time.Time) (Lease, bool, error) {
	var l Lease
	var ns int64
	err := db.QueryRow(
		`SELECT mandate_id, host, pid, acquired_at, heartbeat_ns
		   FROM owner_lease WHERE mandate_id = ?`, mandateID).
		Scan(&l.MandateID, &l.Host, &l.PID, &l.AcquiredAt, &ns)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{MandateID: mandateID}, false, nil
	}
	if err != nil {
		return Lease{}, false, fmt.Errorf("storage: read lease: %w", err)
	}
	l.Heartbeat = time.Unix(0, ns).UTC()
	l.Live = ns >= now.Add(-leaseTTL).UnixNano()
	return l, true, nil
}

// LeaseFor reports who holds a mandate, and whether that holder is still alive.
//
// This is what lets rzp-guard-operator give a useful answer instead of a lock
// error. It is the difference between "could not take the state file -- is the
// guard still running?" and "yes, pid 4711 has it, and here is what you can
// still do while it does".
func (s *Store) LeaseFor(mandateID string) (Lease, bool, error) {
	return readLease(s.db, mandateID, time.Now().UTC())
}

// Renew refreshes this process's lease.
//
// Conditional on the holder token, so a process whose lease was taken over
// while it was stalled cannot silently reclaim it by heartbeating. It gets an
// error instead, which is the signal that it must stop touching money.
func (s *Store) Renew() error {
	if s.attached || s.holder == "" {
		return nil
	}
	res, err := s.db.Exec(
		`UPDATE owner_lease SET heartbeat_ns = ? WHERE mandate_id = ? AND holder = ?`,
		time.Now().UTC().UnixNano(), s.mandateID, s.holder)
	if err != nil {
		return fmt.Errorf("storage: renew lease: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: this process no longer holds the lease on %s; "+
			"another guard took it over after this one stopped heartbeating",
			ErrNotOwner, s.mandateID)
	}
	return nil
}

// releaseLease drops the lease on a clean shutdown, so the next start does not
// have to wait out leaseTTL. Conditional on the holder token: a process that
// already lost the lease must not release the new holder's.
//
// Failure here is deliberately not fatal and not returned. The lease expires on
// its own; the only cost of a missed release is one TTL of delay on the next
// start, which is not worth failing a shutdown over.
func (s *Store) releaseLease() {
	if s.attached || s.holder == "" {
		return
	}
	_, _ = s.db.Exec(
		`DELETE FROM owner_lease WHERE mandate_id = ? AND holder = ?`,
		s.mandateID, s.holder)
}

// HeartbeatLoop renews the lease until stop is closed. It is what makes the
// lease mean "a guard is alive" rather than "a guard once started".
//
// onLost is called if renewal ever fails. Losing a lease means another process
// believes it owns this mandate's ledger, which is the two-ledger condition, so
// the caller's only correct response is to stop forwarding.
func (s *Store) HeartbeatLoop(stop <-chan struct{}, onLost func(error)) {
	if s.attached || s.holder == "" {
		return
	}
	t := time.NewTicker(leaseRenew)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := s.Renew(); err != nil && onLost != nil {
				onLost(err)
			}
		}
	}
}

// StrandedElsewhere lists unresolved actions belonging to OTHER mandates in the
// same state file.
//
// This replaces the refusal that used to guard the same problem. The old rule
// was: opening a populated file under a different mandate hides everything the
// previous one left behind, so refuse. That was correct while a file held one
// mandate, and it is the wrong shape now that a file can hold several -- it
// would refuse the normal case.
//
// The guarantee moves from REFUSE to SURFACE, which is strictly stronger: the
// old refusal only fired on the next start of the same file, whereas this is
// reported at every start and is visible to the operator continuously through
// rzp-guard-operator queue -all. Nothing is hidden; something is now shown that
// previously only existed as a reason to fail.
func (s *Store) StrandedElsewhere() (map[string][]string, error) {
	rows, err := s.db.Query(
		`SELECT mandate_id, action_id FROM action_state
		   WHERE mandate_id <> ? AND state IN ('RESERVED', 'IN_DOUBT')
		   ORDER BY mandate_id, action_id`, s.mandateID)
	if err != nil {
		return nil, fmt.Errorf("storage: stranded elsewhere: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var mid, aid string
		if err := rows.Scan(&mid, &aid); err != nil {
			return nil, err
		}
		out[mid] = append(out[mid], aid)
	}
	return out, rows.Err()
}

// Mandates lists every mandate this state file holds, for a cross-mandate
// operator view. A file with more than one is the point of the lease: ten
// merchants on a host share one queue, one operator credential and one alert
// sink instead of ten of each.
func (s *Store) Mandates() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT mandate_id FROM action_state
		 UNION SELECT mandate_id FROM owner_lease
		 UNION SELECT mandate_id FROM denial
		 ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("storage: mandates: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
