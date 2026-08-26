// Package policy is the default-deny decision pipeline for create_refund.
//
// Deterministic. No model, no scoring, no learned component: the authorization
// decision is a lookup against a merchant-issued capability list, and that is
// the point.
//
// Pipeline: mandate validity -> tool allowlist -> argument typing -> action
// match -> rate limit -> atomic reserve -> receipt injection.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/harshith/rzp-guard/internal/lifecycle"
	"github.com/harshith/rzp-guard/internal/mandate"
)

const RefundTool = "create_refund"

// supportedTools is the surface THIS BUILD will ever forward, independent of
// any mandate. A mandate is a session-scoped grant; it can only narrow this
// set, never widen it. Without this, a mandate listing initiate_payment or
// create_instant_settlement would have them forwarded untouched -- verified
// behaviour of the previous revision, and a direct contradiction of "reads plus
// create_refund only".
//
// Every name here was confirmed present at runtime in evidence/tools_list.json
// against the pinned image digest, not taken from the README (which describes a
// newer build with renamed tools).
var supportedTools = map[string]struct{}{
	"create_refund":                      {},
	"fetch_payment":                      {},
	"fetch_all_payments":                 {},
	"fetch_order":                        {},
	"fetch_order_payments":               {},
	"fetch_refund":                       {},
	"fetch_all_refunds":                  {},
	"fetch_multiple_refunds_for_payment": {},
	"fetch_specific_refund_for_payment":  {},
}

