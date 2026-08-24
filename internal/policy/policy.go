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
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/harshith/rzp-guard/internal/lifecycle"
	"github.com/harshith/rzp-guard/internal/mandate"
)

const RefundTool = "create_refund"

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
		// Only reachable when a caller decoded without UseNumber.
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, errors.New("amount must be finite")
		}
		if n != math.Trunc(n) {
			return 0, fmt.Errorf("amount %v has a fractional part; Razorpay amounts "+
				"are integer paise", n)
		}
		if n > math.MaxInt64 || n < math.MinInt64 {
			return 0, fmt.Errorf("amount %v is out of range", n)
		}
		return int64(n), nil
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	default:
		return 0, fmt.Errorf("amount must be a JSON integer, got %T", v)
	}
}

type rateLimiter struct {
	mu    sync.Mutex
	max   int
	times []time.Time
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

// tryRecord checks and records under one lock, so concurrent callers cannot
// both observe headroom that only one of them can have.
func (r *rateLimiter) tryRecord(now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evictLocked(now)
	if len(r.times) >= r.max {
		return false
	}
	r.times = append(r.times, now)
	return true
}

// Guard is session-scoped authorization state, bound to the process lifetime.
// There is deliberately no method that accepts a replacement mandate.
type Guard struct {
	mu      sync.Mutex // serializes the match->reserve critical section
	mandate *mandate.Mandate
	ledger  *lifecycle.Ledger
	rate    *rateLimiter
}

func New(m *mandate.Mandate) *Guard {
	return &Guard{
		mandate: m,
		ledger:  lifecycle.NewLedger(m.Limits.MaxCumulativePaise),
		rate:    &rateLimiter{max: m.Limits.MaxCallsPerMinute},
	}
}

func (g *Guard) Mandate() *mandate.Mandate { return g.mandate }
func (g *Guard) Ledger() *lifecycle.Ledger { return g.ledger }

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

	// 2. tool allowlist -- default-deny, unknown tools included
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

	// 5. rate limit
	if !g.rate.tryRecord(now) {
		return deny(tool, RateLimitExceeded, fmt.Sprintf(
			"%d calls already in the last 60s, limit is %d",
			g.rate.count(now), g.mandate.Limits.MaxCallsPerMinute), action.ActionID)
	}

	// 6. atomic reserve -- action and budget together, before forwarding
	if err := g.ledger.Reserve(action.ActionID, amountPaise); err != nil {
		return deny(tool, CumulativeCapExceeded, err.Error(), action.ActionID)
	}

	// 7. receipt injection. Validated at mandate load; re-derived here.
	receipt, err := mandate.ReceiptFor(g.mandate.MandateID, action.ActionID)
	if err != nil {
		_ = g.ledger.ReleaseConfirmedRejection(action.ActionID)
		return deny(tool, MalformedArguments, fmt.Sprintf("receipt derivation failed: %v", err), action.ActionID)
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
