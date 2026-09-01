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
	"strings"
	"time"

	_ "modernc.org/sqlite"

	// For lifecycle.Reservation only. The interface lives with its CONSUMER
	// (lifecycle.Persister), so the type in its signature does too, and the
	// implementer imports it. Acyclic: lifecycle does not import storage.
	"github.com/harshith/rzp-guard/internal/lifecycle"
)

// schemaVersion is the version this build writes and understands.
//
// Bump it ONLY alongside an actual schema change, and only together with a
// decision about files at the previous version -- migrate them, or refuse them
// with instructions. Bumping it without that decision converts a silent
// misread into a hard outage: better, but still bad.
const schemaVersion = 2

// callLogRetention is how much history RecordCall keeps. The rate limiter
// only ever asks about the last 60 seconds; the surplus is deliberate slack so
// pruning can never discard a row the limiter is still counting.
const callLogRetention = time.Hour

var (
	// ErrSchemaVersion means the file was written by a build with a different
	// schema. Refused rather than guessed at.
	ErrSchemaVersion = errors.New("state file schema version is not supported by this build")

	ErrReceiptExists = errors.New("receipt already issued")
	// ErrNotOwner means another guard process already owns this state file.
	ErrNotOwner = errors.New("state file is owned by another guard process")
	// ErrNoRowChanged means an expected-state write matched nothing.
	ErrNoRowChanged = errors.New("no row changed: action was not in the expected state")
	// ErrMandateMismatch means this state file already belongs to a different
	// mandate that still has unresolved actions.
	ErrMandateMismatch = errors.New("state file belongs to another mandate with unresolved actions")

	// errLockContended is INTERNAL and never escapes Open. It marks the one
	// failure worth retrying -- another connection held the exclusive lock at this
	// instant -- as opposed to a decision this build has already reached about the
	// file (wrong schema version, another mandate's unresolved actions), which
	// retrying could only delay.
	errLockContended = errors.New("storage: exclusive lock contended")
)

// lockAcquireDeadline bounds how long Open waits for the exclusive lock. A var
// so tests can shorten it; nothing outside this package can set it.
var lockAcquireDeadline = 2 * time.Second

// lockContended reports whether err is SQLite refusing because someone else
// holds the lock right now. Matched on the driver's text because
// modernc.org/sqlite returns a plain error here, not a typed one. A false
// negative costs a retry that would have succeeded; it never yields a wrong
// owner, because the lock itself is what decides that.
func lockContended(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "database is locked") ||
		strings.Contains(s, "sqlite_busy") ||
		strings.Contains(s, "database table is locked") ||
		strings.Contains(s, "sqlite_protocol")
}

func nowUnixNano() int64 { return time.Now().UTC().UnixNano() }

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

-- Durability is the entire point of this package, so the setting that provides
-- it is stated rather than inherited.
--
-- MEASURED (internal/storage/bench_test.go, on this hardware):
--
--   synchronous=FULL     ~5.6 ms per commit
--   synchronous=NORMAL   ~34  us per commit
--
-- (500 iterations x 3 runs. An earlier 200-iteration measurement put these
-- at 10.8 ms and 23 us -- roughly 2x high, on a workload dominated by disk
-- flushes where small samples do not settle.)
--
-- A ~165x difference, and it is the price of the guarantee the design rests on:
-- a reservation must be on disk BEFORE any byte is forwarded to the child. With
-- NORMAL, a WAL commit is not fsynced, so a power loss can discard the most
-- recent transactions. Lose a reservation whose refund already reached Razorpay
-- and the action returns as AVAILABLE at restart -- a replay, and the exact
-- fail-open case the lifecycle exists to prevent. NORMAL is safe against a
-- process or OS crash; it is not safe against power loss, and money is exactly
-- the workload where that distinction is worth 10 ms.
--
-- FULL was already in force as the driver's default. That is why this line
-- exists: a default is not a decision, and a driver upgrade or a DSN change
-- could have quietly moved it to NORMAL. The result would have looked like a
-- 165x performance win while removing the guarantee. TestSynchronousIsFull
-- fails if this is ever weakened.
PRAGMA synchronous=FULL;

