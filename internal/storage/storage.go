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
const schemaVersion = 3

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

-- Which process currently owns which mandate's ledger, as a LEASE.
--
-- This replaces PRAGMA locking_mode = EXCLUSIVE plus a one-row owner table,
-- and the reason is operational rather than aesthetic.
--
-- WHAT THE EXCLUSIVE LOCK COST. It held the database lock for the guard's
-- entire lifetime, so rzp-guard-operator could not open the state file at all
-- while the guard ran. Every operator action -- including listing what is
-- stuck -- required stopping the guard first. That is tolerable for resolving
-- an IN_DOUBT refund, which is reconciliation nobody is waiting on. It is not
-- tolerable for unblocking a refund the guard wrongly refused, because a
-- customer IS waiting, and "stop the payment proxy to unstick one refund" is
-- not a thing a support desk will do. The published false-positive cost model
-- assumes a human is standing by to unblock; the lock is what made that human
-- impossible.
--
-- It also forced one mandate per FILE, so ten merchants meant ten databases,
-- ten IN_DOUBT queues and ten operator credentials, and an operator had to know
-- which file held the refund they were looking for.
--
-- WHAT THE LEASE KEEPS. The money claim was never about the file. It is that
-- two guards must not each restore an in-memory ledger for the SAME mandate and
-- check the cumulative cap against their own copy, because between them they
-- could spend past it. Every table here is already scoped by mandate_id --
-- actions, budget, rate window, receipts -- so the claim is preserved exactly by
-- leasing per mandate rather than per file.
--
-- HOW IT IS ENFORCED. Acquisition is one conditional UPDATE that succeeds only
-- when no live lease exists, so it is atomic in SQLite rather than a
-- read-then-write race. The holder renews on a timer; a lease whose heartbeat
-- has gone stale is takeable, which is what lets a crashed guard's mandate be
-- restarted without an operator command.
--
-- THE COST, STATED: after a crash the next guard waits out leaseTTL instead of
-- starting instantly, because a stale heartbeat and a busy process are not
-- distinguishable from here. A clean shutdown releases the lease, so the wait
-- applies only to an actual crash -- which already requires operator attention,
-- since recovery promotes the in-flight reservation to IN_DOUBT.
CREATE TABLE IF NOT EXISTS owner_lease (
  mandate_id   TEXT PRIMARY KEY,
  -- Random per acquisition. Renewal and release are conditional on it, so a
  -- process that lost its lease to a takeover cannot keep renewing one it no
  -- longer holds, and cannot release the new holder's.
  holder       TEXT    NOT NULL,
  host         TEXT    NOT NULL,
  pid          INTEGER NOT NULL,
  acquired_at  TEXT    NOT NULL,
  -- Unix nanoseconds, so staleness is an integer comparison rather than a
  -- string one. A text timestamp compared lexically is a defect waiting for a
  -- timezone.
  heartbeat_ns INTEGER NOT NULL
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

-- Every refund the guard REFUSED, durably, so a human can see what was blocked.
--
-- Until this table existed, a refusal went to stderr and, if configured, to an
-- optional JSONL decision log. Neither is a queue: nothing tracks whether a
-- blocked refund was ever looked at, and the decision log records the allowed
-- calls too, so the events needing a human are buried among thousands that do
-- not. The published false-positive rate is 0.455. Blocking 45% of legitimate
-- refunds is only survivable if somebody sees them, and nothing showed them.
--
-- DEDUPLICATED on (mandate, rule, payment, amount). An agent that retries a
-- refused call in a loop is the normal case, not the exception, and one row per
-- attempt would turn the queue into the same unreadable stream stderr already
-- is. occurrences counts the retries; first_at and last_at bracket them.
--
-- CONTENT IS AGENT-CONTROLLED. payment_id comes straight off the wire and
-- reason embeds it. Everything rendering this table must escape it, exactly as
-- the decision log's comment says.
CREATE TABLE IF NOT EXISTS denial (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  mandate_id   TEXT    NOT NULL,
  tool         TEXT    NOT NULL,
  rule         TEXT    NOT NULL,
  payment_id   TEXT    NOT NULL,
  amount_paise INTEGER NOT NULL,
  reason       TEXT    NOT NULL,
  first_at     TEXT    NOT NULL,
  last_at      TEXT    NOT NULL,
  occurrences  INTEGER NOT NULL DEFAULT 1,
  -- OPEN | APPROVED | DECLINED. An operator decision, never the guard's.
  resolution   TEXT    NOT NULL DEFAULT 'OPEN',
  UNIQUE (mandate_id, rule, payment_id, amount_paise)
);
CREATE INDEX IF NOT EXISTS denial_open ON denial (resolution, last_at);

-- A single-use authorization issued by a human for one refused refund.
--
-- THE GUARD NEVER WRITES THIS TABLE, exactly as it never writes
-- operator_verifier. IssueGrant demands an opauth.Grant, which only opauth can
-- mint and only after verifying the operator token, so the authority to widen a
-- mandate lives behind the same credential as the authority to resolve an
-- IN_DOUBT refund. Those are the same kind of decision -- a person accepting
-- responsibility for money -- and they should not have different doors.
--
-- WHAT A GRANT DELIBERATELY CANNOT DO:
--
--   It is EXACT. There is no bounded form. A bound is discretion, and an
--   operator unblocking one refused refund is not the moment to delegate a
--   figure to the agent.
--
--   It EXPIRES, and soon. A grant that outlives the incident is standing
--   authority nobody revisits, which is the failure the mandate's own expiry
--   exists to prevent.
--
--   It is SINGLE USE, because it becomes an ordinary row in action_state and
--   goes through the same lifecycle as any other action. There is no second
--   money path; there is one path with one more way to enter it.
--
--   It COUNTS AGAINST THE MANDATE'S CUMULATIVE CAP. So an operator can correct
--   a wrong refusal but cannot raise the merchant's own ceiling. That is the
--   one limit a support desk should not be able to lift alone, and making the
--   grant an ordinary action is what makes it hold without extra code.
CREATE TABLE IF NOT EXISTS operator_grant (
  grant_id     TEXT PRIMARY KEY,
  mandate_id   TEXT    NOT NULL,
  denial_id    INTEGER NOT NULL,
  payment_id   TEXT    NOT NULL,
  amount_paise INTEGER NOT NULL,
  issued_at    TEXT    NOT NULL,
  expires_at   TEXT    NOT NULL,
  expires_ns   INTEGER NOT NULL,
  actor        TEXT    NOT NULL,
  reason       TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS grant_live ON operator_grant (mandate_id, expires_ns);
`

// Store is the durable side of the action lifecycle.
type Store struct {
	db        *sql.DB
	mandateID string

	// holder is this process's lease token, empty for an attached store. Renewal
	// and release are conditional on it, so a process that lost its lease to a
	// takeover cannot reclaim it by heartbeating or release the new holder's.
	holder string

	// attached marks a store opened WITHOUT a lease -- the operator's view while
	// a guard is running. It leases nothing, renews nothing and releases nothing.
	attached bool
}

// Attached reports whether this store was opened without taking a lease. The
// operator CLI uses it to decide which commands are safe while a guard is live.
func (s *Store) Attached() bool { return s.attached }

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
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	// Before ANY read of the tables below. A file this build cannot correctly
	// interpret must be refused while it is still untouched, not after its
	// contents have been half-understood.
	if err := checkSchemaVersion(db); err != nil {
		db.Close()
		// A migration that lost a race to another process's migration is
		// contention, not a file this build cannot read. Hand it to the retry
		// in openWithDeadline, which has the deadline and the fail-closed
		// exit; returning it here would refuse an upgrade that merely needed
		// to go second. Several guards starting together on one pre-v3 file is
		// exactly the case an upgrade produces.
		if lockContended(err) {
			return nil, fmt.Errorf("%w (%v)", errLockContended, err)
		}
		return nil, err
	}

	// Take the lease for THIS MANDATE. Not for the file: the money claim is that
	// two ledgers must not exist over one mandate's actions and budget, and
	// every table here is scoped by mandate_id, so that is the level the
	// exclusion belongs at. See the owner_lease comment in the schema for what
	// the file-wide exclusive lock cost and why it is gone.
	holder, err := acquireLease(db, mandateID)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, mandateID: mandateID, holder: holder}, nil
}

// Attach opens a state file WITHOUT taking a lease.
//
// This is how rzp-guard-operator reads and writes while a guard is running, and
// it is the whole reason the exclusive lock had to go. An attached store must
// not perform any operation that mutates state a live guard is holding in
// memory -- the guard would keep serving from a stale ledger. Which operations
// those are is decided by the caller, which can ask LeaseFor whether a guard is
// live; the CLI refuses the mutating ones and permits the rest.
//
// Reading is always safe: WAL means a reader never blocks a writer or sees a
// half-written transaction.
func Attach(path, mandateID string) (*Store, error) {
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	if err := checkSchemaVersion(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, mandateID: mandateID, attached: true}, nil
}

// openDB applies the schema and the pragmas every caller needs, leasing nothing.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("storage: open: %w", err)
	}
	// One writer per process. SQLite serializes writes anyway; this keeps the
	// pool from opening a second connection that would contend with the first.
	db.SetMaxOpenConns(1)

	// A bounded wait for the write lock, replacing the exclusive lock that used
	// to make contention impossible by making sharing impossible.
	//
	// The old comment argued against a busy timeout, and it was right about the
	// situation it described: under locking_mode = EXCLUSIVE a waiter sleeps
	// while holding the SHARED lock everyone else needs, so the timeout cannot
	// break the deadlock. Without EXCLUSIVE that deadlock does not form, and a
	// busy timeout is exactly the right tool -- writes here are single-statement
	// transactions measured in fractions of a millisecond, so the wait is for a
	// queue of one or two, not for a long-running holder.
	//
	// The value is a ceiling on how long an authorized refund can stall behind
	// another writer, so it is deliberately short. Exceeding it fails the
	// reservation, and a failed reservation forwards nothing.
	if _, err := db.Exec(`PRAGMA busy_timeout = 2000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("storage: busy timeout: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		if lockContended(err) {
			return nil, fmt.Errorf("%w (%v)", errLockContended, err)
		}
		return nil, fmt.Errorf("storage: %w (%v)", ErrNotOwner, err)
	}
	return db, nil
}

