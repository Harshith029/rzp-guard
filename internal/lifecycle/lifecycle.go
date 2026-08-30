// Package lifecycle owns action consumption and budget as ONE state machine,
// under a mutex, failing closed on ambiguity, and durable across restart.
//
// The governing rule: RELEASE ONLY ON CONFIRMED PROVIDER REJECTION.
//
// Releasing on timeout fails open. Razorpay may have processed the refund while
// the proxy lost the response; handing budget back would return headroom for
// money that already left. Equally, a request that provably never reached the
// provider must not permanently burn a legitimate merchant authorization --
// hence release on *confirmed* rejection, and only that.
//
// InDoubt is terminal until a human resolves it, and survives restart: a
// reservation that was live when the process died is exactly the ambiguous
// case, so recovery promotes it to InDoubt rather than dropping it.
package lifecycle

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/harshith/rzp-guard/internal/opauth"
)

type State string

const (
	Available State = "AVAILABLE"
	Reserved  State = "RESERVED"
	Committed State = "COMMITTED"
	InDoubt   State = "IN_DOUBT"
)

var (
	ErrNotAvailable  = errors.New("action is not AVAILABLE")
	ErrCumulativeCap = errors.New("cumulative cap exceeded")
	ErrBadTransition = errors.New("invalid state transition")
	ErrNotAuthorized = errors.New("operator token rejected")
)

// Reservation is one action being consumed by one forwarded call.
type Reservation struct {
	ActionID    string
	AmountPaise int64
}

// Persister is the durable side. Reserve must be written BEFORE any byte
// reaches the child, so a crash mid-flight leaves a recoverable row.
//
// The Many forms exist because one refund may consume SEVERAL actions: a
// merchant who authorized 18500 and 19000 separately has authorized 37500, and
// an agent issuing it as one call is asking for exactly what was granted. Those
// actions must move together or not at all -- a half-reserved call holds budget
// against a refund that never leaves, and a half-committed one leaves the
// ledger disagreeing with itself about a refund that either happened or did not.
type Persister interface {
	Reserve(actionID, receipt string, amountPaise int64) error
	SetState(actionID, from, to string) error
	ReserveMany(receipt string, rs []Reservation) error
	SetStateMany(actionIDs []string, from, to string) error
}

// ResolveStore performs the operator's decision and its audit record atomically.
type ResolveStore interface {
	ResolveInDoubt(actionID, toState, actor, reason string, refundLanded bool) error
}

type entry struct {
	actionID  string
	state     State
	reserved  int64
	committed int64
}

// AuditRecord mirrors what is written durably for an operator transition.
type AuditRecord struct {
	At           time.Time `json:"at"`
	Operator     string    `json:"operator"`
	ActionID     string    `json:"action_id"`
	From         State     `json:"from"`
	To           State     `json:"to"`
	RefundLanded bool      `json:"refund_landed"`
	Reason       string    `json:"reason"`
}

// Ledger tracks per-action state and session cumulative spend.
//
// Budget counts reserved + committed, never committed alone: two concurrent
// refunds must not both pass a cumulative check before either result returns.
type Ledger struct {
	mu                 sync.Mutex
	maxCumulativePaise int64
	entries            map[string]*entry
	store              Persister
}

func NewLedger(maxCumulativePaise int64, store Persister) *Ledger {
	return &Ledger{
		maxCumulativePaise: maxCumulativePaise,
		entries:            map[string]*entry{},
		store:              store,
	}
}

// Restore rebuilds in-memory state from a durable snapshot at startup.
// Callers must have already run the store's recovery step, which promotes any
// still-RESERVED row to IN_DOUBT.
func (l *Ledger) Restore(states map[string]string, amounts map[string]int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, st := range states {
		e := &entry{actionID: id, state: State(st)}
		switch State(st) {
		case Committed:
			e.committed = amounts[id]
		case Reserved, InDoubt:
			// Both hold budget. A RESERVED row should already have been promoted
			// to IN_DOUBT by recovery; if one survives, keep it encumbered.
			e.reserved = amounts[id]
		}
		l.entries[id] = e
	}
}

func (l *Ledger) entryLocked(actionID string) *entry {
	e, ok := l.entries[actionID]
	if !ok {
		e = &entry{actionID: actionID, state: Available}
		l.entries[actionID] = e
	}
	return e
}

func (l *Ledger) encumberedLocked() int64 {
	var total int64
	for _, e := range l.entries {
		total += e.reserved + e.committed
	}
	return total
}

