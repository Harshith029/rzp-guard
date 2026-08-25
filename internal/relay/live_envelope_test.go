package relay

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The refund result envelope in testdata/live_refund_result.json was captured
// from a REAL Razorpay Test Mode create_refund on 2026-08-25, through the
// shipped guard and the official pinned MCP container (gate G1.6). Payment
// pay_TTwUH29tzhB4ME, 100 paise, refund rfnd_TTwf8Hhbx0sjZQ.
//
// It exists because refundEntityMatches was written against Razorpay's
// DOCUMENTED refund entity before any live envelope had been seen. That made
// automatic COMMITTED a compatibility guess. This test converts the guess into
// a pinned regression: if the predicate ever stops accepting the exact bytes
// the provider really sent, the build fails.
func liveEnvelope(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("testdata/live_refund_result.json")
	if err != nil {
		t.Fatalf("reading captured live envelope: %v", err)
	}
	return json.RawMessage(b)
}

var livePending = pending{
	actionID:  "rfa_g16_001",
	isRefund:  true,
	paymentID: "pay_TTwUH29tzhB4ME",
	amount:    100,
	receipt:   "rzpg_6b8602afde6b",
}

func TestLiveRefundEnvelopeCommits(t *testing.T) {
	env := liveEnvelope(t)
	if isToolError(env) {
		t.Fatal("captured live success envelope was read as a tool error")
	}
	if !refundEntityMatches(env, livePending) {
		t.Fatal("refundEntityMatches REJECTED a real Razorpay success envelope; " +
			"a genuine refund would have been forced to IN_DOUBT")
	}
}

// The envelope the provider actually returned carries status "pending" -- it
// only became "processed" asynchronously, after the MCP reply had been sent.
// COMMITTED therefore means "the provider created the refund entity", NOT "the
// money has settled". A synchronous reply cannot prove settlement, so the
// predicate deliberately does not read status. This test pins that the captured
// envelope really was non-terminal at decision time, so the distinction in the
// docs stays honest.
func TestLiveEnvelopeWasNotSettledAtDecisionTime(t *testing.T) {
	var w struct {
		Content []struct{ Text string } `json:"content"`
	}
	if err := json.Unmarshal(liveEnvelope(t), &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(w.Content[0].Text, `"status":"pending"`) {
		t.Fatal("captured envelope no longer shows status pending; the claim that " +
			"COMMITTED is decided pre-settlement must be re-checked")
	}
}

// Each mutation is a way a reply could differ from the refund we authorized.
// Every one must fail the predicate, which routes the action to IN_DOUBT and a
// human, rather than committing on a reply that is not provably ours.
func TestLiveEnvelopeMutationsAreNotCommitted(t *testing.T) {
	raw := string(liveEnvelope(t))
	for _, tc := range []struct{ name, from, to string }{
		{"different receipt", `rzpg_6b8602afde6b`, `rzpg_000000000000`},
		{"different payment", `pay_TTwUH29tzhB4ME`, `pay_OTHER99999999`},
		{"inflated amount", `\"amount\":100`, `\"amount\":9900`},
		{"no provider id", `\"id\":\"rfnd_TTwf8Hhbx0sjZQ\"`, `\"id\":\"\"`},
		{"not a refund entity", `\"entity\":\"refund\"`, `\"entity\":\"payment\"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(raw, tc.from) {
				t.Fatalf("fixture no longer contains %q; mutation is vacuous", tc.from)
			}
			m := json.RawMessage(strings.Replace(raw, tc.from, tc.to, 1))
			if refundEntityMatches(m, livePending) {
				t.Fatalf("COMMITTED on a mutated envelope (%s)", tc.name)
			}
		})
	}
}
