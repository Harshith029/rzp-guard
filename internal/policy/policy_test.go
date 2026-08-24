package policy

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/lifecycle"
	"github.com/harshith/rzp-guard/internal/mandate"
)

var now = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

const payA, payB = "pay_SYN0001", "pay_SYN0002"

// mustMandate builds a mandate through the real JSON loader, so tests exercise
// validation rather than bypassing it.
func mustMandate(t *testing.T, actions string, opts ...func(map[string]any)) *mandate.Mandate {
	t.Helper()
	doc := map[string]any{
		"mandate_id":    "mnd_test",
		"expires_at":    now.Add(4 * time.Hour).Format(time.RFC3339),
		"allowed_tools": []string{"fetch_payment", "fetch_all_payments", RefundTool},
		"global": map[string]any{
			"max_cumulative_paise": 500000, "max_calls_per_minute": 10,
		},
	}
	var acts []any
	if err := json.Unmarshal([]byte(actions), &acts); err != nil {
		t.Fatalf("bad test fixture: %v", err)
	}
	doc["authorized_refund_actions"] = acts
	for _, o := range opts {
		o(doc)
	}
	raw, _ := json.Marshal(doc)
	m, err := mandate.Load(raw)
	if err != nil {
		t.Fatalf("mandate.Load: %v", err)
	}
	return m
}

func expires(at time.Time) func(map[string]any) {
	return func(d map[string]any) { d["expires_at"] = at.Format(time.RFC3339) }
}
func cumulative(v int64) func(map[string]any) {
	return func(d map[string]any) { d["global"].(map[string]any)["max_cumulative_paise"] = v }
}
func perMinute(v int) func(map[string]any) {
	return func(d map[string]any) { d["global"].(map[string]any)["max_calls_per_minute"] = v }
}

// jsonArgs decodes with UseNumber, exactly as the relay will when reading a
// tools/call off the wire. Tests must not hand-build Go types the wire cannot
// produce, or they would not exercise the real typing path.
func jsonArgs(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("bad args fixture %q: %v", s, err)
	}
	return m
}

// ---------------------------------------------------------------- F1.a-c

