package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/harshith/rzp-guard/internal/buildinfo"
	"github.com/harshith/rzp-guard/internal/policy"
)

// A lock-free window into a RUNNING guard.
//
// THE PROBLEM THIS SOLVES. The operator CLI reads the state file, and the guard
// holds an EXCLUSIVE lock on it for its entire lifetime. So the only way to ask
// "is anything stuck?" was: stop the guard, look, restart. That makes routine
// observation cost an outage, which means in practice nobody looks, which is
// how an IN_DOUBT refund sits unnoticed.
//
// The fix is not to weaken the lock -- single-writer ownership is a money
// guarantee (two guards would each check the cumulative cap against their own
// ledger). It is to have the guard PUBLISH what a reader needs, one-way, into a
// separate file it owns. Monitoring reads that file. No lock is taken, no
// contention is possible, and the guarantee is untouched.
//
// This is deliberately observation only. RESOLVING an IN_DOUBT action still
// requires the state file and still requires stopping the guard, because
// resolution is a write and writes are what the lock exists to serialise.
// Seeing and deciding are different privileges and this closes only the first.
//
// Written atomically (temp + rename) so a reader never sees a half-written
// document, and 0600 because it names payment-linked action ids.
type statusWriter struct {
	path string
	mu   sync.Mutex
	g    *policy.Guard
	mnd  string
	pid  int
	born time.Time

	// denials reports refusals the deny-path rate cap could not record. Set
	// after construction rather than passed in, because the recorder is built
	// later in the startup order and reordering that path -- where the defer
	// sequence is load-bearing and commented as such -- is not worth one field.
	denials *denialRecorder
}

// setDenials wires the recorder once it exists. Nil-safe: a status file with no
// recorder simply reports nothing dropped, which is true.
func (s *statusWriter) setDenials(d *denialRecorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.denials = d
}

type statusDoc struct {
	Schema       int       `json:"schema"`
	Program      string    `json:"program"`
	Version      string    `json:"version"`
	Commit       string    `json:"commit"`
	PID          int       `json:"pid"`
	MandateID    string    `json:"mandate_id"`
	StartedAt    time.Time `json:"started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	UptimeSecond int64     `json:"uptime_seconds"`

	// The numbers an operator or a monitor actually acts on.
	InDoubtCount   int      `json:"in_doubt_count"`
	InDoubtActions []string `json:"in_doubt_actions"`
	EncumberedPais int64    `json:"encumbered_paise"`
	CommittedPaise int64    `json:"committed_paise"`
	RemainingPaise int64    `json:"remaining_paise"`

	// NeedsOperator is the single field a monitoring rule should watch. It is
	// computed here rather than left to the reader so every consumer agrees on
	// what "needs a human" means.
	NeedsOperator bool `json:"needs_operator"`

	// DenialsUnrecorded counts refusals the deny-path rate cap did not write.
	//
	// Non-zero means the operator queue is INCOMPLETE. A short queue because it
	// overflowed must never look like a short queue because nothing was refused,
	// and this is the field that separates them for a reader with no metrics
	// stack.
	DenialsUnrecorded int64 `json:"denials_unrecorded"`
}

func newStatusWriter(path string, g *policy.Guard, mandateID string) *statusWriter {
	return &statusWriter{
		path: path, g: g, mnd: mandateID,
		pid: os.Getpid(), born: time.Now().UTC(),
	}
}

func (s *statusWriter) snapshot(now time.Time) statusDoc {
	inDoubt := s.g.InDoubtActions()
	return statusDoc{
		Schema:         1,
		Program:        "rzp-guard",
		Version:        buildinfo.Version,
		Commit:         buildinfo.Commitish(),
		PID:            s.pid,
		MandateID:      s.mnd,
		StartedAt:      s.born,
		UpdatedAt:      now,
		UptimeSecond:   int64(now.Sub(s.born).Seconds()),
		InDoubtCount:   len(inDoubt),
		InDoubtActions: inDoubt,
		EncumberedPais: s.g.Encumbered(),
		CommittedPaise: s.g.Committed(),
		RemainingPaise: s.g.Remaining(),
		NeedsOperator:  len(inDoubt) > 0,

		DenialsUnrecorded: droppedFrom(s.denials),
	}
}

// droppedFrom is nil-safe so the snapshot cannot fail on a status writer that
// was never given a recorder -- which is every test predating this field.
func droppedFrom(d *denialRecorder) int64 {
	if d == nil {
		return 0
	}
	return d.Dropped()
}

// write publishes the current status atomically.
func (s *statusWriter) write(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := json.MarshalIndent(s.snapshot(now), "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	// Same directory, so the rename cannot cross a filesystem boundary and
	// stays atomic. A reader either sees the previous document or the new one.
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".rzp-status-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

// run publishes on a ticker until ctx-like stop channel closes. Failures are
// reported once and never fatal: losing the status file must not take down the
// guard, because the status file is an observability aid and the guard is the
// control that matters.
func (s *statusWriter) run(stop <-chan struct{}, every time.Duration) {
	if err := s.write(time.Now().UTC()); err != nil {
		fmt.Fprintf(os.Stderr, "rzp-guard: status file: %v (continuing)\n", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	var warned bool
	for {
		select {
		case <-stop:
			// A final write, so the last thing on disk reflects the end state
			// -- including anything CloseInflight has just locked.
			if err := s.write(time.Now().UTC()); err != nil && !warned {
				fmt.Fprintf(os.Stderr, "rzp-guard: status file: %v\n", err)
			}
			return
		case now := <-t.C:
			if err := s.write(now.UTC()); err != nil && !warned {
				warned = true
				fmt.Fprintf(os.Stderr, "rzp-guard: status file: %v (continuing)\n", err)
			}
		}
	}
}
