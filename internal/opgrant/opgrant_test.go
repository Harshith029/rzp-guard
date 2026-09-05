package opgrant

import (
	"strings"
	"testing"
	"time"
)

// Validate guards the grant-issuing path in internal/storage/unblock.go, and
// until this file existed no test called it. Every clause below is the only
// thing standing between an operator's mistake and a grant that overrides a
// refusal on the money path, so each one is asserted individually rather than
// through a single happy-path smoke test.

var now = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)

func good() Grant {
	return Grant{
		GrantID:     "opg_0123456789abcdef",
		MandateID:   "mnd_test",
		DenialID:    1,
		PaymentID:   "pay_SYN0001",
		AmountPaise: 20000,
		IssuedAt:    now,
		ExpiresAt:   now.Add(30 * time.Minute),
		Actor:       "ops@merchant",
		Reason:      "customer confirmed the partial refund by phone",
	}
}

func TestAValidGrantPasses(t *testing.T) {
	if err := good().Validate(now); err != nil {
		t.Fatalf("the reference grant must validate: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*Grant)
		want   string
	}{
		{"an id that is not opg_ + 16 hex", func(g *Grant) { g.GrantID = "opg_short" }, "must match"},
		{"an id with the wrong prefix", func(g *Grant) { g.GrantID = "rfa_0123456789abcdef" }, "must match"},
		{"no mandate", func(g *Grant) { g.MandateID = "" }, "no mandate"},
		{"no payment", func(g *Grant) { g.PaymentID = "" }, "no payment"},
		{"a zero amount", func(g *Grant) { g.AmountPaise = 0 }, "must be positive"},
		{"a negative amount", func(g *Grant) { g.AmountPaise = -1 }, "must be positive"},
		{"no operator", func(g *Grant) { g.Actor = "" }, "no operator"},
		{"no reason", func(g *Grant) { g.Reason = "" }, "no reason"},
		{"an expiry in the past", func(g *Grant) { g.ExpiresAt = now.Add(-time.Second) }, "not in the future"},
		{"an expiry of exactly now", func(g *Grant) { g.ExpiresAt = now }, "not in the future"},
		{"a ttl beyond the ceiling", func(g *Grant) { g.ExpiresAt = now.Add(MaxTTL + time.Second) }, "beyond the"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := good()
			tc.break_(&g)
			err := g.Validate(now)
			if err == nil {
				t.Fatalf("accepted a grant with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refused for the wrong reason.\n  got:  %v\n  want: contains %q", err, tc.want)
			}
		})
	}
}

// The ceiling is a boundary, so both sides of it are pinned. A grant that lives
// exactly MaxTTL is the longest an operator may issue; one second more is
// standing authority nobody revisits.
func TestTheTTLCeilingIsInclusive(t *testing.T) {
	g := good()
	g.ExpiresAt = now.Add(MaxTTL)
	if err := g.Validate(now); err != nil {
		t.Fatalf("a grant living exactly MaxTTL must be allowed: %v", err)
	}
}

// Admits is what the refusal path calls. It is deliberately exact on both the
// payment and the amount: a grant is not a budget.
func TestAdmitsIsExactOnPaymentAmountAndTime(t *testing.T) {
	g := good()
	for _, tc := range []struct {
		name    string
		payment string
		amount  int64
		at      time.Time
		want    bool
	}{
		{"the grant as issued", "pay_SYN0001", 20000, now, true},
		{"a different payment", "pay_SYN0002", 20000, now, false},
		{"one paise less", "pay_SYN0001", 19999, now, false},
		{"one paise more", "pay_SYN0001", 20001, now, false},
		{"after it expired", "pay_SYN0001", 20000, g.ExpiresAt.Add(time.Second), false},
		{"at the instant it expires", "pay_SYN0001", 20000, g.ExpiresAt, false},
		{"a moment before expiry", "pay_SYN0001", 20000, g.ExpiresAt.Add(-time.Nanosecond), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.Admits(tc.payment, tc.amount, tc.at); got != tc.want {
				t.Fatalf("Admits(%q, %d, %s) = %v, want %v",
					tc.payment, tc.amount, tc.at.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

// Describe goes into the decision reason an operator reads later, so it must
// name the grant and carry the figure it authorized.
func TestDescribeNamesTheGrantAndTheAmount(t *testing.T) {
	d := good().Describe()
	for _, want := range []string{"opg_0123456789abcdef", "20000"} {
		if !strings.Contains(d, want) {
			t.Fatalf("Describe() = %q, which does not contain %q", d, want)
		}
	}
}
