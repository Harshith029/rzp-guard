// Package storage is durable state for the authorization boundary.
//
// In-memory state is not fail-closed. A crash between reserving an action and
// receiving the child's reply loses the reservation, so after restart the same
// mandate replays, consumed actions return, and the cumulative cap is bypassed.
// The design's central safety property -- IN_DOUBT stays locked until an
// operator resolves it -- has to survive Ctrl-C to mean anything.
//
// Recovery rule: any row still RESERVED at startup is promoted to IN_DOUBT.
// A reservation that was live when the process died is exactly the ambiguous
// case: the bytes may have reached Razorpay and the refund may have landed.
// Fail closed and make a human look.
//
// Pure-Go driver (modernc.org/sqlite): CGO_ENABLED=0 on this machine, and a
// cgo dependency would break the single-static-binary story.
package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

var ErrReceiptExists = errors.New("receipt already issued")

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS action_state (
  mandate_id   TEXT    NOT NULL,
  action_id    TEXT    NOT NULL,
  -- UNIQUE because the receipt is a TRUNCATED hash, not a guaranteed-unique
  -- value. This constraint is what actually prevents two actions sharing a
  -- provider-side correlation key; the hash only makes a collision unlikely.
  receipt      TEXT    NOT NULL UNIQUE,
  state        TEXT    NOT NULL,
  amount_paise INTEGER NOT NULL,
  updated_at   TEXT    NOT NULL,
  PRIMARY KEY (mandate_id, action_id)
);

CREATE TABLE IF NOT EXISTS audit (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  at            TEXT NOT NULL,
  actor         TEXT NOT NULL,
  action_id     TEXT NOT NULL,
  mandate_id    TEXT NOT NULL,
  from_state    TEXT NOT NULL,
  to_state      TEXT NOT NULL,
  refund_landed INTEGER NOT NULL,
  reason        TEXT NOT NULL
);
`

// Store is the durable side of the action lifecycle.
type Store struct {
	db        *sql.DB
	mandateID string
}

// Open prepares the database and applies the schema.
func Open(path, mandateID string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("storage: open: %w", err)
	}
	// One writer: the guard is a single process and SQLite writes serialize
	// anyway. Avoids "database is locked" under concurrent reservations.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: schema: %w", err)
	}
	return &Store{db: db, mandateID: mandateID}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// RecoverStartup promotes every still-RESERVED row to IN_DOUBT and returns the
// action ids it locked. Call once, before accepting any traffic.
func (s *Store) RecoverStartup() ([]string, error) {
	rows, err := s.db.Query(
		`SELECT action_id FROM action_state WHERE mandate_id = ? AND state = 'RESERVED'`,
		s.mandateID)
	if err != nil {
		return nil, fmt.Errorf("storage: recover query: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if _, err := s.db.Exec(
		`UPDATE action_state SET state = 'IN_DOUBT', updated_at = ?
		 WHERE mandate_id = ? AND state = 'RESERVED'`,
		time.Now().UTC().Format(time.RFC3339Nano), s.mandateID); err != nil {
		return nil, fmt.Errorf("storage: recover update: %w", err)
	}
	return ids, nil
}

// Reserve persists a reservation. It must succeed BEFORE anything is written to
// the child's stdin, so a crash mid-flight leaves a recoverable row rather than
// a silently released authorization.
func (s *Store) Reserve(actionID, receipt string, amountPaise int64) error {
	_, err := s.db.Exec(
		`INSERT INTO action_state (mandate_id, action_id, receipt, state, amount_paise, updated_at)
		 VALUES (?, ?, ?, 'RESERVED', ?, ?)
		 ON CONFLICT(mandate_id, action_id) DO UPDATE SET
		   state = 'RESERVED', amount_paise = excluded.amount_paise, updated_at = excluded.updated_at
		 WHERE action_state.state = 'AVAILABLE'`,
		s.mandateID, actionID, receipt, amountPaise,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("storage: reserve %s: %w", actionID, err)
	}
	return nil
}

// SetState records a terminal or recovered state.
func (s *Store) SetState(actionID, state string) error {
	_, err := s.db.Exec(
		`UPDATE action_state SET state = ?, updated_at = ? WHERE mandate_id = ? AND action_id = ?`,
		state, time.Now().UTC().Format(time.RFC3339Nano), s.mandateID, actionID)
	if err != nil {
		return fmt.Errorf("storage: set state %s=%s: %w", actionID, state, err)
	}
	return nil
}

// Snapshot is the durable view used to rebuild in-memory state at startup.
type Snapshot struct {
	States  map[string]string
	Amounts map[string]int64
}

func (s *Store) Snapshot() (Snapshot, error) {
	out := Snapshot{States: map[string]string{}, Amounts: map[string]int64{}}
	rows, err := s.db.Query(
		`SELECT action_id, state, amount_paise FROM action_state WHERE mandate_id = ?`,
		s.mandateID)
	if err != nil {
		return out, fmt.Errorf("storage: snapshot: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, st string
		var amt int64
		if err := rows.Scan(&id, &st, &amt); err != nil {
			return out, err
		}
		out.States[id] = st
		out.Amounts[id] = amt
	}
	return out, rows.Err()
}

// ResolveInDoubt atomically writes the operator's decision and its audit record.
// Both land or neither does: an unaudited resolution of a possibly-completed
// refund is not an acceptable outcome.
func (s *Store) ResolveInDoubt(actionID, toState, actor, reason string, refundLanded bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: begin: %w", err)
	}
	defer tx.Rollback()

	var current string
	if err := tx.QueryRow(
		`SELECT state FROM action_state WHERE mandate_id = ? AND action_id = ?`,
		s.mandateID, actionID).Scan(&current); err != nil {
		return fmt.Errorf("storage: resolve lookup %s: %w", actionID, err)
	}
	if current != "IN_DOUBT" {
		return fmt.Errorf("storage: %s is %s, not IN_DOUBT", actionID, current)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(
		`UPDATE action_state SET state = ?, updated_at = ? WHERE mandate_id = ? AND action_id = ?`,
		toState, now, s.mandateID, actionID); err != nil {
		return err
	}
	landed := 0
	if refundLanded {
		landed = 1
	}
	if _, err := tx.Exec(
		`INSERT INTO audit (at, actor, action_id, mandate_id, from_state, to_state, refund_landed, reason)
		 VALUES (?, ?, ?, ?, 'IN_DOUBT', ?, ?, ?)`,
		now, actor, actionID, s.mandateID, toState, landed, reason); err != nil {
		return err
	}
	return tx.Commit()
}

// AuditCount is used by tests to assert an audit record accompanied a decision.
func (s *Store) AuditCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE mandate_id = ?`, s.mandateID).Scan(&n)
	return n, err
}
