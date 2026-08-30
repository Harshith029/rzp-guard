package policy

import (
	"strings"
	"testing"

	"github.com/harshith/rzp-guard/internal/lifecycle"
	"github.com/harshith/rzp-guard/internal/mandate"
)

// Combining exists because of a MEASURED false block. Arm B's brief A02
// authorized 18500 for the dal and 19000 for the oil; the agent issued one
// refund for 37500 and the guard refused it, in all three runs. The adjudicator
// ruled it in-intent: the two items the merchant asked to be refunded, issued
// as one call instead of two -- same money, same items, different granularity.
//
// The rule these tests pin: a set of the merchant's OWN actions whose exact
// amounts sum to the requested amount is authorized, because it IS the grant.
// Anything else is not.

const twoItems = `[
 {"action_id":"A02_01","payment_id":"pay_SYN0001","amount_paise":18500},
 {"action_id":"A02_02","payment_id":"pay_SYN0001","amount_paise":19000}]`

// THE CASE THIS WAS BUILT FOR.
func TestTwoAuthorizedItemsMayBeRefundedAsOneCall(t *testing.T) {
	g := New(mustMandate(t, twoItems))

	d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":37500}`), now)
	if !d.Allowed {
		t.Fatalf("refused the sum of two authorized items: %s: %s", d.Rule, d.Reason)
	}
	if len(d.MatchedActionIDs) != 2 {
		t.Fatalf("matched %v, want both actions", d.MatchedActionIDs)
	}
	if d.MatchedActionIDs[0] != "A02_01" || d.MatchedActionIDs[1] != "A02_02" {
		t.Fatalf("matched %v, want sorted [A02_01 A02_02]", d.MatchedActionIDs)
	}
	if got, _ := d.ForwardedAmountPaise(); got != 37500 {
		t.Fatalf("forwarded %d, want the canonical 37500", got)
	}
	if !strings.Contains(d.Reason, "combination") {
		t.Fatalf("reason does not say a combination was used: %q", d.Reason)
	}

	// BOTH are consumed. Combining must not leave either spendable again.
	for _, id := range []string{"A02_01", "A02_02"} {
		if st := g.State(id); st != lifecycle.Reserved {
			t.Fatalf("%s is %s after the combined call, want RESERVED", id, st)
		}
	}
	if enc := g.Encumbered(); enc != 37500 {
		t.Fatalf("encumbered %d, want 37500", enc)
	}
}

// The pre-existing granularity must keep working. This is the regression a
// naive "merge the items into one action" design would have caused.
func TestSeparateCallsForEachItemStillWork(t *testing.T) {
	g := New(mustMandate(t, twoItems))

	if d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":18500}`), now); !d.Allowed {
		t.Fatalf("first item refused: %s", d.Reason)
	}
	if d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":19000}`), now); !d.Allowed {
		t.Fatalf("second item refused: %s", d.Reason)
	}
	if enc := g.Encumbered(); enc != 37500 {
		t.Fatalf("encumbered %d, want 37500", enc)
	}
}

// Combining must not become a way to spend an action twice.
func TestACombinationCannotReuseAConsumedAction(t *testing.T) {
	g := New(mustMandate(t, twoItems))

	if d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":18500}`), now); !d.Allowed {
		t.Fatal(d.Reason)
	}
	// A02_01 is now RESERVED. 37500 needs it, so no combination is available.
	d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":37500}`), now)
	if d.Allowed {
		t.Fatal("combined a consumed action into a second refund; that action " +
			"would have been spent twice")
	}
}