func (l *Ledger) MaxCumulativePaise() int64 { return l.maxCumulativePaise }

func (l *Ledger) State(actionID string) State {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entryLocked(actionID).state
}

func (l *Ledger) IsAvailable(actionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.entryLocked(actionID).state == Available
}

// Encumbered is everything spent or possibly spent -- the number the cap applies to.
func (l *Ledger) Encumbered() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.encumberedLocked()
}

func (l *Ledger) Remaining() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.maxCumulativePaise - l.encumberedLocked()
}

func (l *Ledger) Committed() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var t int64
	for _, e := range l.entries {
		t += e.committed
	}
	return t
}

// HasHeadroom reports whether the cap admits amountPaise right now. Used to
// check before consuming any other scarce resource, so a request that will fail
// the cap does not burn a rate-limit slot on its way out.
func (l *Ledger) HasHeadroom(amountPaise int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return amountPaise <= l.maxCumulativePaise-l.encumberedLocked()
}

// Reserve atomically claims the action and its budget and PERSISTS the
// reservation before returning. The durable write happens inside the lock, so a
// caller that sees success knows the row exists before it forwards anything.
// Reserve claims one action.
func (l *Ledger) Reserve(actionID, receipt string, amountPaise int64) error {
	return l.ReserveMany(receipt, []Reservation{{ActionID: actionID, AmountPaise: amountPaise}})
}

