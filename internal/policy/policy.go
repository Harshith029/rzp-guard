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
	Allowed         bool   `json:"allowed"`
	Rule            string `json:"rule"`
	Reason          string `json:"reason"`
	Tool            string `json:"tool"`
	MatchedActionID string `json:"matched_action_id,omitempty"`
	// MatchedActionIDs is every action this one call consumes. Usually one.
	// MatchedActionID above stays the first of them, so the decision log keeps
	// the shape it has always had.
	MatchedActionIDs []string       `json:"matched_action_ids,omitempty"`
	Receipt          string         `json:"receipt,omitempty"`
	AuthorizedPaise  int64          `json:"authorized_paise,omitempty"`
	Forwarded        map[string]any `json:"-"`
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
// jsonInteger reports whether s is an integer exactly as RFC 8259 spells one:
// an optional minus, then either a single 0 or a non-zero digit followed by
// digits. No plus, no leading zeros, no spaces.
func jsonInteger(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' {
		s = s[1:]
	}
	if s == "" {
		return false
	}
	if s == "0" {
		return true
	}
	if s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

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
		// strconv, which Int64 uses, is MORE PERMISSIVE THAN JSON: it accepts a
		// leading "+" and leading zeros, so "+24000" and "024000" both parsed and
		// were authorized. Neither is a valid JSON number -- RFC 8259 gives
		// `-? (0 | [1-9][0-9]*)` -- so a compliant decoder would have rejected the
		// document before this function saw it, which is why nothing was reachable
		// through the relay. But the error above promises "a plain JSON integer",
		// and a check looser than the rule it states is the shape of every defect
		// in FAILURES.md that turned out to defend nothing.
		if !jsonInteger(s) {
			return 0, fmt.Errorf("amount %q is not a plain JSON integer in paise: a "+
				"leading plus or a leading zero is not valid JSON, and money is not "+
				"the place to be more forgiving than the wire format", s)
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

// The set forms. One forwarded refund may consume several actions, and they
// must settle together: a call that half-commits leaves the ledger disagreeing
// with itself about a refund that either happened or did not.
func (g *Guard) CommitMany(actionIDs []string) error { return g.ledger.CommitMany(actionIDs) }
func (g *Guard) ReleaseConfirmedRejectionMany(actionIDs []string) error {
	return g.ledger.ReleaseConfirmedRejectionMany(actionIDs)
}
func (g *Guard) MarkInDoubtMany(actionIDs []string) error {
	return g.ledger.MarkInDoubtMany(actionIDs)
}

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
		// No SINGLE action covers it. Before refusing, ask whether a set of the
		// merchant's own actions sums to exactly this amount -- the agent may
		// have issued one refund for two authorized items.
		if combo := combineExact(forPayment, amountPaise, g.ledger.IsAvailable); combo != nil {
			return g.reserveSet(tool, args, amountPaise, combo, now)
		}
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

	return g.reserveSet(tool, args, amountPaise, []mandate.Action{action}, now)
}

// reserveSet is the shared tail of every ALLOWED refund: rate check, receipt,
// atomic reservation, rate record, argument rewrite.
//
// One code path for one action and for several. The single-action case builds a
// one-element set, so a combined refund cannot drift from the ordering and
// rollback rules that took several revisions to get right.
//
// Caller holds g.mu.
func (g *Guard) reserveSet(tool string, args map[string]any, amountPaise int64,
	actions []mandate.Action, now time.Time) Decision {

	ids := make([]string, 0, len(actions))
	for _, a := range actions {
		ids = append(ids, a.ActionID)
	}
	sort.Strings(ids)
	first := ids[0]

	// 6. rate limit -- HEADROOM CHECK ONLY. The slot is recorded further down,
	// after the reservation succeeds. Consuming it here would let a request that
	// never reaches the child still burn rate capacity, causing avoidable false
	// blocks on the merchant's own legitimate traffic.
	//
	// A combined refund is ONE call and consumes ONE slot, because the rate
	// limit bounds calls to the provider, not actions consumed.
	if !g.rate.hasHeadroom(now) {
		return deny(tool, RateLimitExceeded, fmt.Sprintf(
			"%d calls already in the last 60s, limit is %d",
			g.rate.count(now), g.mandate.Limits.MaxCallsPerMinute), first)
	}

	// 7. receipt is derived BEFORE reserving, because the durable reservation
	// records it and a reservation without a valid receipt must never exist.
	receipt, err := mandate.ReceiptForSet(g.mandate.MandateID, ids)
	if err != nil {
		return deny(tool, MalformedArguments,
			fmt.Sprintf("receipt derivation failed: %v", err), first)
	}

	// 8. atomic reserve -- actions, budget and the durable rows together, before
	// anything is written to the child. Each action carries its OWN authorized
	// amount; for a combination those sum to the requested total by construction.
	rs := make([]lifecycle.Reservation, 0, len(actions))
	for _, a := range actions {
		amt := amountPaise
		if len(actions) > 1 {
			amt = *a.AmountPaise
		}
		rs = append(rs, lifecycle.Reservation{ActionID: a.ActionID, AmountPaise: amt})
	}
	if err := g.ledger.ReserveMany(receipt, rs); err != nil {
		rule := CumulativeCapExceeded
		if errors.Is(err, lifecycle.ErrNotAvailable) {
			rule = ActionConsumed
		}
		return deny(tool, rule, err.Error(), first)
	}

	// Only now is the rate slot consumed: this call really is going to the child.
	// If the durable write fails the reservation is rolled back, because a
	// forwarded call that is not in the rate window is a bypass.
	//
	// The rollback is attempted, not guaranteed: the rate write failing usually
	// means the store is broken, so the release will fail too. Then the actions
	// stay RESERVED, holding their budget, and recovery surfaces them as
	// IN_DOUBT at the next start. Nothing was forwarded, so that is a refund an
	// operator will be asked about that never left the building -- the
	// conservative error, and the right one to make. Releasing actions whose
	// release could not be durably recorded is the alternative, and that one can
	// be replayed.
	if err := g.rate.record(now); err != nil {
		_ = g.ledger.ReleaseConfirmedRejectionMany(ids)
		return deny(tool, MalformedArguments,
			fmt.Sprintf("durable rate-window write failed, refusing to forward: %v", err),
			first)
	}

	forwarded := make(map[string]any, len(args)+1)
	for k, v := range args {
		forwarded[k] = v
	}
	// Canonical integer, not the caller's literal: the forwarded amount is the
	// authorized amount by construction.
	forwarded["amount"] = amountPaise
	forwarded["receipt"] = receipt

	descs := make([]string, 0, len(actions))
	for _, a := range actions {
		descs = append(descs, a.Describe())
	}
	matched := strings.Join(descs, " + ")
	if len(actions) > 1 {
		matched = "combination of " + matched
	}

	return Decision{
		Allowed: true, Rule: Allowed, Tool: tool,
		MatchedActionID: first, MatchedActionIDs: ids, Receipt: receipt,
		AuthorizedPaise: amountPaise, Forwarded: forwarded,
		Reason: fmt.Sprintf("matches %s; reserved %d paise (%d remaining of %d)",
			matched, amountPaise, g.ledger.Remaining(), g.ledger.MaxCumulativePaise()),
	}
}