// Only exact sums. A combination is the grant, not a licence to approximate it.
func TestAmountsThatAreNotAnExactSubsetSumAreRefused(t *testing.T) {
	for _, amount := range []string{"37499", "37501", "20000", "56000", "1"} {
		t.Run(amount, func(t *testing.T) {
			g := New(mustMandate(t, twoItems))
			d := g.Decide(RefundTool,
				jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":`+amount+`}`), now)
			if d.Allowed {
				t.Fatalf("%s is not any subset-sum of {18500,19000} but was allowed: %s",
					amount, d.Reason)
			}
			if d.Rule != AmountNotAuthorized {
				t.Fatalf("rule %s, want %s", d.Rule, AmountNotAuthorized)
			}
		})
	}
}

// A bounded action has no fixed amount, so "which part of the target did it
// cover" has no single answer. Combining one would consume more authority than
// the refund actually used.
func TestBoundedActionsAreNeverCombined(t *testing.T) {
	g := New(mustMandate(t, `[
	 {"action_id":"bnd_1","payment_id":"pay_SYN0001","max_amount_paise":20000},
	 {"action_id":"bnd_2","payment_id":"pay_SYN0001","max_amount_paise":20000}]`))

	// 40000 is the sum of the two ceilings. It must NOT be admitted.
	d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":40000}`), now)
	if d.Allowed {
		t.Fatalf("combined two bounded grants: %s", d.Reason)
	}

	// A single bounded action still behaves exactly as before.
	if d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":15000}`), now); !d.Allowed {
		t.Fatalf("single bounded action refused: %s", d.Reason)
	}
}

// The cap applies to the TOTAL, checked before anything is reserved. Checking
// actions one at a time would let a set through whose sum exceeds it.
func TestACombinationCannotExceedTheCumulativeCap(t *testing.T) {
	g := New(mustMandate(t, twoItems, cumulative(30000)))

	d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":37500}`), now)
	if d.Allowed {
		t.Fatalf("a combination of 37500 passed a 30000 cap: %s", d.Reason)
	}
	if g.Encumbered() != 0 {
		t.Fatalf("a refused combination encumbered %d paise", g.Encumbered())
	}
	for _, id := range []string{"A02_01", "A02_02"} {
		if st := g.State(id); st != lifecycle.Available {
			t.Fatalf("%s is %s after a refused combination, want AVAILABLE", id, st)
		}
	}
}

// A single action is preferred over a combination reaching the same total, so
// a combination is never used where one grant already covers it.
func TestASingleExactActionWinsOverACombination(t *testing.T) {
	g := New(mustMandate(t, `[
	 {"action_id":"whole","payment_id":"pay_SYN0001","amount_paise":30000},
	 {"action_id":"part_a","payment_id":"pay_SYN0001","amount_paise":10000},
	 {"action_id":"part_b","payment_id":"pay_SYN0001","amount_paise":20000}]`))

	d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":30000}`), now)
	if !d.Allowed {
		t.Fatal(d.Reason)
	}
	if len(d.MatchedActionIDs) != 1 || d.MatchedActionIDs[0] != "whole" {
		t.Fatalf("matched %v, want the single action that covers it exactly",
			d.MatchedActionIDs)
	}
}

// A single action's receipt must be BIT-IDENTICAL to what this project has
// always issued, or every receipt in the frozen study traces changes meaning.
func TestSingleActionReceiptsAreUnchanged(t *testing.T) {
	want, err := mandate.ReceiptFor("mnd_test", "A02_01")
	if err != nil {
		t.Fatal(err)
	}
	got, err := mandate.ReceiptForSet("mnd_test", []string{"A02_01"})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("one-element set yielded %q, want the plain receipt %q", got, want)
	}
}