// Close releases the lease before closing the handle, so an ordinary restart
// does not wait out leaseTTL. Release is best-effort: the lease expires on its
// own, and a missed release costs one TTL of delay, which is not worth failing
// a shutdown over.
func (s *Store) Close() error {
	s.releaseLease()
	return s.db.Close()
}

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
	return s.reserveMany(receipt, rs, 0)
}

// ReserveManyWithCall reserves the actions AND records the rate-window slot in
// ONE transaction, which is one fsync instead of two.
//
// WHY IT EXISTS -- THE PERFORMANCE HALF. At synchronous=FULL a commit costs
// about 6.5ms on this hardware, and an allowed refund performed two of them:
// 15.3ms measured end to end, against 779ns for a denial. Durability is the
// whole design and is not up for negotiation, but performing it TWICE for one
// decision was not a requirement, it was an artifact of the two writes living
// in different packages. One transaction halves the cost of every authorized
// refund and roughly doubles the ceiling, from ~65 to ~130 per second per
// process, with no guarantee weakened -- both rows are still durable before a
// byte reaches the child.
//
// WHY IT EXISTS -- THE CORRECTNESS HALF, WHICH MATTERS MORE. Two transactions
// could half-succeed. policy.reserveSet dealt with that by attempting a
// rollback, and said so honestly: "The rollback is attempted, not guaranteed:
// the rate write failing usually means the store is broken, so the release will
// fail too." That left a real state where actions sit RESERVED holding budget
// against a refund that never left the building, discovered only at the next
// restart as a spurious IN_DOUBT an operator has to ask about. One transaction
// removes the state rather than compensating for it.
//
// atUnixNano of 0 means no slot -- the plain ReserveMany path, kept for callers
// that do not own a rate window.
func (s *Store) ReserveManyWithCall(receipt string, rs []lifecycle.Reservation,
	atUnixNano int64) error {
	return s.reserveMany(receipt, rs, atUnixNano)
}

