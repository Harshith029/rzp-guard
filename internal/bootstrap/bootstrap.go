// Package bootstrap is the ordered constructor a guard process must use.
//
// It IS the single startup path. cmd/rzp-guard/main.go constructs all durable
// state through Open and nothing else, wires the resulting Guard to the relay,
// and guarantees CloseInflight on every exit route. This comment said the
// opposite, describing a state of the world main.go had already left behind.
//
// Durable state is only worth having if something actually restores it. A
// previous revision persisted the rate window and the action lifecycle but left
// recovery to whoever remembered to call the right methods in the right order,
// and no such caller existed -- so a restart still reset max_calls_per_minute.
//
// Everything here happens BEFORE the relay reads a single byte of stdin.
package bootstrap

import (
	"fmt"
	"time"

	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/policy"
	"github.com/harshith/rzp-guard/internal/storage"
)

// Result is everything a startup needs to hand to the relay and the operator
// console, plus what recovery had to lock.
type Result struct {
	Guard *policy.Guard
	Store *storage.Store

	// RecoveredInDoubt lists actions that were mid-flight when the previous
	// process died. Each is locked until an operator resolves it.
	RecoveredInDoubt []string

	// StrandedElsewhere is unresolved work belonging to OTHER mandates in the
	// same state file. It is reported rather than refused: a shared file is the
	// supported multi-tenant case now, and hiding another merchant's stuck
	// refund behind a silent scope boundary is what the old refusal existed to
	// prevent. See storage.StrandedElsewhere.
	StrandedElsewhere map[string][]string

	// stopBeat ends the lease heartbeat on Close.
	stopBeat    chan struct{}
	onLeaseLost func(error)
}

// Open performs, in order: exclusive ownership, crash recovery, lifecycle
// snapshot restore, rate-window restore.
//
// The order is load-bearing. Ownership first, or two processes race the
// recovery step. Recovery before snapshot, or a still-RESERVED row would be
// restored as RESERVED and could be replayed rather than held.
func Open(dbPath string, m *mandate.Mandate, now time.Time) (*Result, error) {
	// 1. Exclusive ownership. A second guard over the same state file would
	//    enforce the cumulative cap against its own in-memory ledger.
	store, err := storage.Open(dbPath, m.MandateID)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: acquire state file: %w", err)
	}

	// 2. Crash recovery: any still-RESERVED row was mid-flight when the previous
	//    process died, which is exactly the ambiguous case.
	recovered, err := store.RecoverStartup()
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("bootstrap: recover: %w", err)
	}

	// 3. Lifecycle snapshot.
	snap, err := store.Snapshot()
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("bootstrap: snapshot: %w", err)
	}

	guard := policy.NewWithStore(m, store)
	guard.Restore(snap.States, snap.Amounts)

	// 4. Rate window. Without this a crash-loop bypasses max_calls_per_minute.
	if err := guard.RestoreRateWindow(now); err != nil {
		store.Close()
		return nil, fmt.Errorf("bootstrap: restore rate window: %w", err)
	}

	// 5. What ELSE is stuck in this file. Read once, at startup, before any
	//    traffic -- the same moment the old ownership check ran, so nothing that
	//    used to be refused now passes unmentioned.
	stranded, err := store.StrandedElsewhere()
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("bootstrap: stranded elsewhere: %w", err)
	}

	res := &Result{
		Guard: guard, Store: store,
		RecoveredInDoubt:  recovered,
		StrandedElsewhere: stranded,
		stopBeat:          make(chan struct{}),
	}

	// 6. Start the heartbeat LAST, and only once everything above succeeded.
	//
	//    The lease is what stops a second guard building a second ledger over
	//    this mandate, and it means "a guard is alive" only if something keeps
	//    saying so. Without this the lease goes stale after leaseTTL and a
	//    second process takes it over while this one is still forwarding
	//    refunds -- which is the exact two-ledger bug, arrived at by omission.
	//
	//    onLost is deliberately not fatal here. bootstrap does not own the
	//    process's exit path; it reports upward and main decides, which is the
	//    same division as everywhere else in this package.
	go store.HeartbeatLoop(res.stopBeat, func(err error) {
		if res.onLeaseLost != nil {
			res.onLeaseLost(err)
		}
	})

	return res, nil
}

// OnLeaseLost installs the handler for losing the mandate lease mid-session.
//
// Losing it means another process believes it owns this mandate's ledger, so
// the only correct response is to stop forwarding. Set it before any traffic.
func (r *Result) OnLeaseLost(f func(error)) { r.onLeaseLost = f }

func (r *Result) Close() error {
	if r.stopBeat != nil {
		close(r.stopBeat)
		r.stopBeat = nil
	}
	return r.Store.Close()
}
