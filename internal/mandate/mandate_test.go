package mandate

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func mustLoad(t *testing.T, m map[string]any) *Mandate {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	md, err := Load(b)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return md
}

func base() map[string]any {
	return map[string]any{
		"mandate_id":    "mnd_test",
		"expires_at":    "2030-01-01T00:00:00Z",
		"allowed_tools": []string{"fetch_payment", "create_refund"},
		"authorized_refund_actions": []map[string]any{
			{"action_id": "rfa_001", "payment_id": "pay_SYN0001", "amount_paise": 24000},
		},
		"global": map[string]any{"max_cumulative_paise": 100000, "max_calls_per_minute": 20},
	}
}

// The receipt is a PROVIDER-SIDE idempotency key, so its properties are not
// cosmetic. Determinism is what lets a replay be recognised; the length floor is
// what stops the provider rejecting it outright.
func TestReceiptIsDeterministicAndClearsTheFloor(t *testing.T) {
	a, err := ReceiptFor("mnd_test", "rfa_001")
	if err != nil {
		t.Fatalf("ReceiptFor: %v", err)
	}
	b, _ := ReceiptFor("mnd_test", "rfa_001")
	if a != b {
		t.Fatalf("not deterministic: %q then %q — a replay could not be recognised", a, b)
	}

	if !strings.HasPrefix(a, receiptPrefix) {
		t.Fatalf("receipt %q lacks the %q prefix", a, receiptPrefix)
	}
	if len(a) < receiptMinLen {
		t.Fatalf("receipt %q is %d chars, below the documented %d floor — an early "+
			"version produced the 6-character \"rzpg_a\"", a, len(a), receiptMinLen)
	}
	if got := len(a) - len(receiptPrefix); got != receiptHashLen {
		t.Fatalf("hash segment is %d chars, want %d", got, receiptHashLen)
	}
	if !receiptPattern.MatchString(a) {
		t.Fatalf("receipt %q contains characters the provider does not accept", a)
	}
}

// Distinct actions must not collide, and the mandate id must participate --
// otherwise the same action id in two mandates would produce one key.
func TestReceiptSeparatesActionsAndMandates(t *testing.T) {
	seen := map[string]string{}
	for _, mid := range []string{"mnd_a", "mnd_b"} {
		for i := 0; i < 200; i++ {
			aid := fmt.Sprintf("rfa_%03d", i)
			r, err := ReceiptFor(mid, aid)
			if err != nil {
				t.Fatalf("ReceiptFor(%s,%s): %v", mid, aid, err)
			}
			key := mid + "/" + aid
			if prev, dup := seen[r]; dup {
				t.Fatalf("receipt collision: %s and %s both produced %s", prev, key, r)
			}
			seen[r] = key
		}
	}
}

// A fractional amount is expressible because Razorpay declares amount as
// {"type":"number"}. Admits must not round, floor or otherwise accept it:
// FAILURES.md F1 records 50000.9 being authorized against an action for 50000.
func TestActionAdmitsExactAmountsOnly(t *testing.T) {
	exact := int64(50000)
	a := Action{ActionID: "rfa_001", PaymentID: "pay_x", AmountPaise: &exact}

	if !a.Admits(50000) {
		t.Fatal("the exact authorized amount must be admitted")
	}
	for _, bad := range []int64{49999, 50001, 0, -50000} {
		if a.Admits(bad) {
			t.Errorf("Admits(%d) on an action for exactly 50000", bad)
		}
	}
	if a.IsBounded() {
		t.Fatal("an exact-amount action is not a bounded range")
	}
	if a.Ceiling() != 50000 {
		t.Fatalf("Ceiling = %d, want 50000", a.Ceiling())
	}
}

func TestBoundedActionAdmitsUpToTheCeiling(t *testing.T) {
	max := int64(50000)
	a := Action{ActionID: "rfa_002", PaymentID: "pay_x", MaxAmountPaise: &max}

	if !a.IsBounded() {
		t.Fatal("a max-amount action is bounded")
	}
	for _, ok := range []int64{100, 25000, 50000} {
		if !a.Admits(ok) {
			t.Errorf("Admits(%d) = false, want true up to the ceiling", ok)
		}
	}
	for _, bad := range []int64{50001, 0, -1} {
		if a.Admits(bad) {
			t.Errorf("Admits(%d) = true, above the ceiling or non-positive", bad)
		}
	}
}