-- Records which mandate owns this state file. READ on every Open: every query
-- in this package is scoped by mandate_id, so opening a populated file under a
-- different mandate would silently hide the previous mandate's unresolved
-- actions instead of surfacing them (FAILURES.md F22).
--
-- It does NOT force the exclusive lock. The schema statement above already
-- took it, which a mutation of the insert proves.
CREATE TABLE IF NOT EXISTS owner (
  id          INTEGER PRIMARY KEY CHECK (id = 1),
  mandate_id  TEXT NOT NULL,
  acquired_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS action_state (
  mandate_id   TEXT    NOT NULL,
  action_id    TEXT    NOT NULL,
  -- The receipt of the call that most recently reserved this action.
  --
  -- NOT UNIQUE any more, and that is the point of schema v2. One forwarded
  -- refund may now consume SEVERAL actions at once -- a merchant who authorized
  -- 18500 and 19000 separately should not have an agent's single 37500 call
  -- refused -- and every action it consumed must carry the SAME receipt,
  -- because that receipt is the string an operator searches Razorpay for when
  -- resolving. Rows showing different receipts for one real refund would be
  -- actively misleading during an incident.
  --
  -- The uniqueness guarantee did not go away, it moved to where it belongs:
  -- call_receipt below, one row per forwarded call. The receipt is a TRUNCATED
  -- 48-bit hash, so uniqueness has to be enforced rather than assumed.
  receipt      TEXT    NOT NULL,
  state        TEXT    NOT NULL,
  amount_paise INTEGER NOT NULL,
  updated_at   TEXT    NOT NULL,
  PRIMARY KEY (mandate_id, action_id)
);

-- Rate-limit window. Persisted because an in-memory limiter resets on restart,
-- which would let a crash-loop bypass max_calls_per_minute entirely.
-- One row per forwarded refund call. This is where receipt uniqueness lives.
--
-- Previously it was a UNIQUE column on action_state, which silently assumed one
-- call consumes exactly one action. That assumption is what made a merchant's
-- two separate authorizations un-combinable.
CREATE TABLE IF NOT EXISTS call_receipt (
  receipt    TEXT PRIMARY KEY,
  mandate_id TEXT NOT NULL,
  issued_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS call_log (
  mandate_id   TEXT    NOT NULL,
  at_unix_nano INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS call_log_window ON call_log (mandate_id, at_unix_nano);

-- Salted Argon2id verifier for the operator token.
--
-- The GUARD NEVER WRITES THIS. Only "rzp-guard-operator init" (once) and
-- "rotate" (authenticated with the current token) may. An earlier design had
-- the guard rewrite the credential on every start, so anyone able to relaunch
-- the process could install their own token and resolve locked refunds.
CREATE TABLE IF NOT EXISTS operator_verifier (
  id       INTEGER PRIMARY KEY CHECK (id = 1),
  verifier TEXT NOT NULL,
  set_at   TEXT NOT NULL,
  rotations INTEGER NOT NULL DEFAULT 0,
  -- 1 when the verifier was created by a TEST FIXTURE whose token was
  -- discarded. Such a state file satisfies "a credential is configured" while
  -- being impossible for any human to recover, so the production guard must
  -- refuse it. Without this column, "configured" is not evidence that recovery
  -- is possible -- it only proves a row exists.
  ephemeral INTEGER NOT NULL DEFAULT 0
);

-- What version of this schema the file was created with.
--
-- There is no migration framework here and this does not add one. What it adds
-- is the ability to FAIL LOUDLY instead of silently misreading a file, and that
-- is the half that cannot be retrofitted: once state files exist in the wild, a
-- file with no version stamp is indistinguishable from a v1 file, and the first
-- schema change has no safe way to tell them apart.
--
-- Added while every state file in existence is still disposable. That is the
-- only moment it is free.
CREATE TABLE IF NOT EXISTS schema_meta (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  version    INTEGER NOT NULL,
  created_at TEXT    NOT NULL
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

// Open prepares the database and applies the schema, waiting out lock
// contention for a bounded time.
//
// WHY A RETRY IS NEEDED AT ALL. The exclusive lock is taken by the schema
// statement in openOnce, not by the PRAGMA above it. Under
// `locking_mode = EXCLUSIVE` a connection that has read the database KEEPS its
// SHARED lock for the connection's whole life. So N processes starting together
// can each hold SHARED, none can upgrade to EXCLUSIVE, and every one of them
// fails -- not one owner and N-1 refusals, but ZERO owners. CI observed exactly
// that on ca1e4c1 ("0 of 16 concurrent opens succeeded"), and it reproduced 1
// run in 12 under --cpus=0.5 with -race. A single attempt cannot recover from
// it, because the losers are holding the very lock they are waiting for.
//
// Closing the handle releases it, so each attempt here uses a FRESH connection
// and the losers get out of each other's way. Jittered backoff stops them
// colliding again in lockstep.
//
// WHY NOT A BUSY TIMEOUT. sqlite3_busy_timeout sleeps and retries on the SAME
// connection, so in this deadlock every waiter would sleep while holding the
// SHARED lock that blocks everyone: it cannot break it. It would also apply to
// every later statement, turning a fast refusal into an unbounded stall
// mid-refund. This retry is confined to startup and has a deadline.
//
// THE DIRECTION IS STILL FAIL CLOSED. Nothing has been forwarded when Open
// runs. Exhausting the deadline returns ErrNotOwner and the guard does not
// start: unavailable, never two owners over one ledger.
func Open(path, mandateID string) (*Store, error) {
	return openWithDeadline(path, mandateID, lockAcquireDeadline)
}

func openWithDeadline(path, mandateID string, deadline time.Duration) (*Store, error) {
	giveUp := time.Now().Add(deadline)
	backoff := 2 * time.Millisecond
	for attempt := 1; ; attempt++ {
		st, err := openOnce(path, mandateID)
		if err == nil {
			return st, nil
		}
		if !errors.Is(err, errLockContended) {
			return nil, err
		}
		if !time.Now().Before(giveUp) {
			return nil, fmt.Errorf(
				"storage: %w: could not acquire the exclusive lock on %s within %s "+
					"(%d attempts). Another guard process holds it, so this one "+
					"refuses to start rather than run a second ledger over the "+
					"same file", ErrNotOwner, path, deadline, attempt)
		}
		// Jitter from the clock rather than math/rand: this package should not
		// carry a seeded global, and the only requirement is that two contenders
		// do not wake together.
		wait := backoff + time.Duration(time.Now().UnixNano()%int64(backoff))
		if rem := time.Until(giveUp); wait > rem {
			wait = rem
		}
		time.Sleep(wait)
		if backoff < 40*time.Millisecond {
			backoff *= 2
		}
	}
}

// openOnce is one attempt. Every failure path closes the handle, which is what
// releases the SHARED lock and lets a competing attempt make progress.
func openOnce(path, mandateID string) (*Store, error) {
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
		if lockContended(err) {
			return nil, fmt.Errorf("%w (%v)", errLockContended, err)
		}
		return nil, fmt.Errorf("storage: %w (%v)", ErrNotOwner, err)
	}
	// Before ANY read of the tables below. A file this build cannot correctly
	// interpret must be refused while it is still untouched, not after its
	// contents have been half-understood.
	if err := checkSchemaVersion(db); err != nil {
		db.Close()
		return nil, err
	}
	// Which mandate does this state file already belong to?
	//
	// Every query in this package is scoped by mandate_id, including recovery.
	// So opening a populated state file under a DIFFERENT mandate does not fail
	// -- it silently hides everything the previous mandate left behind. A refund
	// that was RESERVED when the process died would never be promoted to
	// IN_DOUBT, never appear in the operator console, and never be resolvable,
	// while the money it represents may already have moved.
	//
	// That is not an exotic path: -state defaults to rzp-guard.db for both
	// binaries while the mandate is a separate file supplied per run, and this
	// repository alone carries 18 distinct mandate ids. Running any two of them
	// without passing -state is enough.
	//
	// Refuse. The alternative -- recovering across mandates -- would surface one
	// mandate's actions inside another's ledger and cap arithmetic, which is a
	// worse trade for a component whose whole job is to keep authorization
	// scoped.
	var previous string
	switch err := db.QueryRow(`SELECT mandate_id FROM owner WHERE id = 1`).Scan(&previous); {
	case errors.Is(err, sql.ErrNoRows):
		// Fresh state file; this mandate takes it below.
	case err != nil:
		db.Close()
		return nil, fmt.Errorf("storage: read state file owner: %w", err)
	case previous != mandateID:
		stranded, serr := unresolvedFor(db, previous)
		if serr != nil {
			db.Close()
			return nil, fmt.Errorf("storage: check for stranded actions: %w", serr)
		}
		if len(stranded) > 0 {
			db.Close()
			return nil, fmt.Errorf(
				"storage: %w: %s holds %d unresolved action(s) [%s]; opening it as %s "+
					"would hide them permanently, and a refund that was in flight may "+
					"already have landed. Re-open with that mandate and resolve them "+
					"(rzp-guard-operator -mandate <that mandate> list), or point -state "+
					"at a different file",
				ErrMandateMismatch, previous, len(stranded),
				strings.Join(stranded, ", "), mandateID)
		}
		// The previous mandate left nothing unresolved, so reuse is safe.
	}

	// Record the owner. This does NOT force the exclusive lock -- the schema
	// statement above already took it, which a mutation of this line proves. It
	// is here so the check above has something to read on the next open.
	if _, err := db.Exec(
		`INSERT INTO owner (id, mandate_id, acquired_at) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET mandate_id = excluded.mandate_id,
		                               acquired_at = excluded.acquired_at`,
		mandateID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		db.Close()
		if lockContended(err) {
			return nil, fmt.Errorf("%w (%v)", errLockContended, err)
		}
		return nil, fmt.Errorf("storage: %w (%v)", ErrNotOwner, err)
	}
	return &Store{db: db, mandateID: mandateID}, nil
}

// unresolvedFor lists the actions of some other mandate that still need a
// human: RESERVED (mid-flight when the process died) or IN_DOUBT (recovered and
// waiting). COMMITTED and terminal rows are finished business and do not block
// reuse of the state file.
func unresolvedFor(db *sql.DB, mandateID string) ([]string, error) {
	rows, err := db.Query(`SELECT action_id FROM action_state
		 WHERE mandate_id = ? AND state IN ('RESERVED', 'IN_DOUBT')
		 ORDER BY action_id`, mandateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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
// Reserve durably claims one action for one call.
func (s *Store) Reserve(actionID, receipt string, amountPaise int64) error {
	return s.ReserveMany(receipt, []lifecycle.Reservation{{ActionID: actionID, AmountPaise: amountPaise}})
}

// ReserveMany durably claims EVERY action a single forwarded call consumes,
// atomically, before any byte reaches the child.
//
// One call may consume several actions because a merchant who authorized 18500
// and 19000 separately has authorized 37500 in total, and an agent that issues
// it as one refund is asking for exactly what was granted. Refusing that was
// measured as three of arm B's nine false blocks (study/RESULTS-armB.md).
//
// ATOMICITY IS THE WHOLE POINT. Reserving two of three actions and failing on
// the third would leave budget encumbered against a refund that never gets
// forwarded, and the partial reservation would block the agent's legitimate
// retry. One transaction, one fsync, all or nothing.
//
// The receipt is inserted into call_receipt FIRST. That table is where receipt
// uniqueness now lives (schema v2): it is one row per forwarded call, which is
// the level the guarantee actually belongs at. Every action row then carries
// the same receipt, because that string is what an operator searches Razorpay
// for -- rows disagreeing about it would mislead exactly when it matters.
func (s *Store) ReserveMany(receipt string, rs []lifecycle.Reservation) error {
	if len(rs) == 0 {
		return errors.New("storage: reserve: no actions given")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: reserve: %w", err)
	}
	defer tx.Rollback()

	for _, r := range rs {
		res, err := tx.Exec(
			`INSERT INTO action_state (mandate_id, action_id, receipt, state, amount_paise, updated_at)
			 VALUES (?, ?, ?, 'RESERVED', ?, ?)
			 ON CONFLICT(mandate_id, action_id) DO UPDATE SET
			   state = 'RESERVED', receipt = excluded.receipt,
			   amount_paise = excluded.amount_paise, updated_at = excluded.updated_at
			 WHERE action_state.state = 'AVAILABLE'`,
			s.mandateID, r.ActionID, receipt, r.AmountPaise, now)
		if err != nil {
			return fmt.Errorf("storage: reserve %s: %w", r.ActionID, err)
		}
		// The ON CONFLICT guard silently changes NOTHING when the action is
		// already RESERVED / COMMITTED / IN_DOUBT. Without this check the caller
		// would treat a no-op as success and mark the action reserved in memory
		// anyway.
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("storage: reserve %s: rows affected: %w", r.ActionID, err)
		}
		if n != 1 {
			return fmt.Errorf("storage: reserve %s: %w (%d rows)", r.ActionID, ErrNoRowChanged, n)
		}
	}

	// Claim the receipt LAST.
	//
	// Order matters for the error a caller sees. An action that is already
	// RESERVED or COMMITTED is the more specific and more actionable failure --
	// it is a replay, and ErrNoRowChanged is what the policy maps to
	// ACTION_CONSUMED. Claiming the receipt first meant a replay of the very
	// same call reported "receipt already issued" instead, which is true but
	// less useful and changed a rule the decision log has always carried.
	//
	// A PRIMARY KEY collision here is a genuine one: the receipt is a 48-bit
	// truncated hash, so uniqueness must be enforced rather than assumed. It
	// fails the whole transaction, so nothing is reserved and nothing is
	// forwarded -- a collision can only refuse a refund, never duplicate one.
	if _, err := tx.Exec(
		`INSERT INTO call_receipt (receipt, mandate_id, issued_at) VALUES (?, ?, ?)`,
		receipt, s.mandateID, now); err != nil {
		// Classify INSIDE the transaction. s.receiptTaken() would ask the pool
		// for a connection while tx holds the only one (SetMaxOpenConns(1)) and
		// deadlock -- which is exactly what it did, until the tests hung.
		var n int
		if qerr := tx.QueryRow(
			`SELECT COUNT(*) FROM call_receipt WHERE receipt = ?`, receipt).Scan(&n); qerr == nil && n > 0 {
			return fmt.Errorf("storage: reserve: %w (receipt %s)", ErrReceiptExists, receipt)
		}
		return fmt.Errorf("storage: reserve: claim receipt: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: reserve: %w", err)
	}
	return nil
}

// receiptTaken reports whether some row already holds this receipt. Used only
// to classify a failed insert, never to gate one -- the UNIQUE constraint is
// the actual guard.
// checkSchemaVersion stamps a new file and refuses one this build cannot read.
//
// A file at a HIGHER version was written by a newer guard that may track state
// in columns this build ignores. Opening it would mean acting on a partial view
// of money another process considers authoritative, so it is refused rather
// than tolerated. A LOWER version means a schema change shipped without a
// migration -- a defect in this repository, not in the operator's file, and the
// message says so plainly, because the instinct on meeting it is to delete the
// file and that file may hold IN_DOUBT actions whose refunds already moved.
//
// INSERT OR IGNORE, not INSERT: a file created before this table existed gets
// stamped v1 on first open, which is correct -- the schema it holds IS v1.
func checkSchemaVersion(db *sql.DB) error {
	// An UNSTAMPED file is not necessarily a new one.
	//
	// Files created before schema_meta existed carry the v1 layout, and
	// stamping them with the current version would skip the migration they
	// actually need -- adopting a v1 file as v2 and then reading it with v2
	// assumptions. So the version is INFERRED FROM STRUCTURE when the stamp is
	// missing: v1's action_state declares receipt UNIQUE and v2's does not.
	//
	// Reading the DDL is deliberate. It asks the file what it IS rather than
	// trusting what it is labelled, which is the whole reason this check exists.
	initial := schemaVersion
	var stamped int
	switch err := db.QueryRow(`SELECT version FROM schema_meta WHERE id = 1`).
		Scan(&stamped); {
	case err == nil:
		initial = stamped
	case errors.Is(err, sql.ErrNoRows):
		var ddl string
		if derr := db.QueryRow(
			`SELECT sql FROM sqlite_master WHERE type='table' AND name='action_state'`).
			Scan(&ddl); derr == nil && strings.Contains(ddl, "UNIQUE") {
			initial = 1
		}
	default:
		return fmt.Errorf("storage: read schema version: %w", err)
	}

	if _, err := db.Exec(
		`INSERT OR IGNORE INTO schema_meta (id, version, created_at) VALUES (1, ?, ?)`,
		initial, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("storage: stamp schema version: %w", err)
	}

	var found int
	if err := db.QueryRow(`SELECT version FROM schema_meta WHERE id = 1`).
		Scan(&found); err != nil {
		return fmt.Errorf("storage: read schema version: %w", err)
	}
	switch {
	case found == schemaVersion:
		return nil
	case found == 1 && schemaVersion == 2:
		// The first real migration, and the reason the version stamp was added
		// while state files were still disposable.
		return migrateV1toV2(db)
	case found > schemaVersion:
		return fmt.Errorf("%w: file is version %d, this build understands %d. "+
			"It was written by a NEWER rzp-guard which may track state in columns "+
			"this build cannot see, so opening it would act on a partial view of "+
			"money that another process considers authoritative. Upgrade the "+
			"binary rather than downgrading the file",
			ErrSchemaVersion, found, schemaVersion)
	default:
		return fmt.Errorf("%w: file is version %d, this build expects %d. No "+
			"migration exists for that step, which is a defect in rzp-guard and "+
			"not in your state file. Do NOT delete it: it may hold IN_DOUBT "+
			"actions whose refunds already moved money",
			ErrSchemaVersion, found, schemaVersion)
	}
}

// migrateV1toV2 drops the UNIQUE constraint on action_state.receipt and moves
// that guarantee into call_receipt.
//
// SQLite cannot drop a constraint in place, so the table is rebuilt. Everything
// happens in ONE transaction: a half-migrated state file holding refund
// reservations is worse than one that refuses to open.
//
// Existing receipts are backfilled into call_receipt so the uniqueness
// guarantee is continuous across the migration rather than restarting empty.
func migrateV1toV2(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("storage: migrate v1->v2: %w", err)
	}
	defer tx.Rollback()

	stmts := []string{
		`ALTER TABLE action_state RENAME TO action_state_v1`,
		`CREATE TABLE action_state (
		   mandate_id   TEXT    NOT NULL,
		   action_id    TEXT    NOT NULL,
		   receipt      TEXT    NOT NULL,
		   state        TEXT    NOT NULL,
		   amount_paise INTEGER NOT NULL,
		   updated_at   TEXT    NOT NULL,
		   PRIMARY KEY (mandate_id, action_id)
		 )`,
		`INSERT INTO action_state
		   (mandate_id, action_id, receipt, state, amount_paise, updated_at)
		 SELECT mandate_id, action_id, receipt, state, amount_paise, updated_at
		 FROM action_state_v1`,
		// Continuity: every receipt already issued stays reserved, so a
		// migrated file cannot re-issue one it has used before.
		`INSERT OR IGNORE INTO call_receipt (receipt, mandate_id, issued_at)
		 SELECT receipt, mandate_id, updated_at FROM action_state_v1`,
		`DROP TABLE action_state_v1`,
		`UPDATE schema_meta SET version = 2 WHERE id = 1`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("storage: migrate v1->v2: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: migrate v1->v2: %w", err)
	}
	return nil
}

func (s *Store) receiptTaken(receipt string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM call_receipt WHERE receipt = ?`, receipt).Scan(&n)
	return n > 0, err
}

// SetState performs an EXPECTED-STATE transition.
//
// Filtering on the previous state is what makes this a transition rather than
// an overwrite. Matching on (mandate, action) alone would let a stale caller
// move a COMMITTED action back to AVAILABLE, and RowsAffected == 1 would
// cheerfully report success -- it proves the row exists, not that the intended
// transition is the one that happened.
// SetStateMany moves every action of one call together, in one transaction.
//
// A combined refund either commits as a whole or does not: leaving one action
// COMMITTED and another RESERVED would mean the ledger disagrees with itself
// about a single refund that either happened or did not.
func (s *Store) SetStateMany(actionIDs []string, from, to string) error {
	if len(actionIDs) == 0 {
		return errors.New("storage: set state: no actions given")
	}
	if len(actionIDs) == 1 {
		return s.SetState(actionIDs[0], from, to)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: set state: %w", err)
	}
	defer tx.Rollback()

	for _, id := range actionIDs {
		res, err := tx.Exec(
			`UPDATE action_state SET state = ?, updated_at = ?
			 WHERE mandate_id = ? AND action_id = ? AND state = ?`,
			to, now, s.mandateID, id, from)
		if err != nil {
			return fmt.Errorf("storage: set state %s %s->%s: %w", id, from, to, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("storage: set state %s: rows affected: %w", id, err)
		}
		if n != 1 {
			return fmt.Errorf("storage: set state %s %s->%s: %w (%d rows; the action "+
				"was not in the expected state)", id, from, to, ErrNoRowChanged, n)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: set state: %w", err)
	}
	return nil
}

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
// RecordCall appends to the rate window and drops rows that have fallen out
// of it, in ONE transaction.
//
// Pruning used to happen only in RecentCalls, whose sole caller is the startup
// restore. So a long-running guard appended a row per forwarded call and never
// removed one until the next restart -- bounded only by uptime.
//
// The obvious fix, a second Exec, would have been the wrong one: at
// synchronous=FULL every commit is an fsync costing ~11ms (F23), so a separate
// DELETE would DOUBLE the cost of every authorized refund to buy tidiness.
// Both statements share one transaction and therefore one fsync, so the table
// is bounded at no measurable cost.
//
// The window is generous on purpose: the limiter cares about 60 seconds, and
// this keeps an hour. Pruning to exactly the window would race a clock that
// moved backwards and silently discard slots the limiter still counts.
func (s *Store) RecordCall(atUnixNano int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("storage: record call: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO call_log (mandate_id, at_unix_nano) VALUES (?, ?)`,
		s.mandateID, atUnixNano); err != nil {
		return fmt.Errorf("storage: record call: %w", err)
	}
	if _, err := tx.Exec(
		`DELETE FROM call_log WHERE mandate_id = ? AND at_unix_nano < ?`,
		s.mandateID, atUnixNano-int64(callLogRetention)); err != nil {
		return fmt.Errorf("storage: prune call log: %w", err)
	}
	if err := tx.Commit(); err != nil {
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

// OperatorVerifier returns the stored verifier, whether one is set, and whether
// it is an EPHEMERAL fixture credential that no human can present.
func (s *Store) OperatorVerifier() (verifier string, configured, ephemeral bool, err error) {
	var eph int
	err = s.db.QueryRow(
		`SELECT verifier, ephemeral FROM operator_verifier WHERE id = 1`).Scan(&verifier, &eph)
	if err == sql.ErrNoRows {
		return "", false, false, nil
	}
	if err != nil {
		return "", false, false, fmt.Errorf("storage: operator verifier: %w", err)
	}
	return verifier, verifier != "", eph == 1, nil
}

// InitOperatorVerifier writes the verifier ONLY if none exists.
//
// The INSERT has no upsert clause on purpose: a second init cannot silently
// replace an existing credential, which is exactly how the restart bypass
// worked. Rotation is a separate, authenticated operation.
func (s *Store) InitOperatorVerifier(verifier string) error {
	return s.initVerifier(verifier, false)
}

// InitEphemeralVerifier records a fixture credential whose token was discarded.
// The production guard refuses a state file marked this way.
func (s *Store) InitEphemeralVerifier(verifier string) error {
	return s.initVerifier(verifier, true)
}

func (s *Store) initVerifier(verifier string, ephemeral bool) error {
	eph := 0
	if ephemeral {
		eph = 1
	}
	res, err := s.db.Exec(
		`INSERT OR IGNORE INTO operator_verifier (id, verifier, set_at, rotations, ephemeral)
		 VALUES (1, ?, ?, 0, ?)`,
		verifier, time.Now().UTC().Format(time.RFC3339Nano), eph)
	if err != nil {
		return fmt.Errorf("storage: init operator verifier: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("an operator credential already exists for this state file; " +
			"use `rotate` with the current token instead")
	}
	return nil
}

// RotateOperatorVerifier replaces the verifier and records the rotation. The
// caller must already have verified the CURRENT token.
func (s *Store) RotateOperatorVerifier(verifier, actor, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.Exec(
		`UPDATE operator_verifier SET verifier = ?, set_at = ?, rotations = rotations + 1
		 WHERE id = 1`, verifier, now)
	if err != nil {
		return fmt.Errorf("storage: rotate: %w", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return errors.New("no operator credential to rotate; run `init` first")
	}
	if _, err := tx.Exec(
		`INSERT INTO audit (at, actor, action_id, mandate_id, from_state, to_state,
		                    refund_landed, reason)
		 VALUES (?, ?, '(credential)', ?, 'OPERATOR_TOKEN', 'ROTATED', 0, ?)`,
		now, actor, s.mandateID, reason); err != nil {
		return err
	}
	return tx.Commit()
}