// SupportedTools returns the build-level surface, for the dashboard and tests.
func SupportedTools() []string {
	out := make([]string, 0, len(supportedTools))
	for name := range supportedTools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Rule identifiers, recorded verbatim in the decision log.
const (
	MandateExpired        = "MANDATE_EXPIRED"
	ToolNotAllowed        = "TOOL_NOT_ALLOWED"
	MalformedArguments    = "MALFORMED_ARGUMENTS"
	NoAuthorizedAction    = "NO_AUTHORIZED_ACTION"
	AmountNotAuthorized   = "AMOUNT_NOT_AUTHORIZED"
	ActionConsumed        = "ACTION_CONSUMED"
	RateLimitExceeded     = "RATE_LIMIT_EXCEEDED"
	CumulativeCapExceeded = "CUMULATIVE_CAP_EXCEEDED"
	ToolNotSupported      = "TOOL_NOT_SUPPORTED"
	Allowed               = "ALLOWED"
)

// Decision is the complete record of one authorization outcome.
type Decision struct {
	Allowed         bool           `json:"allowed"`
	Rule            string         `json:"rule"`
	Reason          string         `json:"reason"`
	Tool            string         `json:"tool"`
	MatchedActionID string         `json:"matched_action_id,omitempty"`
	Receipt         string         `json:"receipt,omitempty"`
	AuthorizedPaise int64          `json:"authorized_paise,omitempty"`
	Forwarded       map[string]any `json:"-"`
}

// ForwardedAmountPaise reports the amount actually written to the child, so a
// test can assert it equals AuthorizedPaise exactly. The prototype authorized
// int(50000.9)==50000 and then forwarded 50000.9 (FAILURES.md F1.a); here the
// forwarded value is the canonical int64, so the two cannot diverge.
func (d Decision) ForwardedAmountPaise() (int64, bool) {
	v, ok := d.Forwarded["amount"]
	if !ok {
		return 0, false
	}
	n, ok := v.(int64)
	return n, ok
}

// parseAmountPaise accepts ONLY a JSON integer.
//
// Razorpay amounts are integer paise. The runtime schema declares amount as
// {"type":"number"} rather than integer (evidence/tools_list.json), so a
// fractional value is schema-valid at the MCP layer and the child will NOT
// reject it. The guard must, or an amount can be authorized as its truncation
// and forwarded as something else.
//
// Rejected: booleans, fractions, exponent forms that are not integral,
// non-finite values, and anything that overflows int64.
func parseAmountPaise(v any) (int64, error) {
	switch n := v.(type) {
	case json.Number:
		s := n.String()
		if strings.ContainsAny(s, ".eE") {
			// Could still be integral (1e3), but accepting exponent notation for
			// money invites exactly the ambiguity this function exists to remove.
			return 0, fmt.Errorf("amount %q must be a plain JSON integer in paise, "+
				"not a fraction or exponent form", s)
		}
		i, err := n.Int64()
		if err != nil {
			return 0, fmt.Errorf("amount %q is not representable as an integer: %w", s, err)
		}
		return i, nil
	case bool:
		return 0, errors.New("amount must be a number, not a boolean")
	case float64:
		// Rejected outright, never coerced. A float64 here means the transport
		// decoded without UseNumber, which silently turns 1e3 into 1000 and
		// bypasses the exponent rule above -- verified behaviour of the previous
		// revision. Failing closed also surfaces the transport bug instead of
		// hiding it behind a value that happens to be integral.
		return 0, fmt.Errorf("amount arrived as float64 (%v); the transport must "+
			"decode with json.Decoder.UseNumber so paise are never rounded", n)
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("amount must be a JSON integer, got %T", v)
	}
}

// RateStore persists the rate window. An in-memory limiter resets on restart,
// which would let a crash-loop bypass max_calls_per_minute entirely.
type RateStore interface {
	RecordCall(atUnixNano int64) error
	RecentCalls(cutoffUnixNano int64) ([]int64, error)
}

type rateLimiter struct {
	mu    sync.Mutex
	max   int
	times []time.Time
	store RateStore
}

// restore reloads the durable window at startup, before any traffic.
func (r *rateLimiter) restore(now time.Time) error {
	if r.store == nil {
		return nil
	}
	cutoff := now.Add(-time.Minute).UnixNano()
	seen, err := r.store.RecentCalls(cutoff)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.times = r.times[:0]
	for _, ns := range seen {
		r.times = append(r.times, time.Unix(0, ns).UTC())
	}
	return nil
}

func (r *rateLimiter) evictLocked(now time.Time) {
	cutoff := now.Add(-time.Minute)
	keep := r.times[:0]
	for _, t := range r.times {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	r.times = keep
}

func (r *rateLimiter) count(now time.Time) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictLocked(now)
	return len(r.times)
}

// hasHeadroom and record are separate so the slot is consumed only once the
// call is actually going to the child. Both run under the Guard mutex, so no
// other request can slip between them.
func (r *rateLimiter) hasHeadroom(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictLocked(now)
	return len(r.times) < r.max
}

// record persists BEFORE touching the in-memory window. Appending first would
// leave a slot consumed by a call that was never forwarded when the durable
// write fails, so a transient SQLite error would silently shrink the merchant's
// legitimate rate allowance.
func (r *rateLimiter) record(now time.Time) error {
	if r.store != nil {
		if err := r.store.RecordCall(now.UnixNano()); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.times = append(r.times, now)
	return nil
}

// Guard is session-scoped authorization state, bound to the process lifetime.
// There is deliberately no method that accepts a replacement mandate.
type Guard struct {
	mu      sync.Mutex // serializes the match->reserve critical section
	mandate *mandate.Mandate
	ledger  *lifecycle.Ledger
	rate    *rateLimiter
}

// New builds a Guard with in-memory state only. Use NewWithStore for anything
// that moves money: in-memory state does not survive a restart.
func New(m *mandate.Mandate) *Guard { return NewWithStore(m, nil) }

// NewWithStore builds a Guard whose reservations are durable.
func NewWithStore(m *mandate.Mandate, store lifecycle.Persister) *Guard {
	g := NewWithLedger(m, lifecycle.NewLedger(m.Limits.MaxCumulativePaise, store))
	if rs, ok := store.(RateStore); ok {
		g.rate.store = rs
	}
	return g
}

// RestoreRateWindow reloads the durable rate window. Call at startup alongside
// Restore, before accepting traffic.
func (g *Guard) RestoreRateWindow(now time.Time) error { return g.rate.restore(now) }

// NewWithLedger composes a Guard over a caller-owned Ledger.
//
// This is how main wires the operator path: it builds the Ledger and hands it
// to both this Guard and lifecycle.ResolveInDoubt. The relay receives only the
// Guard, which has no way to reach the Ledger or the resolution path.
func NewWithLedger(m *mandate.Mandate, l *lifecycle.Ledger) *Guard {
	return &Guard{
		mandate: m,
		ledger:  l,
		rate:    &rateLimiter{max: m.Limits.MaxCallsPerMinute},
	}
}

func (g *Guard) Mandate() *mandate.Mandate { return g.mandate }

// Restore rebuilds durable state at startup, before any traffic is accepted.
func (g *Guard) Restore(states map[string]string, amounts map[string]int64) {
	g.ledger.Restore(states, amounts)
}

// --- narrow relay interface ---------------------------------------------
//
// The relay gets exactly these three outcome transitions plus read-only views.
// It cannot reach resolveInDoubt, whose only exported route is
// lifecycle.ResolveInDoubt and which demands an opauth.Grant. The previous
// revision exposed the whole Ledger through a Ledger() accessor, which made
// "operator-only" a convention rather than a boundary.

func (g *Guard) Commit(actionID string) error { return g.ledger.Commit(actionID) }
func (g *Guard) ReleaseConfirmedRejection(actionID string) error {
	return g.ledger.ReleaseConfirmedRejection(actionID)
}
func (g *Guard) MarkInDoubt(actionID string) error { return g.ledger.MarkInDoubt(actionID) }

func (g *Guard) State(actionID string) lifecycle.State { return g.ledger.State(actionID) }
func (g *Guard) Encumbered() int64                     { return g.ledger.Encumbered() }
func (g *Guard) Remaining() int64                      { return g.ledger.Remaining() }
func (g *Guard) Committed() int64                      { return g.ledger.Committed() }
func (g *Guard) InDoubtActions() []string              { return g.ledger.InDoubtActions() }

func deny(tool, rule, reason, actionID string) Decision {
	return Decision{Allowed: false, Rule: rule, Reason: reason, Tool: tool, MatchedActionID: actionID}
}

// Decide authorizes one tools/call.
//
// The whole match-and-reserve sequence is under g.mu, so two concurrent
// duplicates cannot both find the action Available.
func (g *Guard) Decide(tool string, args map[string]any, now time.Time) Decision {
	// 1. mandate validity
	if g.mandate.IsExpired(now) {
		return deny(tool, MandateExpired, fmt.Sprintf(
			"mandate %s expired at %s; call at %s", g.mandate.MandateID,
			g.mandate.ExpiresAt.Format(time.RFC3339), now.Format(time.RFC3339)), "")
	}

	// 2. build-level surface. Checked BEFORE the mandate, because a mandate can
	// only narrow this set. A dangerous tool listed in a mandate is a mandate
	// authoring error, and the guard must not honour it.
	if _, ok := supportedTools[tool]; !ok {
		return deny(tool, ToolNotSupported, fmt.Sprintf(
			"%s is outside the tool surface this guard supports (%v); a mandate "+
				"cannot widen it", tool, SupportedTools()), "")
	}

	// 3. mandate allowlist -- default-deny, unknown tools included
	if !g.mandate.PermitsTool(tool) {
		return deny(tool, ToolNotAllowed, fmt.Sprintf(
			"%s is not in allowed_tools %v", tool, g.mandate.AllowedTools), "")
	}

	// Permitted non-refund tools (reads) pass through untouched.
	if tool != RefundTool {
		return Decision{Allowed: true, Rule: Allowed, Tool: tool,
			Reason: fmt.Sprintf("%s is an allowed non-refund tool", tool), Forwarded: args}
	}

	// 3. argument typing
	rawPayment, ok := args["payment_id"]
	paymentID, isStr := rawPayment.(string)
	if !ok || !isStr || paymentID == "" {
		return deny(tool, MalformedArguments, fmt.Sprintf(
			"create_refund requires a string payment_id, got %#v", rawPayment), "")
	}
	amountPaise, err := parseAmountPaise(args["amount"])
	if err != nil {
		return deny(tool, MalformedArguments, err.Error(), "")
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// 4. action match
	forPayment := g.mandate.Find(paymentID)
	if len(forPayment) == 0 {
		return deny(tool, NoAuthorizedAction, fmt.Sprintf(
			"no authorized refund action exists for %s", paymentID), "")
	}
	var admitting []mandate.Action
	for _, a := range forPayment {
		if a.Admits(amountPaise) {
			admitting = append(admitting, a)
		}
	}
	if len(admitting) == 0 {
		descs := make([]string, 0, len(forPayment))
		for _, a := range forPayment {
			descs = append(descs, a.Describe())
		}
		return deny(tool, AmountNotAuthorized, fmt.Sprintf(
			"%d paise is not authorized for %s; actions: %s",
			amountPaise, paymentID, strings.Join(descs, ", ")), "")
	}
	var available []mandate.Action
	for _, a := range admitting {
		if g.ledger.IsAvailable(a.ActionID) {
			available = append(available, a)
		}
	}
	if len(available) == 0 {
		states := make([]string, 0, len(admitting))
		for _, a := range admitting {
			states = append(states, fmt.Sprintf("%s=%s", a.ActionID, g.ledger.State(a.ActionID)))
		}
		return deny(tool, ActionConsumed, fmt.Sprintf(
			"every action authorizing %d paise on %s is already used (%s); treated as a replay",
			amountPaise, paymentID, strings.Join(states, ", ")), admitting[0].ActionID)
	}
	action := pick(available)

	// 6. rate limit -- HEADROOM CHECK ONLY. The slot is recorded further down,
	// after the reservation succeeds. Consuming it here would let a request that
	// never reaches the child still burn rate capacity, causing avoidable false
	// blocks on the merchant's own legitimate traffic.
	if !g.rate.hasHeadroom(now) {
		return deny(tool, RateLimitExceeded, fmt.Sprintf(
			"%d calls already in the last 60s, limit is %d",
			g.rate.count(now), g.mandate.Limits.MaxCallsPerMinute), action.ActionID)
	}

	// 7. receipt is derived BEFORE reserving, because the durable reservation
	// records it and a reservation without a valid receipt must never exist.
	receipt, err := mandate.ReceiptFor(g.mandate.MandateID, action.ActionID)
	if err != nil {
		return deny(tool, MalformedArguments,
			fmt.Sprintf("receipt derivation failed: %v", err), action.ActionID)
	}

	// 8. atomic reserve -- action, budget and the durable row together, before
	// anything is written to the child.
	if err := g.ledger.Reserve(action.ActionID, receipt, amountPaise); err != nil {
		rule := CumulativeCapExceeded
		if errors.Is(err, lifecycle.ErrNotAvailable) {
			rule = ActionConsumed
		}
		return deny(tool, rule, err.Error(), action.ActionID)
	}

	// Only now is the rate slot consumed: this call really is going to the child.
	// If the durable write fails the reservation is rolled back, because a
	// forwarded call that is not in the rate window is a bypass.
	if err := g.rate.record(now); err != nil {
		_ = g.ledger.ReleaseConfirmedRejection(action.ActionID)
		return deny(tool, MalformedArguments,
			fmt.Sprintf("durable rate-window write failed, refusing to forward: %v", err),
			action.ActionID)
	}

	forwarded := make(map[string]any, len(args)+1)
	for k, v := range args {
		forwarded[k] = v
	}
	// Canonical integer, not the caller's literal: the forwarded amount is the
	// authorized amount by construction.
	forwarded["amount"] = amountPaise
	forwarded["receipt"] = receipt

	return Decision{
		Allowed: true, Rule: Allowed, Tool: tool,
		MatchedActionID: action.ActionID, Receipt: receipt,
		AuthorizedPaise: amountPaise, Forwarded: forwarded,
		Reason: fmt.Sprintf("matches %s; reserved %d paise (%d remaining of %d)",
			action.Describe(), amountPaise, g.ledger.Remaining(), g.ledger.MaxCumulativePaise()),
	}
}

// pick prefers an exact action over a bounded one, so a bounded grant is not
// spent on a refund an exact action already covers. Ties break on action_id so
// the decision log replays identically.
func pick(candidates []mandate.Action) mandate.Action {
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].IsBounded() != candidates[j].IsBounded() {
			return !candidates[i].IsBounded()
		}
		return candidates[i].ActionID < candidates[j].ActionID
	})
	return candidates[0]
}