// combineExact finds a set of AVAILABLE exact-amount actions summing to target.
//
// WHY THIS EXISTS. A merchant who authorized 18500 for the dal and 19000 for
// the oil has authorized 37500 in total. An agent that issues one refund for
// 37500 instead of two is asking for exactly what was granted, and refusing it
// was three of arm B's nine false blocks -- the single largest measured cause
// of the guard obstructing legitimate refunds (study/FINDINGS-armB.md).
//
// WHY IT IS SAFE. Every action in the returned set is one the merchant granted,
// each is still consumed exactly once, and the total is the sum of their exact
// amounts. The cumulative cap is unchanged and checked against the total before
// anything is reserved. The maximum a compromised agent can extract is
// identical to what it could extract by issuing the calls separately; only the
// NUMBER of provider calls differs.
//
// BOUNDED ACTIONS ARE EXCLUDED. A bounded grant has no fixed amount, so "which
// part of the target did it cover" has no single answer, and a wrong split
// would consume more authority than the refund used. Bounded actions stay
// single-match, as before.
//
// The search is a depth-first walk with pruning, and it is CAPPED. Subset-sum
// is NP-hard; a mandate with many same-payment actions could otherwise make an
// agent-supplied amount expensive to refuse. Exhausting the budget returns no
// match, which denies the call -- the safe direction.
func combineExact(candidates []mandate.Action, target int64, avail func(string) bool) []mandate.Action {
	// Descending, so large actions are tried first and pruning bites early.
	pool := make([]mandate.Action, 0, len(candidates))
	for _, a := range candidates {
		if a.IsBounded() || !avail(a.ActionID) {
			continue
		}
		if *a.AmountPaise <= target {
			pool = append(pool, a)
		}
	}
	sort.Slice(pool, func(i, j int) bool {
		if *pool[i].AmountPaise != *pool[j].AmountPaise {
			return *pool[i].AmountPaise > *pool[j].AmountPaise
		}
		return pool[i].ActionID < pool[j].ActionID
	})
	// A single action is not a combination; Decide has already tried that.
	if len(pool) < 2 {
		return nil
	}

	const maxNodes = 50000
	const maxSetSize = 8

	// Suffix sums let the walk abandon a branch that cannot reach the target.
	suffix := make([]int64, len(pool)+1)
	for i := len(pool) - 1; i >= 0; i-- {
		suffix[i] = suffix[i+1] + *pool[i].AmountPaise
	}

	var best []mandate.Action
	var cur []mandate.Action
	nodes := 0

	var walk func(i int, sum int64)
	walk = func(i int, sum int64) {
		if best != nil || nodes > maxNodes {
			return
		}
		nodes++
		if sum == target {
			if len(cur) >= 2 {
				best = append([]mandate.Action(nil), cur...)
			}
			return
		}
		if i >= len(pool) || sum > target || len(cur) >= maxSetSize {
			return
		}
		if sum+suffix[i] < target {
			return // even taking everything left cannot reach it
		}
		cur = append(cur, pool[i])
		walk(i+1, sum+*pool[i].AmountPaise)
		cur = cur[:len(cur)-1]
		walk(i+1, sum)
	}
	walk(0, 0)
	return best
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
