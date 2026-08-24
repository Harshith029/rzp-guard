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

var (
	ErrReceiptExists = errors.New("receipt already issued")
	// ErrNotOwner means another guard process already owns this state file.
	ErrNotOwner = errors.New("state file is owned by another guard process")
	// ErrNoRowChanged means an expected-state write matched nothing.
	ErrNoRowChanged = errors.New("no row changed: action was not in the expected state")
)

func nowUnixNano() int64 { return time.Now().UTC().UnixNano() }

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

-- Written at startup purely to force the EXCLUSIVE lock to be acquired.
CREATE TABLE IF NOT EXISTS owner (
  id          INTEGER PRIMARY KEY CHECK (id = 1),
  mandate_id  TEXT NOT NULL,
  acquired_at TEXT NOT NULL
);

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

-- Rate-limit window. Persisted because an in-memory limiter resets on restart,
-- which would let a crash-loop bypass max_calls_per_minute entirely.
CREATE TABLE IF NOT EXISTS call_log (
  mandate_id   TEXT    NOT NULL,
  at_unix_nano INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS call_log_window ON call_log (mandate_id, at_unix_nano);

-- SHA-256 of the operator token, written at guard launch. The expected value
-- must come from a different source than the value being checked.
CREATE TABLE IF NOT EXISTS operator_auth (
  id           INTEGER PRIMARY KEY CHECK (id = 1),
  token_sha256 TEXT NOT NULL,
  set_at       TEXT NOT NULL
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

	// Single-instance ownership. Two guard processes over one state file would
	// each restore their own in-memory ledger and check the cumulative cap
	// locally, so between them they could reserve past the mandate cap.
	// SetMaxOpenConns(1) only serializes writers WITHIN one process.
	//
	// EXCLUSIVE locking mode makes this connection retain the database lock for
	// its lifetime, so a second process fails fast instead of silently sharing.
	if _, err := db.Exec(`PRAGMA locking_mode = EXCLUSIVE`); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: locking mode: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: %w (%v)", ErrNotOwner, err)
	}
	// Force the exclusive lock to be taken now rather than on first write, so
	// ownership is decided at startup and not mid-refund.
	if _, err := db.Exec(
		`INSERT INTO owner (id, mandate_id, acquired_at) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET mandate_id = excluded.mandate_id,
		                               acquired_at = excluded.acquired_at`,
		mandateID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: %w (%v)", ErrNotOwner, err)
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
	res, err := s.db.Exec(
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
	// The ON CONFLICT guard silently changes NOTHING when the action is already
	// RESERVED / COMMITTED / IN_DOUBT. Without this check the caller would treat
	// a no-op as success and mark the action reserved in memory anyway.
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: reserve %s: rows affected: %w", actionID, err)
	}
	if n != 1 {
		return fmt.Errorf("storage: reserve %s: %w (%d rows)", actionID, ErrNoRowChanged, n)
	}
	return nil
}

// SetState performs an EXPECTED-STATE transition.
//
// Filtering on the previous state is what makes this a transition rather than
// an overwrite. Matching on (mandate, action) alone would let a stale caller
// move a COMMITTED action back to AVAILABLE, and RowsAffected == 1 would
// cheerfully report success -- it proves the row exists, not that the intended
// transition is the one that happened.
func (s *Store) SetState(actionID, from, to string) error {
	res, err := s.db.Exec(
		`UPDATE action_state SET state = ?, updated_at = ?
		 WHERE mandate_id = ? AND action_id = ? AND state = ?`,
		to, time.Now().UTC().Format(time.RFC3339Nano), s.mandateID, actionID, from)
	if err != nil {
		return fmt.Errorf("storage: set state %s %s->%s: %w", actionID, from, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("storage: set state %s: rows affected: %w", actionID, err)
	}
	if n != 1 {
		return fmt.Errorf("storage: set state %s %s->%s: %w (%d rows; the action was "+
			"not in the expected state)", actionID, from, to, ErrNoRowChanged, n)
	}
	return nil
}

// RecordCall appends a forwarded call to the durable rate window.
func (s *Store) RecordCall(atUnixNano int64) error {
	if _, err := s.db.Exec(
		`INSERT INTO call_log (mandate_id, at_unix_nano) VALUES (?, ?)`,
		s.mandateID, atUnixNano); err != nil {
		return fmt.Errorf("storage: record call: %w", err)
	}
	return nil
}

// RecentCalls returns forwarded-call timestamps at or after cutoff, and prunes
// everything older so the table cannot grow without bound.
func (s *Store) RecentCalls(cutoffUnixNano int64) ([]int64, error) {
	if _, err := s.db.Exec(
		`DELETE FROM call_log WHERE mandate_id = ? AND at_unix_nano < ?`,
		s.mandateID, cutoffUnixNano); err != nil {
		return nil, fmt.Errorf("storage: prune call log: %w", err)
	}
	rows, err := s.db.Query(
		`SELECT at_unix_nano FROM call_log WHERE mandate_id = ? AND at_unix_nano >= ?
		 ORDER BY at_unix_nano`, s.mandateID, cutoffUnixNano)
	if err != nil {
		return nil, fmt.Errorf("storage: recent calls: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
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

// ActionRow is a durable action as an operator sees it.
type ActionRow struct {
	ActionID    string
	PaymentID   string
	Receipt     string
	State       string
	AmountPaise int64
	UpdatedAt   string
}

// ActionsInState lists actions in a given state, for the operator console.
//
// payment_id is not stored on action_state (the mandate holds it), so the
// operator tool resolves it from the mandate it is given. Receipt is the field
// an operator actually needs: it is the correlation key to look the refund up
// in the Razorpay dashboard.
func (s *Store) ActionsInState(state string) ([]ActionRow, error) {
	rows, err := s.db.Query(
		`SELECT action_id, receipt, state, amount_paise, updated_at
		 FROM action_state WHERE mandate_id = ? AND state = ? ORDER BY updated_at`,
		s.mandateID, state)
	if err != nil {
		return nil, fmt.Errorf("storage: list %s: %w", state, err)
	}
	defer rows.Close()
	var out []ActionRow
	for rows.Next() {
		var a ActionRow
		if err := rows.Scan(&a.ActionID, &a.Receipt, &a.State, &a.AmountPaise, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AuditRow is one operator-initiated transition.
type AuditRow struct {
	At           string
	Actor        string
	ActionID     string
	From, To     string
	RefundLanded bool
	Reason       string
}

// AuditTrail returns every operator resolution, oldest first.
func (s *Store) AuditTrail() ([]AuditRow, error) {
	rows, err := s.db.Query(
		`SELECT at, actor, action_id, from_state, to_state, refund_landed, reason
		 FROM audit WHERE mandate_id = ? ORDER BY id`, s.mandateID)
	if err != nil {
		return nil, fmt.Errorf("storage: audit: %w", err)
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		var a AuditRow
		var landed int
		if err := rows.Scan(&a.At, &a.Actor, &a.ActionID, &a.From, &a.To, &landed, &a.Reason); err != nil {
			return nil, err
		}
		a.RefundLanded = landed == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetOperatorTokenHash records the SHA-256 of the operator token, at guard
// launch, in the state file.
//
// The expected value MUST come from a different source than the value being
// checked. An earlier operator CLI read the token from the environment and then
// constructed the console with that same token, so the comparison was against
// itself and any token of sufficient length was accepted. Storing the hash at
// guard launch makes the check real: whoever starts the guard sets it, and the
// operator must present the matching secret later.
func (s *Store) SetOperatorTokenHash(hash string) error {
	_, err := s.db.Exec(
		`INSERT INTO operator_auth (id, token_sha256, set_at) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET token_sha256 = excluded.token_sha256,
		                               set_at = excluded.set_at`,
		hash, time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("storage: set operator token: %w", err)
	}
	return nil
}

// OperatorTokenHash returns the configured hash, or false if none was set.
func (s *Store) OperatorTokenHash() (string, bool, error) {
	var h string
	err := s.db.QueryRow(`SELECT token_sha256 FROM operator_auth WHERE id = 1`).Scan(&h)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("storage: operator token: %w", err)
	}
	return h, h != "", nil
}
