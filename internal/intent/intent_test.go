package intent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/harshith/rzp-guard/internal/mandate"
)

// baseIntent is a valid two-line intent. Every refusal test below mutates
// exactly one thing about it, so a failure names the rule that fired rather
// than leaving it ambiguous which of several defects was caught.
const baseIntent = `{
  "intent_id": "int_test_001",
  "issued_by": "support@merchant.example",
  "issued_at": "2026-09-04T10:00:00Z",
  "valid_for": "2h",
  "max_calls_per_minute": 6,
  "read_tools": ["fetch_payment"],
  "items": [
    {
      "item_id": "damaged_dal",
      "payment_id": "pay_SYN00000000001",
      "captured_paise": 100000,
      "refund": {"exact_paise": 18500},
      "because": "the dal jar arrived cracked"
    },
    {
      "item_id": "spilled_oil",
      "payment_id": "pay_SYN00000000001",
      "captured_paise": 100000,
      "refund": {"exact_paise": 19000},
      "because": "the oil leaked over the rest of the order"
    }
  ],
  "total_paise": 37500
}`

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return v
}

func compileString(t *testing.T, raw string) (*Result, error) {
	t.Helper()
	in, err := Load([]byte(raw))
	if err != nil {
		return nil, err
	}
	return Compile(in, at(t, "2026-09-04T10:05:00Z"))
}

func mustCompile(t *testing.T, raw string) *Result {
	t.Helper()
	res, err := compileString(t, raw)
	if err != nil {
		t.Fatalf("expected this intent to compile, got: %v", err)
	}
	return res
}

// replace swaps one substring, failing the test if it was not there. It keeps a
// mutation honest: a typo in the fixture would otherwise produce a test that
// passes for the wrong reason.
func replace(t *testing.T, s, old, new string) string {
	t.Helper()
	if !strings.Contains(s, old) {
		t.Fatalf("fixture does not contain %q; the mutation would be a no-op", old)
	}
	return strings.Replace(s, old, new, 1)
}

func TestTheHappyPathGrantsExactlyWhatWasAsked(t *testing.T) {
	res := mustCompile(t, baseIntent)

	if got := res.Mandate.MandateID; got != "mnd_int_test_001" {
		t.Errorf("mandate_id = %q", got)
	}
	if got := res.Mandate.ExpiresAt; !got.Equal(at(t, "2026-09-04T12:00:00Z")) {
		t.Errorf("expires_at = %s, want issued_at + valid_for", got)
	}
	// THE INVARIANT. The cap is the sum of the actions, not a padded figure.
	if got, want := res.Mandate.Limits.MaxCumulativePaise, int64(37500); got != want {
		t.Errorf("cumulative cap = %d, want exactly the %d the lines total", got, want)
	}
	if got := len(res.Mandate.AuthorizedRefundActions); got != 2 {
		t.Fatalf("actions = %d, want 2", got)
	}
	for _, a := range res.Mandate.AuthorizedRefundActions {
		if a.IsBounded() {
			t.Errorf("action %s compiled to a bound from an exact line", a.ActionID)
		}
	}
	if got, want := res.Provenance.TotalHeadroomPaise, int64(0); got != want {
		t.Errorf("headroom = %d over an intent that named every figure", got)
	}
	// allowed_tools carries the read tool asked for plus create_refund, and
	// nothing else -- the compiler adds the refund tool rather than trusting the
	// merchant to list it.
	if got := strings.Join(res.Mandate.AllowedTools, ","); got != "fetch_payment,create_refund" {
		t.Errorf("allowed_tools = %q", got)
	}
}

// The compiled mandate must be enforceable by the guard, not merely well-formed
// by this package's own opinion. Compile re-parses through mandate.Load for
// exactly this reason; the test pins it.
func TestTheCompiledMandateLoadsInTheGuard(t *testing.T) {
	res := mustCompile(t, baseIntent)
	m, err := mandate.Load(res.MandateJSON)
	if err != nil {
		t.Fatalf("the guard's own loader rejects the compiled mandate: %v", err)
	}
	if !m.PermitsTool("create_refund") {
		t.Error("compiled mandate does not permit create_refund")
	}
}

// Compilation is deterministic: expiry derives from issued_at rather than the
// wall clock, so the same intent always produces the same bytes. Verify and
// detached signatures both depend on this.
func TestCompilingTwiceProducesIdenticalBytes(t *testing.T) {
	a := mustCompile(t, baseIntent)
	b := mustCompile(t, baseIntent)
	if string(a.MandateJSON) != string(b.MandateJSON) {
		t.Error("two compiles of one intent produced different mandates; a detached " +
			"signature over one would not verify against the other")
	}
}