func TestCombinationReceiptIsDeterministicAndDistinct(t *testing.T) {
	a, err := mandate.ReceiptForSet("mnd_test", []string{"A02_01", "A02_02"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := mandate.ReceiptForSet("mnd_test", []string{"A02_02", "A02_01"})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("receipt depends on match order: %q vs %q", a, b)
	}
	for _, id := range []string{"A02_01", "A02_02"} {
		single, _ := mandate.ReceiptFor("mnd_test", id)
		if a == single {
			t.Fatalf("the combination's receipt collides with %s's", id)
		}
	}
	if !strings.HasPrefix(a, "rzpg_") {
		t.Fatalf("combination receipt %q lost its prefix", a)
	}
}

// A duplicate in the set would be counted once against the cap and consumed
// once, while the caller believes it paid for two.
func TestADuplicatedActionInOneCallIsRefused(t *testing.T) {
	if _, err := mandate.ReceiptForSet("mnd_test", []string{"A02_01", "A02_01"}); err == nil {
		t.Fatal("a receipt was derived for a set containing the same action twice")
	}
}

// Three items: the sum of all three, and the sum of each pair.
func TestCombinationsOfMoreThanTwo(t *testing.T) {
	const three = `[
	 {"action_id":"itm_1","payment_id":"pay_SYN0001","amount_paise":1000},
	 {"action_id":"itm_2","payment_id":"pay_SYN0001","amount_paise":2000},
	 {"action_id":"itm_3","payment_id":"pay_SYN0001","amount_paise":4000}]`

	for _, tc := range []struct {
		amount string
		want   int
	}{
		{"7000", 3}, // all three
		{"3000", 2}, // itm_1+itm_2
		{"5000", 2}, // itm_1+itm_3
		{"6000", 2}, // itm_2+itm_3
		{"4000", 1}, // itm_3 alone -- a single action, not a combination
	} {
		t.Run(tc.amount, func(t *testing.T) {
			g := New(mustMandate(t, three))
			d := g.Decide(RefundTool,
				jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":`+tc.amount+`}`), now)
			if !d.Allowed {
				t.Fatalf("refused %s: %s", tc.amount, d.Reason)
			}
			if len(d.MatchedActionIDs) != tc.want {
				t.Fatalf("%s matched %v (%d actions), want %d",
					tc.amount, d.MatchedActionIDs, len(d.MatchedActionIDs), tc.want)
			}
		})
	}
}

// Actions on OTHER payments must never be pulled into a combination.
func TestCombinationNeverCrossesPayments(t *testing.T) {
	g := New(mustMandate(t, `[
	 {"action_id":"pay_1","payment_id":"pay_SYN0001","amount_paise":10000},
	 {"action_id":"pay_2","payment_id":"pay_SYN0002","amount_paise":10000}]`))

	d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":20000}`), now)
	if d.Allowed {
		t.Fatalf("combined an action from another payment: %v", d.MatchedActionIDs)
	}
}

// The availability filter is about COMPLETENESS, not just safety.
//
// The ledger refuses a consumed action regardless, so dropping the filter still
// fails closed -- which is why a mutation removing it went unnoticed until the
// sweep. The damage it does is subtler: the search may settle on a set that
// includes a consumed action and stop, denying a refund for which a DIFFERENT
// valid combination existed. Failing closed is correct; failing closed when the
// merchant did authorize the money is a false block, and false blocks are the
// entire reason this rule was written.
func TestACombinationIsFoundEvenWhenAnotherActionIsAlreadyConsumed(t *testing.T) {
	g := New(mustMandate(t, `[
	 {"action_id":"aaa_used","payment_id":"pay_SYN0001","amount_paise":5000},
	 {"action_id":"bbb_free","payment_id":"pay_SYN0001","amount_paise":5000},
	 {"action_id":"ccc_ten","payment_id":"pay_SYN0001","amount_paise":10000},
	 {"action_id":"ddd_ten","payment_id":"pay_SYN0001","amount_paise":10000}]`))

	// Consume aaa_used on its own.
	if d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":5000}`), now); !d.Allowed {
		t.Fatalf("setup: %s", d.Reason)
	}
	if d := g.State("aaa_used"); d != lifecycle.Reserved {
		t.Fatalf("setup: used_a is %s", d)
	}

	// 15000 = 5000 + 10000. The only 5000 left is bbb_free, so the search must
	// skip the consumed aaa_used and still find {bbb_free, ccc_ten or ddd_ten}.
	d := g.Decide(RefundTool, jsonArgs(t, `{"payment_id":"pay_SYN0001","amount":15000}`), now)
	if !d.Allowed {
		t.Fatalf("refused 15000 although bbb_free + a 10000 action were both "+
			"available; the search settled on the consumed action instead of "+
			"looking past it: %s: %s", d.Rule, d.Reason)
	}
	if len(d.MatchedActionIDs) != 2 {
		t.Fatalf("matched %v, want two actions", d.MatchedActionIDs)
	}
	for _, id := range d.MatchedActionIDs {
		if id == "aaa_used" {
			t.Fatalf("matched the already-consumed aaa_used: %v", d.MatchedActionIDs)
		}
	}
}
