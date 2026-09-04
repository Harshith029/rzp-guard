package policy

import (
	"sync"
	"time"

	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/opgrant"
)

// GrantSource is where live operator grants come from.
//
// It is READ ON THE REFUSAL PATH, not loaded at startup. That is the property
// the whole unblock workflow rests on: a guard which started an hour ago must
// see a grant issued a moment ago, or an operator would have to restart the
// payment proxy to unstick one refund -- which is exactly the workflow nobody
// was ever going to perform, and the reason the published false-positive cost
// model had nothing behind it.
type GrantSource interface {
	LiveGrants(mandateID string, now time.Time) ([]opgrant.Grant, error)
}

// grantPoll bounds how often the source is consulted.
//
// WHY A CACHE AT ALL. A denial costs 779 nanoseconds, and that number is the
// reason refusals are effectively unbounded: an agent looping on a refused call
// cannot cost the guard anything. Reading SQLite on every refusal would put a
// query on that path and hand an untrusted party a way to make refusals
// expensive. Polling at most this often keeps the refusal path free in the
// steady state and bounds the database load a hostile agent can create to one
// query a second, whatever it does.
//
// WHY THE LATENCY IS ACCEPTABLE. It is at most one second between an operator
// issuing a grant and the guard honouring it, and the agent retries. A support
// interaction does not notice a second; a benchmark does.
const grantPoll = time.Second

// grantCache holds the last read and when it happened.
type grantCache struct {
	mu       sync.Mutex
	at       time.Time
	grants   []opgrant.Grant
	lastErr  error
	consumed map[string]bool
}

// SetGrantSource installs the operator-grant source. Call at startup, before
// traffic. A Guard with no source behaves exactly as it always has: the
// override path cannot fire, and Decide is the same function it was.
func (g *Guard) SetGrantSource(src GrantSource) {
	g.grants = src
	g.grantCache = &grantCache{consumed: map[string]bool{}}
}

// liveGrants returns the cached grant set, refreshing at most once per
// grantPoll. Caller holds g.mu.
func (g *Guard) liveGrants(now time.Time) []opgrant.Grant {
	if g.grants == nil || g.grantCache == nil {
		return nil
	}
	c := g.grantCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && now.Sub(c.at) < grantPoll {
		return c.grants
	}
	c.at = now
	grants, err := g.grants.LiveGrants(g.mandate.MandateID, now)
	// A failed read leaves the PREVIOUS set in place rather than emptying it.
	//
	// Emptying would turn a transient database error into the silent withdrawal
	// of an authorization a human issued, and the agent's retry would be refused
	// with a rule that says nothing about the real cause. Keeping the stale set
	// is bounded by the grant's own expiry, which is the shorter leash.
	if err != nil {
		c.lastErr = err
		return c.grants
	}
	c.lastErr = nil
	c.grants = grants
	return grants
}

// GrantSourceError reports the last failure reading grants, for the status
// document and the health endpoint. A guard that has quietly stopped seeing
// operator grants looks identical to one nobody has issued any to, and those
// are very different situations during an incident.
func (g *Guard) GrantSourceError() error {
	if g.grantCache == nil {
		return nil
	}
	g.grantCache.mu.Lock()
	defer g.grantCache.mu.Unlock()
	return g.grantCache.lastErr
}

// overridableRules are the refusals a human may correct.
//
// The list is short on purpose, and what is ABSENT from it is the design.
//
//	NO_AUTHORIZED_ACTION and AMOUNT_NOT_AUTHORIZED are the mandate being
//	narrower than the merchant's actual intent. That is an authoring mistake,
//	it is what the false-positive rate measures, and it is precisely the case a
//	person can look at and settle.
//
//	ACTION_CONSUMED is a legitimate second refund of the same figure meeting a
//	single-use authorization. A human can tell that from a replay; the guard
//	structurally cannot.
//
//	MANDATE_EXPIRED is NOT here. An expired mandate means the whole grant
//	lapsed, and the answer is a new mandate from the merchant, not a patch from
//	support. Allowing it would make expiry advisory.
//
//	TOOL_NOT_SUPPORTED and TOOL_NOT_ALLOWED are NOT here. The tool surface is a
//	property of this build and of the merchant's grant, not an incident
//	decision, and an operator who can widen it can forward anything.
//
//	MALFORMED_ARGUMENTS is NOT here. The guard could not understand the request;
//	approving one it could not parse would authorize something nobody has read.
//
//	RATE_LIMIT_EXCEEDED and CUMULATIVE_CAP_EXCEEDED are NOT here. Those are the
//	merchant's own ceilings. A support desk correcting a wrong refusal is one
//	thing; a support desk raising the limit the merchant set is another, and it
//	is the one boundary that must need the merchant.
var overridableRules = map[string]struct{}{
	NoAuthorizedAction:  {},
	AmountNotAuthorized: {},
	ActionConsumed:      {},
}

// operatorOverride gives a refused refund one last chance against the grants a
// human has issued.
//
// It runs AFTER the mandate has refused, never before, so a call the mandate
// authorizes never touches this path and the ordinary decision ladder is
// unchanged. Caller holds g.mu.
func (g *Guard) operatorOverride(tool string, args map[string]any, paymentID string,
	amountPaise int64, now time.Time, denied Decision) Decision {

	if g.grants == nil {
		return denied
	}
	if _, ok := overridableRules[denied.Rule]; !ok {
		return denied
	}
	for _, gr := range g.liveGrants(now) {
		if !gr.Admits(paymentID, amountPaise, now) {
			continue
		}
		// Single use, enforced by the ledger rather than by this loop: the grant
		// becomes an ordinary action row and the reservation below fails if it
		// has already been spent. Checking availability first only avoids
		// building a decision that would be thrown away.
		if !g.ledger.IsAvailable(gr.GrantID) {
			continue
		}
		// One action, exact amount, through the SAME reserve path as every
		// mandate action. There is no second money path: the durable write, the
		// receipt, the cumulative cap, the rate limit and the lifecycle are all
		// the ones that were already there.
		amt := gr.AmountPaise
		act := mandate.Action{
			ActionID:    gr.GrantID,
			PaymentID:   gr.PaymentID,
			AmountPaise: &amt,
		}
		d := g.reserveSet(tool, args, amountPaise, []mandate.Action{act}, now)
		if !d.Allowed {
			// The cap or the rate limit refused it. That refusal STANDS: those are
			// the merchant's own ceilings and an operator grant is not a way past
			// them. Return it rather than the original, because "cumulative cap
			// exceeded" is the true reason now and the operator needs to see that
			// their approval was not what failed.
			return d
		}
		d.Rule = OperatorApproved
		d.OperatorGrantID = gr.GrantID
		d.PaymentID = paymentID
		d.RequestedPaise = amountPaise
		d.Reason = "the mandate refused this (" + denied.Rule + "); allowed by " +
			gr.Describe() + " -- reason given: " + gr.Reason
		return d
	}
	return denied
}