func TestEveryRefusalRule(t *testing.T) {
	cases := []struct {
		name string
		want string
		raw  string
	}{
		{
			name: "an item with no figure is not compiled into a guess",
			want: AmbiguousAmount,
			raw:  replace(t, baseIntent, `"refund": {"exact_paise": 18500}`, `"refund": {}`),
		},
		{
			name: "two figures on one line is two intents",
			want: AmbiguousAmount,
			raw: replace(t, baseIntent, `"refund": {"exact_paise": 18500}`,
				`"refund": {"exact_paise": 18500, "up_to_paise": 20000, "delegated": true, "delegated_because": "x"}`),
		},
		{
			name: "a percentage of an unknown total is not a figure",
			want: AmbiguousAmount,
			raw: replace(t,
				replace(t, baseIntent, `"captured_paise": 100000,
      "refund": {"exact_paise": 18500}`, `"refund": {"percent_of_captured": "10"}`),
				`"total_paise": 37500`, `"total_paise": 29000`),
		},
		{
			name: "a bound without an explicit acknowledgement is refused",
			want: UndeclaredBound,
			raw: replace(t, baseIntent, `"refund": {"exact_paise": 18500}`,
				`"refund": {"up_to_paise": 18500}`),
		},
		{
			name: "a bound acknowledged with no reason is refused",
			want: UndeclaredBound,
			raw: replace(t, baseIntent, `"refund": {"exact_paise": 18500}`,
				`"refund": {"up_to_paise": 18500, "delegated": true}`),
		},
		{
			name: "the merchant's own total must agree with the lines",
			want: TotalDisagrees,
			raw:  replace(t, baseIntent, `"total_paise": 37500`, `"total_paise": 37000`),
		},
		{
			// 0.55% of 100000 paise is 550.0 exactly, but of 1000 paise it is 5.5,
			// and there is no correct way to choose between 5 and 6 on a merchant's
			// behalf.
			name: "a percentage that does not land on a whole paise is refused",
			want: FractionalPercent,
			raw: `{
			  "intent_id": "int_frac", "issued_by": "a@b.c",
			  "issued_at": "2026-09-04T10:00:00Z", "valid_for": "1h",
			  "max_calls_per_minute": 5,
			  "items": [{
			    "item_id": "part_refund", "payment_id": "pay_SYN00000000003",
			    "captured_paise": 1000, "refund": {"percent_of_captured": "0.55"},
			    "because": "agreed part refund"
			  }]
			}`,
		},
		{
			name: "a refund larger than its payment is refused",
			want: ExceedsCaptured,
			raw: replace(t, baseIntent, `"refund": {"exact_paise": 18500}`,
				`"refund": {"exact_paise": 180000}`),
		},
		{
			name: "lines that individually fit but together exceed the payment are refused",
			want: ExceedsCaptured,
			raw: replace(t,
				replace(t, baseIntent, `"captured_paise": 100000,
      "refund": {"exact_paise": 18500}`, `"captured_paise": 30000,
      "refund": {"exact_paise": 18500}`),
				`"captured_paise": 100000,
      "refund": {"exact_paise": 19000}`, `"captured_paise": 30000,
      "refund": {"exact_paise": 19000}`),
		},
		{
			name: "one item id twice would silently drop one of two requests",
			want: DuplicateItem,
			raw:  replace(t, baseIntent, `"item_id": "spilled_oil"`, `"item_id": "damaged_dal"`),
		},
		{
			name: "a window beyond the ceiling is authority nobody revisits",
			want: UnboundedWindow,
			raw:  replace(t, baseIntent, `"valid_for": "2h"`, `"valid_for": "720h"`),
		},
		{
			name: "a window shorter than the floor is a mandate born useless",
			want: UnboundedWindow,
			raw:  replace(t, baseIntent, `"valid_for": "2h"`, `"valid_for": "10s"`),
		},
		{
			name: "a mandate that would already be expired is refused",
			want: UnboundedWindow,
			raw:  replace(t, baseIntent, `"issued_at": "2026-09-04T10:00:00Z"`, `"issued_at": "2020-01-01T00:00:00Z"`),
		},
		{
			name: "a write tool cannot be smuggled in through read_tools",
			want: ToolWidening,
			raw:  replace(t, baseIntent, `"read_tools": ["fetch_payment"]`, `"read_tools": ["create_payment_link"]`),
		},
		{
			name: "create_refund is added by the compiler, not requested",
			want: ToolWidening,
			raw:  replace(t, baseIntent, `"read_tools": ["fetch_payment"]`, `"read_tools": ["create_refund"]`),
		},
		{
			name: "an amount below the provider floor could never be forwarded",
			want: BelowFloor,
			raw: replace(t,
				replace(t, baseIntent, `"refund": {"exact_paise": 18500}`, `"refund": {"exact_paise": 50}`),
				`"total_paise": 37500`, `"total_paise": 19050`),
		},
		{
			name: "an intent with no items authorizes nothing and is a mistake",
			want: EmptyIntent,
			raw: `{"intent_id":"int_x","issued_by":"a@b.c","issued_at":"2026-09-04T10:00:00Z",
			       "valid_for":"1h","max_calls_per_minute":5,"items":[]}`,
		},
		{
			name: "a line with no stated reason cannot be reviewed",
			want: MalformedIntent,
			raw:  replace(t, baseIntent, `"because": "the dal jar arrived cracked"`, `"because": "  "`),
		},
		{
			name: "an unknown field is a typo, not a default",
			want: MalformedIntent,
			raw:  replace(t, baseIntent, `"max_calls_per_minute": 6`, `"max_calls_per_min": 6`),
		},
		{
			name: "a second JSON document in the file would be ignored",
			want: MalformedIntent,
			raw:  baseIntent + "\n{}",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileString(t, tc.raw)
			if err == nil {
				t.Fatalf("compiled; expected refusal %s", tc.want)
			}
			if got := RuleOf(err); got != tc.want {
				t.Fatalf("refused with %s, want %s\n  %v", got, tc.want, err)
			}
		})
	}
}

