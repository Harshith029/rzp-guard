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

// Persister is the durable side. Reserve must be written BEFORE any byte
// reaches the child, so a crash mid-flight leaves a recoverable row.
type Persister interface {
	Reserve(actionID, receipt string, amountPaise int64) error
	SetState(actionID, state string) error
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
func (l *Ledger) Reserve(actionID, receipt string, amountPaise int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entryLocked(actionID)
	if e.state != Available {
		return fmt.Errorf("%w: %s is %s", ErrNotAvailable, actionID, e.state)
	}
	if remaining := l.maxCumulativePaise - l.encumberedLocked(); amountPaise > remaining {
		return fmt.Errorf("%w: %d paise exceeds %d remaining of %d",
			ErrCumulativeCap, amountPaise, remaining, l.maxCumulativePaise)
	}
	if l.store != nil {
		if err := l.store.Reserve(actionID, receipt, amountPaise); err != nil {
			// Durability failed, so the reservation does not exist. Fail closed:
			// no in-memory claim either.
			return fmt.Errorf("durable reserve failed, refusing to forward: %w", err)
		}
	}
	e.state = Reserved
	e.reserved = amountPaise
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
		if err := l.store.SetState(actionID, string(next)); err != nil {
			return fmt.Errorf("durable state write failed: %w", err)
		}
	}
	mutate(e)
	e.state = next
	return nil
}

// Commit records a confirmed success.
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

// resolveInDoubt is unexported. Console is the only caller, so nothing on the
// request-handling path can reach it.
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

// Console is the operator resolution path: separate from the relay surface,
// token-gated, and audited. The relay never holds the token.
type Console struct {
	ledger *Ledger
	token  string
	store  ResolveStore
}

func NewConsole(l *Ledger, token string, store ResolveStore) (*Console, error) {
	if len(token) < 16 {
		return nil, fmt.Errorf("operator token must be at least 16 characters")
	}
	if store == nil {
		return nil, fmt.Errorf("a durable audit store is required: unaudited resolution " +
			"of a possibly-completed refund is not an acceptable operation")
	}
	return &Console{ledger: l, token: token, store: store}, nil
}

// Resolve clears an IN_DOUBT reservation.
//
// refundLanded must come from a human checking Razorpay for the issued receipt.
// Absence of a matching record is NOT sufficient to pass false: eventual
// consistency, a still-pending refund, or a failed lookup all produce "not
// found" without meaning "did not happen".
func (c *Console) Resolve(token, operator, actionID string, refundLanded bool, reason string) error {
	if token != c.token {
		return ErrNotAuthorized
	}
	if operator == "" || reason == "" {
		return fmt.Errorf("operator identity and reason are required for the audit record")
	}
	return c.ledger.resolveInDoubt(actionID, refundLanded, c.store, operator, reason)
}