// Load is the authorization boundary's parser. Anything it accepts becomes
// policy, so its refusals matter more than its successes.
func TestLoadRefusesMalformedMandates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"no mandate id", func(m map[string]any) { delete(m, "mandate_id") }},
		{"no expiry", func(m map[string]any) { delete(m, "expires_at") }},
		{"duplicate action ids", func(m map[string]any) {
			m["authorized_refund_actions"] = []map[string]any{
				{"action_id": "dup", "payment_id": "pay_a", "amount_paise": 100},
				{"action_id": "dup", "payment_id": "pay_b", "amount_paise": 200},
			}
		}},
		{"action with no payment", func(m map[string]any) {
			m["authorized_refund_actions"] = []map[string]any{
				{"action_id": "rfa_x", "amount_paise": 100},
			}
		}},
		{"amount below the 100 paise floor", func(m map[string]any) {
			m["authorized_refund_actions"] = []map[string]any{
				{"action_id": "rfa_x", "payment_id": "pay_a", "amount_paise": 99},
			}
		}},
		{"negative amount", func(m map[string]any) {
			m["authorized_refund_actions"] = []map[string]any{
				{"action_id": "rfa_x", "payment_id": "pay_a", "amount_paise": -500},
			}
		}},
		{"both exact and max amount", func(m map[string]any) {
			m["authorized_refund_actions"] = []map[string]any{
				{"action_id": "rfa_x", "payment_id": "pay_a",
					"amount_paise": 500, "max_amount_paise": 900},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.mutate(m)
			b, _ := json.Marshal(m)
			if _, err := Load(b); err == nil {
				t.Fatal("accepted; a malformed mandate must be refused, not " +
					"partially honoured")
			}
		})
	}
}

func TestLoadRefusesNonJSON(t *testing.T) {
	if _, err := Load([]byte("{not json")); err == nil {
		t.Fatal("unparseable input must be refused")
	}
}

func TestPermitsToolAndExpiry(t *testing.T) {
	m := mustLoad(t, base())

	if !m.PermitsTool("create_refund") || !m.PermitsTool("fetch_payment") {
		t.Fatal("listed tools must be permitted")
	}
	if m.PermitsTool("create_payout") {
		t.Fatal("an unlisted tool must not be permitted")
	}
	if m.PermitsTool("") {
		t.Fatal("the empty tool name must not be permitted")
	}

	if m.IsExpired(time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("a mandate before its expiry is not expired")
	}
	if !m.IsExpired(time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("a mandate after its expiry is expired")
	}
	// The boundary is inclusive: at the instant of expiry it is expired.
	if !m.IsExpired(m.ExpiresAt) {
		t.Fatal("a mandate is expired at exactly its expiry instant")
	}
}

func TestFindMatchesOnlyTheNamedPayment(t *testing.T) {
	m := base()
	m["authorized_refund_actions"] = []map[string]any{
		{"action_id": "rfa_001", "payment_id": "pay_A", "amount_paise": 100},
		{"action_id": "rfa_002", "payment_id": "pay_A", "amount_paise": 200},
		{"action_id": "rfa_003", "payment_id": "pay_B", "amount_paise": 300},
	}
	md := mustLoad(t, m)

	if got := len(md.Find("pay_A")); got != 2 {
		t.Fatalf("Find(pay_A) returned %d actions, want 2", got)
	}
	if got := len(md.Find("pay_B")); got != 1 {
		t.Fatalf("Find(pay_B) returned %d actions, want 1", got)
	}
	if got := len(md.Find("pay_UNKNOWN")); got != 0 {
		t.Fatalf("Find on an unknown payment returned %d actions, want 0", got)
	}
	if got := len(md.Find("")); got != 0 {
		t.Fatalf("Find(\"\") returned %d actions, want 0", got)
	}
}