// Delegation is legal, and the record has to say so loudly enough that a
// reviewer cannot miss it. This is the one path that can grant more than a
// named figure, so it gets its own test rather than a row in the table.
func TestDelegationCompilesAndIsRecordedAsDiscretion(t *testing.T) {
	raw := replace(t,
		replace(t, baseIntent, `"refund": {"exact_paise": 19000}`,
			`"refund": {"up_to_paise": 19000, "delegated": true,
			            "delegated_because": "courier has not billed the return leg yet"}`),
		`"total_paise": 37500`, `"total_paise": 37500`)

	res := mustCompile(t, raw)
	var bounded int
	for _, a := range res.Mandate.AuthorizedRefundActions {
		if a.IsBounded() {
			bounded++
		}
	}
	if bounded != 1 {
		t.Fatalf("bounded actions = %d, want 1", bounded)
	}
	if got, want := res.Provenance.TotalHeadroomPaise, int64(19000); got != want {
		t.Errorf("headroom = %d, want %d: the record must name every paise of discretion", got, want)
	}
	if got, want := res.Provenance.TotalAskedPaise, int64(18500); got != want {
		t.Errorf("asked = %d, want %d", got, want)
	}
	var found bool
	for _, c := range res.Provenance.Coverage {
		if c.Class == "delegated" && c.DelegatedBecause == "" {
			t.Error("a delegated line reached the record without its reason")
		}
		if c.Class == "delegated" {
			found = true
		}
	}
	if !found {
		t.Error("no delegated line in the coverage record")
	}
}

// The percentage path exists so a merchant can say "half of it" without doing
// arithmetic. It must land on an exact action, never a bound.
func TestAWholePercentageCompilesToAnExactAction(t *testing.T) {
	raw := `{
	  "intent_id": "int_pct",
	  "issued_by": "a@b.c",
	  "issued_at": "2026-09-04T10:00:00Z",
	  "valid_for": "1h",
	  "max_calls_per_minute": 5,
	  "items": [{
	    "item_id": "half_back",
	    "payment_id": "pay_SYN00000000002",
	    "captured_paise": 50000,
	    "refund": {"percent_of_captured": "12.5"},
	    "because": "restocking fee agreed at 12.5%"
	  }]
	}`
	res := mustCompile(t, raw)
	a := res.Mandate.AuthorizedRefundActions[0]
	if a.IsBounded() {
		t.Fatal("a percentage compiled to a bound; it names an exact figure")
	}
	if got, want := *a.AmountPaise, int64(6250); got != want {
		t.Errorf("12.5%% of 50000 = %d, want %d", got, want)
	}
}

// assertCoverage must not trust the loop that built the mandate. Corrupting the
// output after the fact is the only way to test that from outside, so this pins
// the check itself rather than the path through Compile.
func TestTheCoverageAssertionCatchesAWidenedCap(t *testing.T) {
	res := mustCompile(t, baseIntent)
	m := res.Mandate
	m.Limits.MaxCumulativePaise += 100000 // the examples/mandate.json habit
	if err := assertCoverage(m, res.Provenance); err == nil {
		t.Fatal("a cap above the sum of the actions passed the coverage assertion")
	} else if RuleOf(err) != CoverageOver {
		t.Fatalf("refused with %s, want %s", RuleOf(err), CoverageOver)
	}
}

