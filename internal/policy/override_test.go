package policy

import (
	"errors"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/opgrant"
)

// stubGrants is a GrantSource a test controls, so the override path can be
// exercised without a database. calls counts reads, which is how the poll
// interval is asserted -- a cache nobody checks is a cache that quietly stops
// caching.
type stubGrants struct {
	grants []opgrant.Grant
	err    error
	calls  int
}

func (s *stubGrants) LiveGrants(string, time.Time) ([]opgrant.Grant, error) {
	s.calls++
	return s.grants, s.err
}

func aGrant(id, payment string, paise int64, expires time.Time) opgrant.Grant {
	return opgrant.Grant{
		GrantID: id, MandateID: "mnd_test", DenialID: 1,
		PaymentID: payment, AmountPaise: paise,
		IssuedAt: now, ExpiresAt: expires,
		Actor: "ops@merchant.example", Reason: "customer produced the receipt",
	}
}

// The behaviour everything else depends on: a refund the MANDATE refuses is
// forwarded when a human has approved exactly it.
func TestAnOperatorGrantAllowsARefundTheMandateRefused(t *testing.T) {
	g := New(mustMandate(t, `[{"action_id":"rfa_1","payment_id":"`+payA+`","amount_paise":1000}]`))

	args := map[string]any{"payment_id": payB, "amount": int64(7500)}
	if d := g.Decide(RefundTool, args, now); d.Allowed || d.Rule != NoAuthorizedAction {
		t.Fatalf("precondition: want NO_AUTHORIZED_ACTION, got %s allowed=%v", d.Rule, d.Allowed)
	}

	g.SetGrantSource(&stubGrants{grants: []opgrant.Grant{
		aGrant("opg_00000000000000aa", payB, 7500, now.Add(time.Hour)),
	}})

	d := g.Decide(RefundTool, args, now)
	if !d.Allowed {
		t.Fatalf("a refund a human explicitly approved was still refused: %s", d.Reason)
	}
	if d.Rule != OperatorApproved {
		t.Errorf("rule = %s, want %s: the log must distinguish what the merchant "+
			"authorized from what support did", d.Rule, OperatorApproved)
	}
	if d.OperatorGrantID != "opg_00000000000000aa" {
		t.Errorf("grant id = %q, want it recorded on the decision", d.OperatorGrantID)
	}
	if amt, ok := d.ForwardedAmountPaise(); !ok || amt != 7500 {
		t.Errorf("forwarded %d (ok=%v), want the exact approved amount", amt, ok)
	}
	if d.Receipt == "" {
		t.Error("no receipt: a forwarded refund with no correlation key cannot be " +
			"looked up during an incident")
	}
}

// SINGLE USE. A grant is an ordinary ledger row, so the second attempt meets
// the same ACTION_CONSUMED wall every other authorization does. If this ever
// stops holding, one approval becomes unlimited refunds of that amount.
func TestAnOperatorGrantIsSingleUse(t *testing.T) {
	g := New(mustMandate(t, `[{"action_id":"rfa_1","payment_id":"`+payA+`","amount_paise":1000}]`))
	g.SetGrantSource(&stubGrants{grants: []opgrant.Grant{
		aGrant("opg_00000000000000bb", payB, 7500, now.Add(time.Hour)),
	}})
	args := map[string]any{"payment_id": payB, "amount": int64(7500)}

	if d := g.Decide(RefundTool, args, now); !d.Allowed {
		t.Fatalf("first use refused: %s", d.Reason)
	}
	if err := g.Commit("opg_00000000000000bb"); err != nil {
		t.Fatal(err)
	}
	d := g.Decide(RefundTool, args, now)
	if d.Allowed {
		t.Fatal("one operator approval authorized a second refund of the same amount")
	}
	if d.Rule != NoAuthorizedAction {
		t.Errorf("rule = %s; the mandate's own refusal must come back once the "+
			"grant is spent", d.Rule)
	}
}

