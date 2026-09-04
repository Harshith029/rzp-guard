// Package opgrant models a single-use authorization issued by a human for one
// refund the guard refused.
//
// WHY IT EXISTS. The guard's published false-positive rate is 0.455. Blocking
// 45% of legitimate refunds is survivable only if somebody unblocks them, and
// the cost model that prices those blocks assumes exactly that. Nothing
// provided it. A refused refund went to stderr, the mandate could not be
// changed without restarting the process, and the operator tool could not even
// open the state file while the guard held it. The escape hatch the economics
// depend on did not exist.
//
// WHAT A GRANT IS. One payment, one exact amount, one use, expiring soon,
// attributed to a named person with a stated reason. It is minted only by
// rzp-guard-operator, only against a refusal already recorded in the denial
// queue, and only by a caller holding an opauth.Grant -- the same Argon2id
// credential that authorises resolving an IN_DOUBT refund. Those are the same
// kind of decision, a person accepting responsibility for money, and they
// should not have different doors.
//
// WHAT IT DELIBERATELY IS NOT.
//
//	NOT BOUNDED. There is no max_amount form. A bound is discretion, and
//	unblocking one wrongly-refused refund is not the moment to delegate a
//	figure to the agent that just had one refused.
//
//	NOT A MANDATE EDIT. The mandate is loaded once at startup and nothing can
//	replace it; that property is load-bearing and is not weakened here. A grant
//	is a separate, narrower object that the guard consults only AFTER the
//	mandate has refused.
//
//	NOT A WAY PAST THE CUMULATIVE CAP. A grant becomes an ordinary row in the
//	action ledger and is reserved through the same path as any mandate action,
//	so the merchant's own ceiling still applies. An operator can correct a wrong
//	refusal; an operator cannot raise the merchant's limit alone. That is the
//	one boundary a support desk should not be able to move.
//
//	NOT A WAY PAST AN EXPIRED MANDATE. When the whole grant has lapsed, the
//	answer is a new mandate from the merchant, not a patch from support.
package opgrant

import (
	"fmt"
	"regexp"
	"time"
)

// IDPattern is the same shape mandate action ids must match, because a grant
// BECOMES an action id: it is reserved, committed and resolved through exactly
// the same ledger rows, and it ends up inside a provider-side receipt.
var IDPattern = regexp.MustCompile(`^opg_[a-f0-9]{16}$`)

// MaxTTL bounds how long a grant may live.
//
// A grant that outlives the incident is standing authority nobody revisits,
// which is the failure the mandate's own expiry exists to prevent -- and it
// would be a worse version of it, because a grant is invisible in the mandate
// file a reviewer reads. An hour is longer than any support interaction and far
// shorter than a shift.
const MaxTTL = time.Hour

// DefaultTTL is what an operator gets without saying otherwise. Fifteen minutes
// is the length of the conversation the unblock is happening inside.
const DefaultTTL = 15 * time.Minute

// Grant is one issued authorization.
type Grant struct {
	GrantID   string `json:"grant_id"`
	MandateID string `json:"mandate_id"`
	DenialID  int64  `json:"denial_id"`

	PaymentID string `json:"payment_id"`
	// AmountPaise is EXACT. A grant admits this figure and no other.
	AmountPaise int64 `json:"amount_paise"`

	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Actor     string    `json:"actor"`
	Reason    string    `json:"reason"`
}

// Admits reports whether this grant authorizes exactly this refund, right now.
//
// Every clause is an equality or a strict time bound. There is no tolerance
// window on the amount and no grace period on the expiry, because both are the
// kind of small kindness that turns a narrow authorization into a broad one.
func (g Grant) Admits(paymentID string, amountPaise int64, now time.Time) bool {
	return g.PaymentID == paymentID &&
		g.AmountPaise == amountPaise &&
		now.Before(g.ExpiresAt)
}

// Describe renders a grant for a decision log and an operator's screen.
func (g Grant) Describe() string {
	return fmt.Sprintf("%s (%d paise on %s, issued by %s, expires %s)",
		g.GrantID, g.AmountPaise, g.PaymentID, g.Actor,
		g.ExpiresAt.UTC().Format(time.RFC3339))
}

// Validate refuses a grant that could not be honoured or could not be audited.
//
// It runs at ISSUE time, in the operator tool, so a grant that the guard would
// have to refuse is never written. A grant sitting in the database that can
// never fire is worse than no grant: an operator believes the refund is
// unblocked, and nothing says otherwise until the customer calls again.
func (g Grant) Validate(now time.Time) error {
	switch {
	case !IDPattern.MatchString(g.GrantID):
		return fmt.Errorf("grant id %q must match %s", g.GrantID, IDPattern)
	case g.MandateID == "":
		return fmt.Errorf("grant %s has no mandate", g.GrantID)
	case g.PaymentID == "":
		return fmt.Errorf("grant %s has no payment", g.GrantID)
	case g.AmountPaise <= 0:
		return fmt.Errorf("grant %s: amount must be positive, got %d",
			g.GrantID, g.AmountPaise)
	case g.Actor == "":
		return fmt.Errorf("grant %s has no operator; an unattributed grant is not "+
			"a decision anyone can be held to", g.GrantID)
	case g.Reason == "":
		return fmt.Errorf("grant %s has no reason; the reason is what a later "+
			"reviewer reads to decide whether this should have been issued", g.GrantID)
	case !g.ExpiresAt.After(now):
		return fmt.Errorf("grant %s expires at %s, which is not in the future",
			g.GrantID, g.ExpiresAt.Format(time.RFC3339))
	case g.ExpiresAt.Sub(now) > MaxTTL:
		return fmt.Errorf("grant %s would live %s, beyond the %s ceiling. A grant "+
			"that outlives the incident is standing authority nobody revisits, and "+
			"it is invisible in the mandate a reviewer reads",
			g.GrantID, g.ExpiresAt.Sub(now).Truncate(time.Second), MaxTTL)
	}
	return nil
}