// The prototype authorized int(50000.9)==50000, reserved 50000, and forwarded
// 50000.9 (FAILURES.md F1.a). The runtime schema declares amount as
// {"type":"number"}, so the child would NOT have caught it.
func TestFractionalAmountIsRejected(t *testing.T) {
	g := New(mustMandate(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":50000}]`))
	d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":50000.9}`), now)
	if d.Allowed {
		t.Fatalf("fractional amount was authorized: %+v", d)
	}
	if d.Rule != MalformedArguments {
		t.Fatalf("rule = %s, want %s", d.Rule, MalformedArguments)
	}
	if enc := g.Ledger().Encumbered(); enc != 0 {
		t.Fatalf("rejected call reserved %d paise", enc)
	}
}

func TestForwardedAmountEqualsAuthorizedAmountExactly(t *testing.T) {
	g := New(mustMandate(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":50000}]`))
	d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":50000}`), now)
	if !d.Allowed {
		t.Fatalf("expected allow: %s", d.Reason)
	}
	fwd, ok := d.ForwardedAmountPaise()
	if !ok {
		t.Fatal("forwarded arguments carry no integer amount")
	}
	if fwd != d.AuthorizedPaise {
		t.Fatalf("forwarded %d != authorized %d", fwd, d.AuthorizedPaise)
	}
	if fwd != 50000 {
		t.Fatalf("forwarded %d, want 50000", fwd)
	}
}

func TestBooleanAndNonFiniteAmountsAreRejectedNotCoerced(t *testing.T) {
	g := New(mustMandate(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":100}]`))
	for _, tc := range []struct{ name, args string }{
		{"boolean true", `{"payment_id":"pay_SYN0001","amount":true}`},
		{"string", `{"payment_id":"pay_SYN0001","amount":"50000"}`},
		{"exponent", `{"payment_id":"pay_SYN0001","amount":1e3}`},
		{"missing", `{"payment_id":"pay_SYN0001"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := g.Decide(RefundTool, jsonArgs(t, tc.args), now)
			if d.Allowed || d.Rule != MalformedArguments {
				t.Fatalf("got allowed=%v rule=%s, want malformed", d.Allowed, d.Rule)
			}
		})
	}
}

// NaN and Inf cannot appear in valid JSON, but a caller decoding without
// UseNumber can hand us float64 values. The prototype panicked on these.
func TestNonFiniteFloatsDoNotPanic(t *testing.T) {
	for _, v := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 50000.9} {
		if _, err := parseAmountPaise(v); err == nil {
			t.Fatalf("parseAmountPaise(%v) returned no error", v)
		}
	}
}

// ---------------------------------------------------------------- F1.d/e

func TestGeneratedReceiptMeetsRazorpayFormatForHostileActionIDs(t *testing.T) {
	// The prototype produced "rzpg_a" (6 chars) against a documented 10 floor.
	for _, id := range []string{"a", "x1", "rfa_001", strings.Repeat("z", 64)} {
		r, err := mandate.ReceiptFor("mnd_test", id)
		if err != nil {
			// Short ids are rejected upstream by the action_id pattern; if
			// ReceiptFor is reached it must still produce a valid receipt.
			t.Fatalf("ReceiptFor(%q): %v", id, err)
		}
		if len(r) < 10 {
			t.Fatalf("receipt %q for action %q is %d chars, below 10", r, id, len(r))
		}
		for _, c := range r {
			ok := c == '_' || c == '-' ||
				(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
			if !ok {
				t.Fatalf("receipt %q contains %q, outside [A-Za-z0-9_-]", r, c)
			}
		}
	}
}

func TestActionIDWithSpacesOrPunctuationIsRejectedAtLoad(t *testing.T) {
	for _, bad := range []string{"rfa 001", "rfa/001", "a", "rfa.001", ""} {
		doc := fmt.Sprintf(`{"mandate_id":"mnd_test","expires_at":%q,
			"allowed_tools":["create_refund"],
			"authorized_refund_actions":[{"action_id":%q,"payment_id":"pay_SYN0001","amount_paise":100}],
			"global":{"max_cumulative_paise":1000,"max_calls_per_minute":1}}`,
			now.Add(time.Hour).Format(time.RFC3339), bad)
		if _, err := mandate.Load([]byte(doc)); err == nil {
			t.Fatalf("action_id %q was accepted", bad)
		}
	}
}

func TestReceiptIsUniqueAcrossMandatesForTheSameActionID(t *testing.T) {
	a, err := mandate.ReceiptFor("mnd_alpha", "rfa_001")
	if err != nil {
		t.Fatal(err)
	}
	b, err := mandate.ReceiptFor("mnd_beta", "rfa_001")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("receipts collide across mandates: %q", a)
	}
	// Deterministic: idempotency depends on it.
	again, _ := mandate.ReceiptFor("mnd_alpha", "rfa_001")
	if again != a {
		t.Fatalf("receipt not deterministic: %q vs %q", a, again)
	}
}

// ---------------------------------------------------------------- core policy

func TestTwoEqualPartialRefundsBothPass(t *testing.T) {
	g := New(mustMandate(t, `[
		{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":50000},
		{"action_id":"rfa_002","payment_id":"pay_SYN0001","amount_paise":50000}]`))
	args := `{"payment_id":"pay_SYN0001","amount":50000}`

	first := g.Decide(RefundTool, jsonArgs(t, args), now)
	if !first.Allowed {
		t.Fatalf("first refund denied: %s", first.Reason)
	}
	if err := g.Ledger().Commit(first.MatchedActionID); err != nil {
		t.Fatal(err)
	}
	second := g.Decide(RefundTool, jsonArgs(t, args), now)
	if !second.Allowed {
		t.Fatalf("second legitimate partial refund denied: %s", second.Reason)
	}
	if first.MatchedActionID == second.MatchedActionID {
		t.Fatal("both refunds consumed the same action")
	}
	if first.Receipt == second.Receipt {
		t.Fatal("identical receipts: Razorpay would reject the second as a duplicate")
	}
}

func TestReplayOfConsumedActionIsDenied(t *testing.T) {
	g := New(mustMandate(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":50000}]`))
	args := `{"payment_id":"pay_SYN0001","amount":50000}`
	first := g.Decide(RefundTool, jsonArgs(t, args), now)
	_ = g.Ledger().Commit(first.MatchedActionID)
	if d := g.Decide(RefundTool, jsonArgs(t, args), now); d.Allowed || d.Rule != ActionConsumed {
		t.Fatalf("replay allowed=%v rule=%s", d.Allowed, d.Rule)
	}
}

func TestDefaultDenyForUnlistedTools(t *testing.T) {
	g := New(mustMandate(t, `[]`))
	for _, tool := range []string{"create_instant_settlement", "initiate_payment", "not_a_real_tool"} {
		if d := g.Decide(tool, jsonArgs(t, `{"amount":100000}`), now); d.Allowed || d.Rule != ToolNotAllowed {
			t.Fatalf("%s: allowed=%v rule=%s", tool, d.Allowed, d.Rule)
		}
	}
}

func TestExpiredMandateDeniesOtherwiseValidRefund(t *testing.T) {
	g := New(mustMandate(t,
		`[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":50000}]`,
		expires(now.Add(-time.Minute))))
	if d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":50000}`), now); d.Allowed {
		t.Fatal("expired mandate authorized a refund")
	}
}

func TestExactRejectsLowerAmountBoundedAccepts(t *testing.T) {
	exact := New(mustMandate(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":50000}]`))
	if d := exact.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":10000}`), now); d.Allowed {
		t.Fatal("exact action admitted a lower amount")
	}
	bounded := New(mustMandate(t, `[{"action_id":"rfa_003","payment_id":"pay_SYN0001","max_amount_paise":120000}]`))
	if d := bounded.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":45000}`), now); !d.Allowed {
		t.Fatalf("bounded action rejected an amount inside its bound: %s", d.Reason)
	}
	if d := bounded.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":120001}`), now); d.Allowed {
		t.Fatal("bounded action admitted an amount above its ceiling")
	}
}

func TestAmountIsBoundToItsPaymentNotToASetOfValidAmounts(t *testing.T) {
	g := New(mustMandate(t, `[
		{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":30000},
		{"action_id":"rfa_002","payment_id":"pay_SYN0002","amount_paise":90000}]`))
	d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":90000}`), now)
	if d.Allowed || d.Rule != AmountNotAuthorized {
		t.Fatalf("allowed=%v rule=%s: 90000 is authorized only on pay_SYN0002", d.Allowed, d.Rule)
	}
}

func TestAgentChosenSpeedAndNotesNeverDeny(t *testing.T) {
	g := New(mustMandate(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":15000}]`))
	d := g.Decide(RefundTool, jsonArgs(t,
		`{"payment_id":"pay_SYN0001","amount":15000,"speed":"normal","notes":{"ref":"t-1"}}`), now)
	if !d.Allowed {
		t.Fatalf("agent-chosen non-authorizing fields caused a denial: %s", d.Reason)
	}
}

func TestProxyOverwritesAgentSuppliedReceipt(t *testing.T) {
	g := New(mustMandate(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":15000}]`))
	d := g.Decide(RefundTool, jsonArgs(t,
		`{"payment_id":"pay_SYN0001","amount":15000,"receipt":"attacker-chosen"}`), now)
	if !d.Allowed {
		t.Fatalf("denied: %s", d.Reason)
	}
	if got := d.Forwarded["receipt"]; got == "attacker-chosen" {
		t.Fatal("agent-supplied receipt survived to the child")
	}
	want, _ := mandate.ReceiptFor("mnd_test", "rfa_001")
	if d.Forwarded["receipt"] != want {
		t.Fatalf("receipt = %v, want %v", d.Forwarded["receipt"], want)
	}
}

func TestRateLimitAloneDecidesWhenEveryActionIsAuthorized(t *testing.T) {
	var acts []string
	for k := 0; k < 14; k++ {
		acts = append(acts, fmt.Sprintf(
			`{"action_id":"rfa_%03d","payment_id":"pay_SYN%d","amount_paise":20000}`, k, k))
	}
	g := New(mustMandate(t, "["+strings.Join(acts, ",")+"]",
		cumulative(1000000), perMinute(10)))

	for k := 0; k < 14; k++ {
		d := g.Decide(RefundTool, jsonArgs(t, fmt.Sprintf(
			`{"payment_id":"pay_SYN%d","amount":20000}`, k)), now)
		if k < 10 && !d.Allowed {
			t.Fatalf("call %d denied: %s (%s)", k, d.Reason, d.Rule)
		}
		if k >= 10 {
			if d.Allowed {
				t.Fatalf("call %d allowed past the rate limit", k)
			}
			if d.Rule != RateLimitExceeded {
				t.Fatalf("call %d rule = %s, want %s: all prior controls pass, so only "+
					"the rate limit can be the reason", k, d.Rule, RateLimitExceeded)
			}
		}
	}
}

func TestBudgetCountsReservedNotOnlyCommitted(t *testing.T) {
	g := New(mustMandate(t, `[
		{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":60000},
		{"action_id":"rfa_002","payment_id":"pay_SYN0002","amount_paise":60000}]`,
		cumulative(100000)))
	if d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":60000}`), now); !d.Allowed {
		t.Fatalf("first denied: %s", d.Reason)
	}
	d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0002","amount":60000}`), now)
	if d.Allowed || d.Rule != CumulativeCapExceeded {
		t.Fatalf("allowed=%v rule=%s: reserved budget must count against the cap", d.Allowed, d.Rule)
	}
}

// ---------------------------------------------------------------- lifecycle

func TestInDoubtHoldsBudgetAndActionLocked(t *testing.T) {
	g := New(mustMandate(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":60000}]`,
		cumulative(100000)))
	args := `{"payment_id":"pay_SYN0001","amount":60000}`
	d := g.Decide(RefundTool, jsonArgs(t, args), now)
	if err := g.Ledger().MarkInDoubt(d.MatchedActionID); err != nil {
		t.Fatal(err)
	}
	if st := g.Ledger().State("rfa_001"); st != lifecycle.InDoubt {
		t.Fatalf("state = %s", st)
	}
	if enc := g.Ledger().Encumbered(); enc != 60000 {
		t.Fatalf("encumbered = %d, want 60000: budget must NOT be handed back", enc)
	}
	if retry := g.Decide(RefundTool, jsonArgs(t, args), now); retry.Allowed {
		t.Fatal("retry allowed while the original is IN_DOUBT")
	}
}

func TestConfirmedRejectionReturnsTheActionWithoutBurningIt(t *testing.T) {
	g := New(mustMandate(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`))
	args := `{"payment_id":"pay_SYN0001","amount":20000}`
	d := g.Decide(RefundTool, jsonArgs(t, args), now)
	if err := g.Ledger().ReleaseConfirmedRejection(d.MatchedActionID); err != nil {
		t.Fatal(err)
	}
	if g.Ledger().Encumbered() != 0 {
		t.Fatal("budget still encumbered after a confirmed rejection")
	}
	// Rate limiter already recorded the first attempt, so this is call 2 of 10.
	if again := g.Decide(RefundTool, jsonArgs(t, args), now); !again.Allowed {
		t.Fatalf("legitimate authorization was burned by a rejected request: %s", again.Reason)
	}
}

// F1.f: operator resolution must be a boundary, not a comment. The prototype's
// resolve_in_doubt was publicly callable with no auth and no audit trail.
func TestOperatorResolutionRequiresTokenAndWritesAudit(t *testing.T) {
	g := New(mustMandate(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`))
	d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":20000}`), now)
	_ = g.Ledger().MarkInDoubt(d.MatchedActionID)

	var audits []lifecycle.AuditRecord
	if _, err := lifecycle.NewConsole(g.Ledger(), "short", func(lifecycle.AuditRecord) {}); err == nil {
		t.Fatal("a weak operator token was accepted")
	}
	if _, err := lifecycle.NewConsole(g.Ledger(), strings.Repeat("k", 32), nil); err == nil {
		t.Fatal("a console with no audit sink was accepted")
	}
	console, err := lifecycle.NewConsole(g.Ledger(), strings.Repeat("k", 32),
		func(r lifecycle.AuditRecord) { audits = append(audits, r) })
	if err != nil {
		t.Fatal(err)
	}
	if err := console.Resolve("wrong-token", "ops@merchant", "rfa_001", true, "checked"); err == nil {
		t.Fatal("resolution succeeded with the wrong token")
	}
	if g.Ledger().State("rfa_001") != lifecycle.InDoubt {
		t.Fatal("a rejected resolution changed state")
	}
	if err := console.Resolve(strings.Repeat("k", 32), "ops@merchant", "rfa_001", true,
		"found receipt in Razorpay dashboard"); err != nil {
		t.Fatal(err)
	}
	if g.Ledger().State("rfa_001") != lifecycle.Committed {
		t.Fatalf("state = %s, want COMMITTED", g.Ledger().State("rfa_001"))
	}
	if len(audits) != 1 || audits[0].Operator != "ops@merchant" || !audits[0].RefundLanded {
		t.Fatalf("audit record missing or wrong: %+v", audits)
	}
}

// ---------------------------------------------------------------- concurrency

// A REAL concurrency test, unlike the prototype's two sequential calls
// (FAILURES.md F3). Run under `go test -race`.
func TestConcurrentDuplicatesExactlyOneReserves(t *testing.T) {
	for trial := 0; trial < 50; trial++ {
		g := New(mustMandate(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":55000}]`,
			perMinute(1000)))
		const goroutines = 8
		var wg sync.WaitGroup
		results := make([]Decision, goroutines)
		start := make(chan struct{})
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				args := map[string]any{"payment_id": payA, "amount": json.Number("55000")}
				<-start
				results[idx] = g.Decide(RefundTool, args, now)
			}(i)
		}
		close(start)
		wg.Wait()

		allowed := 0
		for _, d := range results {
			if d.Allowed {
				allowed++
			}
		}
		if allowed != 1 {
			t.Fatalf("trial %d: %d of %d concurrent duplicates were authorized, want exactly 1",
				trial, allowed, goroutines)
		}
		if enc := g.Ledger().Encumbered(); enc != 55000 {
			t.Fatalf("trial %d: encumbered = %d, want 55000", trial, enc)
		}
	}
}

func TestConcurrentDistinctRefundsRespectCumulativeCap(t *testing.T) {
	for trial := 0; trial < 20; trial++ {
		var acts []string
		for k := 0; k < 10; k++ {
			acts = append(acts, fmt.Sprintf(
				`{"action_id":"rfa_%03d","payment_id":"pay_SYN%d","amount_paise":30000}`, k, k))
		}
		// Cap admits exactly 3 refunds of 30000.
		g := New(mustMandate(t, "["+strings.Join(acts, ",")+"]",
			cumulative(90000), perMinute(1000)))

		var wg sync.WaitGroup
		results := make([]Decision, 10)
		start := make(chan struct{})
		for k := 0; k < 10; k++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				args := map[string]any{
					"payment_id": fmt.Sprintf("pay_SYN%d", idx),
					"amount":     json.Number("30000"),
				}
				<-start
				results[idx] = g.Decide(RefundTool, args, now)
			}(k)
		}
		close(start)
		wg.Wait()

		allowed := 0
		for _, d := range results {
			if d.Allowed {
				allowed++
			}
		}
		if allowed != 3 {
			t.Fatalf("trial %d: %d refunds authorized against a 3-refund cap", trial, allowed)
		}
		if enc := g.Ledger().Encumbered(); enc > 90000 {
			t.Fatalf("trial %d: encumbered %d exceeds cap 90000", trial, enc)
		}
	}
}

var _ = payB