func TestGrantScope(t *testing.T) {
	cases := []struct {
		name    string
		grant   opgrant.Grant
		payment string
		amount  int64
		want    bool
	}{
		{
			name:    "the exact refund that was refused",
			grant:   aGrant("opg_00000000000000c1", payB, 7500, now.Add(time.Hour)),
			payment: payB, amount: 7500, want: true,
		},
		{
			// No tolerance window. A grant that admits "about" the approved figure
			// is a bounded grant wearing an exact grant's clothes.
			name:    "one paise more than approved",
			grant:   aGrant("opg_00000000000000c2", payB, 7500, now.Add(time.Hour)),
			payment: payB, amount: 7501, want: false,
		},
		{
			name:    "less than approved is still not what was approved",
			grant:   aGrant("opg_00000000000000c3", payB, 7500, now.Add(time.Hour)),
			payment: payB, amount: 7499, want: false,
		},
		{
			name:    "a different payment",
			grant:   aGrant("opg_00000000000000c4", payB, 7500, now.Add(time.Hour)),
			payment: "pay_SYN9999", amount: 7500, want: false,
		},
		{
			// No grace period either. An expired grant is authority nobody is
			// watching any more, which is the whole reason it expires.
			name:    "expired one nanosecond ago",
			grant:   aGrant("opg_00000000000000c5", payB, 7500, now),
			payment: payB, amount: 7500, want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := New(mustMandate(t, `[{"action_id":"rfa_1","payment_id":"`+payA+`","amount_paise":1000}]`))
			g.SetGrantSource(&stubGrants{grants: []opgrant.Grant{tc.grant}})
			d := g.Decide(RefundTool,
				map[string]any{"payment_id": tc.payment, "amount": tc.amount}, now)
			if d.Allowed != tc.want {
				t.Fatalf("allowed=%v want %v (rule %s): %s", d.Allowed, tc.want, d.Rule, d.Reason)
			}
		})
	}
}

// WHAT A GRANT MUST NOT REACH. Each of these refusals is a decision that is not
// support's to make, and the override list is the place that is enforced.
func TestSomeRefusalsAreNotOverridable(t *testing.T) {
	// An expired mandate: the whole grant lapsed, and the answer is a new mandate
	// from the merchant, not a patch from support. Overriding it would make
	// expiry advisory.
	t.Run("an expired mandate", func(t *testing.T) {
		g := New(mustMandate(t,
			`[{"action_id":"rfa_1","payment_id":"`+payA+`","amount_paise":1000}]`,
			expires(now.Add(-time.Hour))))
		g.SetGrantSource(&stubGrants{grants: []opgrant.Grant{
			aGrant("opg_00000000000000d1", payA, 1000, now.Add(time.Hour)),
		}})
		d := g.Decide(RefundTool, map[string]any{"payment_id": payA, "amount": int64(1000)}, now)
		if d.Allowed || d.Rule != MandateExpired {
			t.Fatalf("an expired mandate was overridden: allowed=%v rule=%s", d.Allowed, d.Rule)
		}
	})

	// A tool this build does not forward. An operator who can widen the tool
	// surface can forward anything, which is a different power entirely.
	t.Run("an unsupported tool", func(t *testing.T) {
		g := New(mustMandate(t, `[{"action_id":"rfa_1","payment_id":"`+payA+`","amount_paise":1000}]`))
		g.SetGrantSource(&stubGrants{grants: []opgrant.Grant{
			aGrant("opg_00000000000000d2", payA, 1000, now.Add(time.Hour)),
		}})
		d := g.Decide("create_payment_link", map[string]any{"payment_id": payA, "amount": int64(1000)}, now)
		if d.Allowed {
			t.Fatal("an operator grant widened the build's tool surface")
		}
	})

	// The merchant's own ceiling. Support correcting a wrong refusal is one
	// thing; support raising the limit the merchant set is another, and it is
	// the boundary that must need the merchant.
	t.Run("the cumulative cap", func(t *testing.T) {
		g := New(mustMandate(t,
			`[{"action_id":"rfa_1","payment_id":"`+payA+`","amount_paise":1000}]`,
			cumulative(5000)))
		g.SetGrantSource(&stubGrants{grants: []opgrant.Grant{
			aGrant("opg_00000000000000d3", payB, 900000, now.Add(time.Hour)),
		}})
		d := g.Decide(RefundTool, map[string]any{"payment_id": payB, "amount": int64(900000)}, now)
		if d.Allowed {
			t.Fatal("an operator grant spent past the merchant's cumulative cap")
		}
		if d.Rule != CumulativeCapExceeded {
			t.Errorf("rule = %s, want %s: the operator must see that it was the cap "+
				"that refused, not their approval", d.Rule, CumulativeCapExceeded)
		}
	})
}