func (s *Store) reserveMany(receipt string, rs []lifecycle.Reservation,
	atUnixNano int64) error {
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

	// The rate-window slot, in the SAME transaction as the reservation it
	// belongs to. Consuming the slot and claiming the actions are one decision;
	// splitting them into two commits made them two facts that could disagree.
	if atUnixNano != 0 {
		if _, err := tx.Exec(
			`INSERT INTO call_log (mandate_id, at_unix_nano) VALUES (?, ?)`,
			s.mandateID, atUnixNano); err != nil {
			return fmt.Errorf("storage: reserve: record call: %w", err)
		}
		// Pruning rides along for free: it is already inside a transaction that
		// is about to fsync, so bounding the table costs nothing here. Doing it
		// as its own statement outside would have doubled the cost of every
		// refund to buy tidiness (F23).
		if _, err := tx.Exec(
			`DELETE FROM call_log WHERE mandate_id = ? AND at_unix_nano < ?`,
			s.mandateID, atUnixNano-int64(callLogRetention)); err != nil {
			return fmt.Errorf("storage: reserve: prune call log: %w", err)
		}
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
// uniqueIndexOnReceiptOnly counts the unique indexes over exactly one column,
// and that column is receipt. v1 declared `receipt TEXT NOT NULL UNIQUE`, which
// SQLite implements as precisely such an index. v3's only unique index is the
// primary key, over (mandate_id, action_id).
const uniqueIndexOnReceiptOnly = `
SELECT COUNT(*) FROM pragma_index_list('action_state') il
WHERE il."unique" = 1
  AND (SELECT COUNT(*) FROM pragma_index_info(il.name)) = 1
  AND (SELECT ii.name FROM pragma_index_info(il.name) ii) = 'receipt'`

// declaresUniqueReceipt reports whether action_state carries v1's uniqueness
// constraint, structurally.
//
// IT USED TO READ THE DDL TEXT: strings.Contains(ddl, "UNIQUE") against
// sqlite_master.sql. sqlite_master stores CREATE TABLE statements verbatim,
// COMMENTS INCLUDED, and the v3 action_state carries a comment explaining that
// receipt is "NOT UNIQUE any more". So the string matched on every file this
// build has ever created: each new state file was adopted as v1 and run through
// both migrations against a schema that was already current.
//
// It finished stamped correctly, which is why it survived review. migrateV1toV2
// rebuilds action_state, and the rebuilt table has no comments -- so the file
// looked wrong only for the few milliseconds it took to migrate, and looked
// right forever after.
//
// It was not harmless. Sixteen processes opening one new file each ran two
// unnecessary write transactions against it, and CI failed on the contention:
// SQLITE_BUSY out of migrate v2->v3, in TestTheMandateLeaseHoldsUnderConcurrentOpens.
//
// An index cannot be forged by prose, so the question is now put to the indexes.
func declaresUniqueReceipt(db *sql.DB) (bool, error) {
	var n int
	if err := db.QueryRow(uniqueIndexOnReceiptOnly).Scan(&n); err != nil {
		return false, fmt.Errorf("storage: inspect action_state indexes: %w", err)
	}
	return n > 0, nil
}

func checkSchemaVersion(db *sql.DB) error {
	// An UNSTAMPED file is not necessarily a new one.
	//
	// Files created before schema_meta existed carry the v1 layout, and
	// stamping them with the current version would skip the migration they
	// actually need -- adopting a v1 file as v2 and then reading it with v2
	// assumptions. So the version is INFERRED FROM STRUCTURE when the stamp is
	// missing: v1's action_state declares receipt UNIQUE and v2's does not.
	//
	// The question is put to the file's INDEXES, for the reason in
	// declaresUniqueReceipt. It asks what the file IS rather than what it is
	// labelled, which is the whole point of inferring at all.
	initial := schemaVersion
	var stamped int
	switch err := db.QueryRow(`SELECT version FROM schema_meta WHERE id = 1`).
		Scan(&stamped); {
	case err == nil:
		initial = stamped
	case errors.Is(err, sql.ErrNoRows):
		v1, ierr := declaresUniqueReceipt(db)
		if ierr != nil {
			return ierr
		}
		if v1 {
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
	// Migrations run in sequence, so a v1 file reaches v3 through v2 rather than
	// needing a v1->v3 path nobody would ever exercise. Each step is its own
	// transaction and each ends by stamping its own version, so a failure part
	// way leaves a file at a version this build understands how to resume from.
	for found < schemaVersion {
		var err error
		switch found {
		case 1:
			err = migrateV1toV2(db)
		case 2:
			err = migrateV2toV3(db)
		default:
			return fmt.Errorf("%w: file is version %d and no migration exists for "+
				"that step, which is a defect in rzp-guard and not in your state "+
				"file. Do NOT delete it: it may hold IN_DOUBT actions whose refunds "+
				"already moved money", ErrSchemaVersion, found)
		}
		if err != nil {
			return err
		}
		if err := db.QueryRow(`SELECT version FROM schema_meta WHERE id = 1`).
			Scan(&found); err != nil {
			return fmt.Errorf("storage: read schema version after migration: %w", err)
		}
	}

	switch {
	case found == schemaVersion:
		return nil
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

// migrateV2toV3 replaces the one-row owner table with a per-mandate lease, and
// adds the denial queue and operator grants.
//
// THE OWNER ROW IS NOT CARRIED FORWARD AS A LIVE LEASE. A v2 file's owner row
// records which mandate last used the file, not which process is running now --
// the exclusive lock was what proved liveness, and the lock is gone. Copying it
// in as a live lease would make the first guard to open a migrated file refuse
// to start, blaming a process that exited weeks ago. The row is dropped and the
// first opener takes a fresh lease, which is correct: nothing is running yet,
// because a v2 build could not have been holding a v3 file open.
//
// One transaction. A half-migrated state file holding refund reservations is
// worse than one that refuses to open.
func migrateV2toV3(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("storage: migrate v2->v3: %w", err)
	}
	defer tx.Rollback()

	// The tables themselves were created by the schema statement at open, which
	// is CREATE TABLE IF NOT EXISTS throughout. What remains is retiring the old
	// one and stamping the version -- the two things IF NOT EXISTS cannot do.
	stmts := []string{
		`DROP TABLE IF EXISTS owner`,
		`UPDATE schema_meta SET version = 3 WHERE id = 1`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("storage: migrate v2->v3: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: migrate v2->v3: %w", err)
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