// ReserveMany claims every action one forwarded call consumes.
//
// All checks run against the WHOLE set before anything is written: every action
// must be Available, and the combined amount must fit the remaining budget.
// Checking them one at a time would let a set pass whose total exceeds the cap.
func (l *Ledger) ReserveMany(receipt string, rs []Reservation) error {
	if len(rs) == 0 {
		return errors.New("reserve: no actions given")
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	var total int64
	seen := make(map[string]struct{}, len(rs))
	for _, r := range rs {
		if _, dup := seen[r.ActionID]; dup {
			// The same action twice in one call would be counted once against
			// the cap and consumed once, while the caller believes it paid for
			// two. Refuse rather than resolve the ambiguity.
			return fmt.Errorf("reserve: %s appears twice in one call", r.ActionID)
		}
		seen[r.ActionID] = struct{}{}

		e := l.entryLocked(r.ActionID)
		if e.state != Available {
			return fmt.Errorf("%w: %s is %s", ErrNotAvailable, r.ActionID, e.state)
		}
		total += r.AmountPaise
	}
	if remaining := l.maxCumulativePaise - l.encumberedLocked(); total > remaining {
		return fmt.Errorf("%w: %d paise exceeds %d remaining of %d",
			ErrCumulativeCap, total, remaining, l.maxCumulativePaise)
	}

	if l.store != nil {
		if err := l.store.ReserveMany(receipt, rs); err != nil {
			// Durability failed, so the reservation does not exist. Fail closed:
			// no in-memory claim either, for any action in the set.
			return fmt.Errorf("durable reserve failed, refusing to forward: %w", err)
		}
	}
	for _, r := range rs {
		e := l.entryLocked(r.ActionID)
		e.state = Reserved
		e.reserved = r.AmountPaise
	}
	return nil
}

func (l *Ledger) transition(actionID string, want, next State, mutate func(*entry)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entryLocked(actionID)
	if e.state != want {
		return fmt.Errorf("%w: cannot move %s from %s to %s", ErrBadTransition, actionID, e.state, next)
	}
	if l.store != nil {
		if err := l.store.SetState(actionID, string(want), string(next)); err != nil {
			return fmt.Errorf("durable state write failed: %w", err)
		}
	}
	mutate(e)
	e.state = next
	return nil
}

// Commit records a confirmed success.
// transitionMany moves a whole call's actions together.
//
// Same ordering rule as transition(): the durable write happens FIRST and the
// in-memory entries move only if it succeeded, so a failure leaves memory and
// the database agreeing on RESERVED for every action in the set.
func (l *Ledger) transitionMany(actionIDs []string, want, next State, mutate func(*entry)) error {
	if len(actionIDs) == 0 {
		return errors.New("transition: no actions given")
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, id := range actionIDs {
		if e := l.entryLocked(id); e.state != want {
			return fmt.Errorf("%w: cannot move %s from %s to %s", ErrBadTransition, id, e.state, next)
		}
	}
	if l.store != nil {
		if err := l.store.SetStateMany(actionIDs, string(want), string(next)); err != nil {
			return fmt.Errorf("durable state write failed: %w", err)
		}
	}
	for _, id := range actionIDs {
		e := l.entryLocked(id)
		mutate(e)
		e.state = next
	}
	return nil
}

// CommitMany settles every action of one forwarded refund.
func (l *Ledger) CommitMany(actionIDs []string) error {
	return l.transitionMany(actionIDs, Reserved, Committed, func(e *entry) {
		e.committed, e.reserved = e.reserved, 0
	})
}

// MarkInDoubtMany locks every action of one ambiguous refund.
func (l *Ledger) MarkInDoubtMany(actionIDs []string) error {
	return l.transitionMany(actionIDs, Reserved, InDoubt, func(e *entry) {})
}

// ReleaseConfirmedRejectionMany returns every action of a call that provably
// never reached the provider.
func (l *Ledger) ReleaseConfirmedRejectionMany(actionIDs []string) error {
	return l.transitionMany(actionIDs, Reserved, Available, func(e *entry) { e.reserved = 0 })
}

func (l *Ledger) Commit(actionID string) error {
	return l.transition(actionID, Reserved, Committed, func(e *entry) {
		e.committed, e.reserved = e.reserved, 0
	})
}

// ReleaseConfirmedRejection is the ONLY automatic path back to Available.
// Callers must hold positive evidence the provider rejected the request; a
// timeout or crash is NOT evidence -- use MarkInDoubt.
func (l *Ledger) ReleaseConfirmedRejection(actionID string) error {
	return l.transition(actionID, Reserved, Available, func(e *entry) { e.reserved = 0 })
}

// MarkInDoubt locks the action and its budget pending an operator. The reserved
// amount is deliberately retained: the money may well have moved.
func (l *Ledger) MarkInDoubt(actionID string) error {
	return l.transition(actionID, Reserved, InDoubt, func(e *entry) {})
}

func (l *Ledger) InDoubtActions() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for id, e := range l.entries {
		if e.state == InDoubt {
			out = append(out, id)
		}
	}
	return out
}

// resolveInDoubt is unexported, so nothing on the request-handling path can
// reach it. The only exported route in is ResolveInDoubt, which demands an
// opauth.Grant.
//
// (An earlier comment here said "Console is the only caller". That type was
// removed when authentication moved into opauth; the guarantee no longer
// depends on a single caller behaving, it depends on the signature.)
func (l *Ledger) resolveInDoubt(actionID string, refundLanded bool, store ResolveStore,
	actor, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entryLocked(actionID)
	if e.state != InDoubt {
		return fmt.Errorf("%w: %s is %s, not IN_DOUBT", ErrBadTransition, actionID, e.state)
	}
	next := Available
	if refundLanded {
		next = Committed
	}
	if store != nil {
		// State change and audit record land in one transaction, or neither does.
		if err := store.ResolveInDoubt(actionID, string(next), actor, reason, refundLanded); err != nil {
			return err
		}
	}
	if refundLanded {
		e.committed, e.reserved = e.reserved, 0
	} else {
		e.reserved = 0
	}
	e.state = next
	return nil
}

// ResolveInDoubt clears an IN_DOUBT reservation. It is the ONLY exported way to
// do so, and it requires an opauth.Grant, which only opauth can mint.
//
// There is deliberately no token parameter and no credential comparison in this
// package. The previous Console took a token at construction and compared a
// caller-supplied token against it -- both sides came from the caller, so the
// check was vacuous. Authentication belongs at one boundary, not scattered into
// a library where the next caller can get it wrong.
//
// landed must come from a human who checked Razorpay for the issued receipt.
// Absence of a matching record is NOT sufficient to pass false: eventual
// consistency, a pending refund, or a failed lookup all produce "not found"
// without meaning "did not happen".
func ResolveInDoubt(g opauth.Grant, l *Ledger, store ResolveStore,
	actionID string, landed bool, reason string) error {
	if !g.Valid() {
		return ErrNotAuthorized
	}
	if store == nil {
		return fmt.Errorf("a durable audit store is required: unaudited resolution " +
			"of a possibly-completed refund is not an acceptable operation")
	}
	if g.Subject() == "" || reason == "" {
		return fmt.Errorf("operator identity and reason are required for the audit record")
	}
	return l.resolveInDoubt(actionID, landed, store, g.Subject(), reason)
}