// A guard with no grant source must decide exactly as it always did. Every test
// written before this feature existed relies on that, and so does every
// deployment that never issues a grant.
func TestWithoutAGrantSourceNothingChanges(t *testing.T) {
	g := New(mustMandate(t, `[{"action_id":"rfa_1","payment_id":"`+payA+`","amount_paise":1000}]`))
	d := g.Decide(RefundTool, map[string]any{"payment_id": payB, "amount": int64(7500)}, now)
	if d.Allowed || d.Rule != NoAuthorizedAction {
		t.Fatalf("allowed=%v rule=%s", d.Allowed, d.Rule)
	}
	if d.OperatorGrantID != "" {
		t.Error("a grant id appeared on a decision made without a grant source")
	}
}

// The cache is what keeps a refusal cheap. Without it an agent looping on a
// refused call would drive one SQLite query per attempt, which hands an
// untrusted party a way to make refusals expensive -- and "a denial costs 779
// nanoseconds" is the reason refusals are safe to leave unbounded.
func TestTheGrantSourceIsPolledAtMostOncePerInterval(t *testing.T) {
	src := &stubGrants{}
	g := New(mustMandate(t, `[{"action_id":"rfa_1","payment_id":"`+payA+`","amount_paise":1000}]`))
	g.SetGrantSource(src)

	args := map[string]any{"payment_id": payB, "amount": int64(7500)}
	for i := 0; i < 50; i++ {
		g.Decide(RefundTool, args, now)
	}
	if src.calls != 1 {
		t.Fatalf("50 refusals within one poll interval read the source %d times, want 1", src.calls)
	}
	// Past the interval it refreshes, or an operator's grant would never be seen.
	g.Decide(RefundTool, args, now.Add(2*grantPoll))
	if src.calls != 2 {
		t.Fatalf("after %v the source was read %d times, want 2: a grant issued "+
			"now must be honoured without restarting the guard", 2*grantPoll, src.calls)
	}
}

// A transient read failure must not silently withdraw an authorization a human
// issued. Emptying the set on error would refuse the retry with a rule that
// says nothing about the real cause.
func TestAFailedGrantReadKeepsTheLastKnownSet(t *testing.T) {
	src := &stubGrants{grants: []opgrant.Grant{
		aGrant("opg_00000000000000ee", payB, 7500, now.Add(time.Hour)),
	}}
	g := New(mustMandate(t, `[{"action_id":"rfa_1","payment_id":"`+payA+`","amount_paise":1000}]`))
	g.SetGrantSource(src)
	args := map[string]any{"payment_id": payB, "amount": int64(7500)}

	if d := g.Decide(RefundTool, args, now); !d.Allowed {
		t.Fatalf("precondition: %s", d.Reason)
	}
	// Release it so the same grant is available again, then break the source.
	if err := g.ReleaseConfirmedRejection("opg_00000000000000ee"); err != nil {
		t.Fatal(err)
	}
	src.err = errors.New("database is locked")

	if d := g.Decide(RefundTool, args, now.Add(2*grantPoll)); !d.Allowed {
		t.Fatalf("a database hiccup withdrew a grant a human issued: %s", d.Reason)
	}
	if g.GrantSourceError() == nil {
		t.Error("the failure is not reported anywhere; a guard that has stopped " +
			"seeing grants looks identical to one nobody has issued any to")
	}
}
