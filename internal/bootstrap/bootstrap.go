// Package bootstrap is the ordered constructor a guard process must use.
//
// It is NOT yet the single startup path: no cmd/ executable wires it to the
// relay, the child process lifecycle and CloseInflight. Until that exists and
// has no alternate construction path, this is a correct constructor rather than
// a guaranteed entry point.
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

	return &Result{Guard: guard, Store: store, RecoveredInDoubt: recovered}, nil
}

func (r *Result) Close() error { return r.Store.Close() }