func TestTheCoverageAssertionCatchesAnUnaccountedAction(t *testing.T) {
	res := mustCompile(t, baseIntent)
	m := res.Mandate
	extra := int64(500)
	m.AuthorizedRefundActions = append(m.AuthorizedRefundActions, mandate.Action{
		ActionID: "smuggled", PaymentID: "pay_SYN00000000009", AmountPaise: &extra,
	})
	if err := assertCoverage(m, res.Provenance); err == nil {
		t.Fatal("an action no line asked for passed the coverage assertion")
	} else if RuleOf(err) != CoverageOver {
		t.Fatalf("refused with %s, want %s", RuleOf(err), CoverageOver)
	}
}

func TestTheCoverageAssertionCatchesABoundFromAnExactLine(t *testing.T) {
	res := mustCompile(t, baseIntent)
	m := res.Mandate
	v := *m.AuthorizedRefundActions[0].AmountPaise
	m.AuthorizedRefundActions[0].AmountPaise = nil
	m.AuthorizedRefundActions[0].MaxAmountPaise = &v
	if err := assertCoverage(m, res.Provenance); err == nil {
		t.Fatal("an exact line silently widened into a bound passed the assertion")
	} else if RuleOf(err) != CoverageOver {
		t.Fatalf("refused with %s, want %s", RuleOf(err), CoverageOver)
	}
}

// Verify is the after-the-fact half: it must notice a mandate edited by hand
// after it was compiled, which is the realistic way an over-broad grant appears
// on a machine that has this compiler installed.
func TestVerifyCatchesAHandEditedMandate(t *testing.T) {
	res := mustCompile(t, baseIntent)
	prov := res.Provenance
	prov.IntentSHA256 = Digest([]byte(baseIntent))
	provJSON, err := MarshalProvenance(prov)
	if err != nil {
		t.Fatal(err)
	}

	if err := Verify([]byte(baseIntent), res.MandateJSON, provJSON, at(t, "2026-09-04T10:05:00Z")); err != nil {
		t.Fatalf("an untouched triple failed verification: %v", err)
	}

	edited := strings.Replace(string(res.MandateJSON), "37500", "375000", 1)
	if err := Verify([]byte(baseIntent), []byte(edited), provJSON, at(t, "2026-09-04T10:05:00Z")); err == nil {
		t.Fatal("verification passed a mandate whose cap was edited after compilation")
	}
}

func TestVerifyCatchesAnIntentRewrittenToJustifyTheMandate(t *testing.T) {
	res := mustCompile(t, baseIntent)
	prov := res.Provenance
	prov.IntentSHA256 = Digest([]byte(baseIntent))
	provJSON, err := MarshalProvenance(prov)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := replace(t, baseIntent, `"because": "the dal jar arrived cracked"`,
		`"because": "customer was owed the whole order"`)
	if err := Verify([]byte(rewritten), res.MandateJSON, provJSON, at(t, "2026-09-04T10:05:00Z")); err == nil {
		t.Fatal("verification passed an intent rewritten after the fact")
	}
}

// The percentage path does its arithmetic in integers. A float64 would make
// this pass and 1e-9 of a paise would be someone's rounding error later.
func TestPercentagesNeverTouchFloatingPoint(t *testing.T) {
	num, den, err := ratio("0.1")
	if err != nil {
		t.Fatal(err)
	}
	if num != 1 || den != 10 {
		t.Fatalf("ratio(0.1) = %d/%d, want 1/10", num, den)
	}
	if _, _, err := ratio("1e2"); err == nil {
		t.Error("exponent notation accepted for a percentage")
	}
	if _, _, err := ratio("-5"); err == nil {
		t.Error("a negative percentage was accepted")
	}
}

// The coverage record is what a reviewer reads. It has to round-trip, or the
// explain command shows something other than what was asserted.
func TestProvenanceRoundTrips(t *testing.T) {
	res := mustCompile(t, baseIntent)
	b, err := MarshalProvenance(res.Provenance)
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalProvenance(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.TotalGrantedPaise != res.Provenance.TotalGrantedPaise ||
		len(back.Coverage) != len(res.Provenance.Coverage) {
		t.Error("coverage record did not round-trip")
	}
	var probe map[string]any
	if err := json.Unmarshal(b, &probe); err != nil {
		t.Fatalf("coverage record is not valid JSON: %v", err)
	}
}
