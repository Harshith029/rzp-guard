package policy

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/lifecycle"
	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/opauth"
	"github.com/harshith/rzp-guard/internal/storage"
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
	if enc := g.Encumbered(); enc != 0 {
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

func TestReceiptIsCollisionResistantAcrossMandatesNotGuaranteedUnique(t *testing.T) {
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
	// 48-bit truncated hash: this shows collision RESISTANCE, not uniqueness.
	// Uniqueness is a database constraint, covered by
	// TestReceiptUniquenessIsEnforcedByTheDatabase.
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
	if err := g.Commit(first.MatchedActionID); err != nil {
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
	_ = g.Commit(first.MatchedActionID)
	if d := g.Decide(RefundTool, jsonArgs(t, args), now); d.Allowed || d.Rule != ActionConsumed {
		t.Fatalf("replay allowed=%v rule=%s", d.Allowed, d.Rule)
	}
}

// Two independent layers, and the test asserts which one fires.
func TestDefaultDenyForUnlistedTools(t *testing.T) {
	// Layer 1: outside the build surface entirely. Denied before the mandate is
	// even consulted, so a mandate listing them cannot help.
	g := New(mustMandate(t, `[]`))
	for _, tool := range []string{"create_instant_settlement", "initiate_payment", "not_a_real_tool"} {
		d := g.Decide(tool, jsonArgs(t, `{"amount":100000}`), now)
		if d.Allowed || d.Rule != ToolNotSupported {
			t.Fatalf("%s: allowed=%v rule=%s, want %s", tool, d.Allowed, d.Rule, ToolNotSupported)
		}
	}

	// Layer 2: inside the build surface but absent from this mandate.
	narrow := mustMandate(t, `[]`, func(doc map[string]any) {
		doc["allowed_tools"] = []string{"fetch_payment"}
	})
	ng := New(narrow)
	for _, tool := range []string{"create_refund", "fetch_all_refunds"} {
		d := ng.Decide(tool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":100}`), now)
		if d.Allowed || d.Rule != ToolNotAllowed {
			t.Fatalf("%s: allowed=%v rule=%s, want %s", tool, d.Allowed, d.Rule, ToolNotAllowed)
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
	if err := g.MarkInDoubt(d.MatchedActionID); err != nil {
		t.Fatal(err)
	}
	if st := g.State("rfa_001"); st != lifecycle.InDoubt {
		t.Fatalf("state = %s", st)
	}
	if enc := g.Encumbered(); enc != 60000 {
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
	if err := g.ReleaseConfirmedRejection(d.MatchedActionID); err != nil {
		t.Fatal(err)
	}
	if g.Encumbered() != 0 {
		t.Fatal("budget still encumbered after a confirmed rejection")
	}
	// Rate limiter already recorded the first attempt, so this is call 2 of 10.
	if again := g.Decide(RefundTool, jsonArgs(t, args), now); !again.Allowed {
		t.Fatalf("legitimate authorization was burned by a rejected request: %s", again.Reason)
	}
}

// ------------------------------------------------- build-level tool surface

// A mandate is a session grant; it can only narrow the build surface, never
// widen it. The previous revision forwarded ANY tool a mandate listed, verified:
// initiate_payment, create_instant_settlement and revoke_token all returned
// allowed=true.
func TestMandateCannotWidenTheToolSurface(t *testing.T) {
	doc := `{"mandate_id":"mnd_test","expires_at":"` +
		now.Add(time.Hour).Format(time.RFC3339) + `",
		"allowed_tools":["initiate_payment","create_instant_settlement","revoke_token",
		                 "create_payment_link","capture_payment"],
		"authorized_refund_actions":[],
		"global":{"max_cumulative_paise":1000000,"max_calls_per_minute":10}}`
	m, err := mandate.Load([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	g := New(m)
	for _, tool := range []string{
		"initiate_payment", "create_instant_settlement", "revoke_token",
		"create_payment_link", "capture_payment",
	} {
		d := g.Decide(tool, jsonArgs(t, `{"amount":900000}`), now)
		if d.Allowed {
			t.Fatalf("%s was forwarded despite being outside the build surface", tool)
		}
		if d.Rule != ToolNotSupported {
			t.Fatalf("%s: rule = %s, want %s", tool, d.Rule, ToolNotSupported)
		}
	}
}

func TestSupportedReadToolsStillPassWhenTheMandateGrantsThem(t *testing.T) {
	g := New(mustMandate(t, `[]`))
	for _, tool := range []string{"fetch_payment", "fetch_all_payments"} {
		if d := g.Decide(tool, jsonArgs(t, `{"count":1}`), now); !d.Allowed {
			t.Fatalf("%s denied: %s (%s)", tool, d.Reason, d.Rule)
		}
	}
}

// ------------------------------------------------- rate slot accounting

// A request denied by the cumulative cap never reaches the child, so it must not
// consume rate capacity. The previous revision called tryRecord() before
// Reserve(), burning a slot on every cap-denied call.
func TestCapDeniedRequestDoesNotConsumeRateCapacity(t *testing.T) {
	g := New(mustMandate(t, `[
		{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":90000},
		{"action_id":"rfa_002","payment_id":"pay_SYN0002","amount_paise":10000},
		{"action_id":"rfa_003","payment_id":"pay_SYN0003","amount_paise":10000}]`,
		cumulative(20000), perMinute(2)))

	if d := g.Decide(RefundTool, jsonArgs(t,
		`{"payment_id":"pay_SYN0001","amount":90000}`), now); d.Rule != CumulativeCapExceeded {
		t.Fatalf("setup: rule = %s, want %s", d.Rule, CumulativeCapExceeded)
	}
	for _, pay := range []string{"pay_SYN0002", "pay_SYN0003"} {
		d := g.Decide(RefundTool, jsonArgs(t,
			`{"payment_id":"`+pay+`","amount":10000}`), now)
		if !d.Allowed {
			t.Fatalf("%s denied (%s): a cap-denied request consumed a rate slot",
				pay, d.Rule)
		}
	}
}

// ------------------------------------------------- strict amount typing

// Without UseNumber, 1e3 decodes to float64(1000); the previous revision
// accepted it, bypassing the stated exponent rejection.
func TestFloat64AmountIsRejectedOutrightNotCoerced(t *testing.T) {
	g := New(mustMandate(t,
		`[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":1000}]`))
	var args map[string]any
	if err := json.Unmarshal([]byte(`{"payment_id":"pay_SYN0001","amount":1e3}`), &args); err != nil {
		t.Fatal(err)
	}
	if _, isFloat := args["amount"].(float64); !isFloat {
		t.Fatalf("fixture precondition failed: amount is %T", args["amount"])
	}
	d := g.Decide(RefundTool, args, now)
	if d.Allowed {
		t.Fatal("float64 amount was authorized; the UseNumber bypass is still open")
	}
	if d.Rule != MalformedArguments {
		t.Fatalf("rule = %s, want %s", d.Rule, MalformedArguments)
	}
	for _, v := range []any{float64(1000), float64(0), float64(-1)} {
		if _, err := parseAmountPaise(v); err == nil {
			t.Fatalf("parseAmountPaise(float64 %v) accepted", v)
		}
	}
}

// ------------------------------------------------- durability

func newDurable(t *testing.T, dbPath, actions string,
	opts ...func(map[string]any)) (*Guard, *storage.Store) {
	t.Helper()
	m := mustMandate(t, actions, opts...)
	st, err := storage.Open(dbPath, m.MandateID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecoverStartup(); err != nil {
		t.Fatal(err)
	}
	snap, err := st.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	g := NewWithStore(m, st)
	g.Restore(snap.States, snap.Amounts)
	return g, st
}

// The design's central safety property has to survive Ctrl-C to mean anything.
func TestReservationSurvivesRestartAndBecomesInDoubt(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")
	const actions = `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":60000}]`

	g1, st1 := newDurable(t, db, actions, cumulative(100000))
	d := g1.Decide(RefundTool, jsonArgs(t,
		`{"payment_id":"pay_SYN0001","amount":60000}`), now)
	if !d.Allowed {
		t.Fatalf("reserve denied: %s", d.Reason)
	}
	if g1.State("rfa_001") != lifecycle.Reserved {
		t.Fatalf("state = %s, want RESERVED", g1.State("rfa_001"))
	}
	// Crash mid-flight: no Commit, no Release, process dies.
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	g2, st2 := newDurable(t, db, actions, cumulative(100000))
	defer st2.Close()

	if got := g2.State("rfa_001"); got != lifecycle.InDoubt {
		t.Fatalf("after restart state = %s, want IN_DOUBT: a live reservation is "+
			"exactly the ambiguous case", got)
	}
	if enc := g2.Encumbered(); enc != 60000 {
		t.Fatalf("after restart encumbered = %d, want 60000: budget must stay held", enc)
	}
	if retry := g2.Decide(RefundTool, jsonArgs(t,
		`{"payment_id":"pay_SYN0001","amount":60000}`), now); retry.Allowed {
		t.Fatal("restart released a locked action, enabling replay")
	}
}

func TestCommittedStateSurvivesRestart(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")
	const actions = `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`

	g1, st1 := newDurable(t, db, actions)
	d := g1.Decide(RefundTool, jsonArgs(t,
		`{"payment_id":"pay_SYN0001","amount":20000}`), now)
	if err := g1.Commit(d.MatchedActionID); err != nil {
		t.Fatal(err)
	}
	st1.Close()

	g2, st2 := newDurable(t, db, actions)
	defer st2.Close()
	if g2.State("rfa_001") != lifecycle.Committed {
		t.Fatalf("state = %s, want COMMITTED", g2.State("rfa_001"))
	}
	if g2.Committed() != 20000 {
		t.Fatalf("committed = %d, want 20000", g2.Committed())
	}
	if replay := g2.Decide(RefundTool, jsonArgs(t,
		`{"payment_id":"pay_SYN0001","amount":20000}`), now); replay.Allowed {
		t.Fatal("a committed action replayed after restart")
	}
}

func TestReceiptUniquenessIsEnforcedByTheDatabase(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")
	st, err := storage.Open(db, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Reserve("rfa_001", "rzpg_deadbeef1234", 1000); err != nil {
		t.Fatal(err)
	}
	// The receipt is a TRUNCATED hash, so uniqueness is a database constraint,
	// not a property of the hash.
	if err := st.Reserve("rfa_002", "rzpg_deadbeef1234", 1000); err == nil {
		t.Fatal("two actions were allowed to share a provider-side correlation key")
	}
}

// ------------------------------------------------- operator boundary

// Resolution requires an unforgeable opauth.Grant, and the audit record lands
// in the same transaction.
//
// lifecycle no longer performs any credential comparison. It used to take a
// token at construction and compare a caller-supplied token against it -- both
// sides came from the caller, so the check was vacuous. Authentication now
// happens once, in opauth, and the resolver demands its result.
func TestResolutionRequiresAGrantAndIsDurablyAudited(t *testing.T) {
	db := filepath.Join(t.TempDir(), "guard.db")
	m := mustMandate(t,
		`[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`)
	st, err := storage.Open(db, m.MandateID)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	led := lifecycle.NewLedger(m.Limits.MaxCumulativePaise, st)
	g := NewWithLedger(m, led)

	d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":20000}`), now)
	if err := g.MarkInDoubt(d.MatchedActionID); err != nil {
		t.Fatal(err)
	}

	// A zero-value Grant cannot be minted outside opauth and must be refused.
	if err := lifecycle.ResolveInDoubt(opauth.Grant{}, led, st, "rfa_001", true,
		"forged"); err == nil {
		t.Fatal("an unauthenticated zero-value Grant resolved the action")
	}
	if g.State("rfa_001") != lifecycle.InDoubt {
		t.Fatal("a refused resolution changed state")
	}
	if n, _ := st.AuditCount(); n != 0 {
		t.Fatalf("refused resolution wrote %d audit rows", n)
	}

	token, err := opauth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := opauth.Verifier(token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := opauth.Authenticate("ops@merchant", "wrong-token", verifier); err == nil {
		t.Fatal("a wrong token produced a Grant")
	}
	grant, err := opauth.Authenticate("ops@merchant", token, verifier)
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.ResolveInDoubt(grant, led, st, "rfa_001", true,
		"found receipt in Razorpay Test Mode"); err != nil {
		t.Fatal(err)
	}
	if g.State("rfa_001") != lifecycle.Committed {
		t.Fatalf("state = %s, want COMMITTED", g.State("rfa_001"))
	}
	if n, err := st.AuditCount(); err != nil || n != 1 {
		t.Fatalf("audit rows = %d (err=%v), want exactly 1", n, err)
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
		if enc := g.Encumbered(); enc != 55000 {
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
		if enc := g.Encumbered(); enc > 90000 {
			t.Fatalf("trial %d: encumbered %d exceeds cap 90000", trial, enc)
		}
	}
}

var _ = payB
