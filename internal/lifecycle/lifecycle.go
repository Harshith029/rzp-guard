// Package lifecycle owns action consumption and budget as ONE state machine,
// under a mutex, failing closed on ambiguity.
//
// The governing rule: RELEASE ONLY ON CONFIRMED PROVIDER REJECTION.
//
// Releasing on timeout fails open. Razorpay may have processed the refund while
// the proxy lost the response; handing budget back would return headroom for
// money that already left, and the cap breaks exactly when it matters. Equally,
// a request that provably never reached the provider must not permanently burn a
// legitimate merchant authorization -- hence release on *confirmed* rejection,
// and only that.
//
// InDoubt is terminal until a human resolves it. Resolution is deliberately NOT
// reachable from this package's request-handling surface: it lives on Console,
// which requires an operator token and writes an audit record. The prototype's
// equivalent was a public method with a comment claiming otherwise.
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

type entry struct {
	actionID  string
	state     State
	reserved  int64
	committed int64
}

// AuditRecord is written for every operator-initiated transition.
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
// Every method takes the mutex. Budget counts reserved + committed, never
// committed alone: two concurrent refunds must not both pass a cumulative check
// before either result returns. MCP permits multiple in-flight requests by
// JSON-RPC id, so that TOCTOU is reachable, not theoretical.
type Ledger struct {
	mu                 sync.Mutex
	maxCumulativePaise int64
	entries            map[string]*entry
}

func NewLedger(maxCumulativePaise int64) *Ledger {
	return &Ledger{maxCumulativePaise: maxCumulativePaise, entries: map[string]*entry{}}
}

// entryLocked requires l.mu held.
func (l *Ledger) entryLocked(actionID string) *entry {
	e, ok := l.entries[actionID]
	if !ok {
		e = &entry{actionID: actionID, state: Available}
		l.entries[actionID] = e
	}
	return e
}

// encumberedLocked requires l.mu held.
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

// Reserve atomically claims the action and its budget, before forwarding.
// The check and the claim happen under one lock, so concurrent duplicates
// cannot both succeed.
func (l *Ledger) Reserve(actionID string, amountPaise int64) error {
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
	e.state = Reserved
	e.reserved = amountPaise
	return nil
}

// Commit records a confirmed success.
func (l *Ledger) Commit(actionID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entryLocked(actionID)
	if e.state != Reserved {
		return fmt.Errorf("%w: cannot commit %s from %s", ErrBadTransition, actionID, e.state)
	}
	e.committed, e.reserved, e.state = e.reserved, 0, Committed
	return nil
}

// ReleaseConfirmedRejection is the ONLY automatic path back to Available.
//
// Callers must hold positive evidence that the provider rejected the request.
// A timeout, a dropped connection or a child crash is NOT evidence: use
// MarkInDoubt for those.
func (l *Ledger) ReleaseConfirmedRejection(actionID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entryLocked(actionID)
	if e.state != Reserved {
		return fmt.Errorf("%w: cannot release %s from %s", ErrBadTransition, actionID, e.state)
	}
	e.reserved, e.state = 0, Available
	return nil
}

// MarkInDoubt locks the action and its budget pending an operator. The reserved
// amount is deliberately retained: the money may well have moved.
func (l *Ledger) MarkInDoubt(actionID string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entryLocked(actionID)
	if e.state != Reserved {
		return fmt.Errorf("%w: cannot mark %s in doubt from %s", ErrBadTransition, actionID, e.state)
	}
	e.state = InDoubt
	return nil
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

// resolveInDoubt is unexported: the only caller is Console, so nothing on the
// request-handling path can reach it.
func (l *Ledger) resolveInDoubt(actionID string, refundLanded bool) (State, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entryLocked(actionID)
	if e.state != InDoubt {
		return e.state, fmt.Errorf("%w: %s is %s, not IN_DOUBT", ErrBadTransition, actionID, e.state)
	}
	from := e.state
	if refundLanded {
		e.committed, e.reserved, e.state = e.reserved, 0, Committed
	} else {
		e.reserved, e.state = 0, Available
	}
	return from, nil
}

// Console is the operator resolution path: separate from the relay surface,
// token-gated, and audited. Constructing one requires the operator token, which
// the relay never holds.
type Console struct {
	ledger *Ledger
	token  string
	audit  func(AuditRecord)
}

func NewConsole(l *Ledger, token string, audit func(AuditRecord)) (*Console, error) {
	if len(token) < 16 {
		return nil, fmt.Errorf("operator token must be at least 16 characters")
	}
	if audit == nil {
		return nil, fmt.Errorf("an audit sink is required: unaudited resolution of a " +
			"possibly-completed refund is not an acceptable operation")
	}
	return &Console{ledger: l, token: token, audit: audit}, nil
}

// Resolve clears an IN_DOUBT reservation. Requires the operator token and always
// writes an audit record.
//
// refundLanded must come from a human checking Razorpay for the injected
// receipt. Absence of a matching record is NOT sufficient to pass false:
// eventual consistency, a still-pending refund, or a failed lookup all produce
// "not found" without meaning "did not happen".
func (c *Console) Resolve(token, operator, actionID string, refundLanded bool, reason string) error {
	if token != c.token {
		return ErrNotAuthorized
	}
	if operator == "" || reason == "" {
		return fmt.Errorf("operator identity and reason are required for the audit record")
	}
	from, err := c.ledger.resolveInDoubt(actionID, refundLanded)
	if err != nil {
		return err
	}
	to := Available
	if refundLanded {
		to = Committed
	}
	c.audit(AuditRecord{
		At: time.Now().UTC(), Operator: operator, ActionID: actionID,
		From: from, To: to, RefundLanded: refundLanded, Reason: reason,
	})
	return nil
}
