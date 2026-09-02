package relay

import (
	"strings"
	"testing"
)

// THE BYPASS THIS GUARDS. Go's encoding/json takes the LAST duplicate key, so
// this line read as method="tools/list" -- a read -- and the relay forwarded it
// byte-for-byte, carrying an unauthorized 900,000-paise create_refund to a child
// whose parser may take the FIRST. Reproduced against the real relay before the
// check existed; the child received the whole thing.
func TestADuplicateMethodCannotSmuggleARefundPastTheClassifier(t *testing.T) {
	g := newGuard(t, `[{"action_id":"rfa_d1","payment_id":"pay_SYND1","amount_paise":24000}]`)
	r, child, agent := newRelay(t, g)

	feed(t, r, `{"jsonrpc":"2.0","id":1,`+
		`"method":"tools/call","method":"tools/list",`+
		`"params":{"name":"create_refund","arguments":`+
		`{"payment_id":"pay_SYND1","amount":900000}}}`)

	if got := child.String(); got != "" {
		t.Fatalf("bytes reached the child from an ambiguous message:\n  %s", got)
	}
	if !strings.Contains(agent.String(), "more than once") {
		t.Errorf("the agent was not told why it was refused:\n  %s", agent.String())
	}
}

// The detector has to find duplicates at ANY depth, or the same trick works one
// level down.
func TestDuplicatesAreFoundAtEveryDepth(t *testing.T) {
	for _, line := range []string{
		`{"a":1,"a":2}`,
		`{"params":{"name":"x","name":"y"}}`,
		`{"params":{"arguments":{"amount":1,"amount":2}}}`,
		`{"items":[{"k":1,"k":2}]}`,
		`{"items":[[{"deep":1,"deep":2}]]}`,
	} {
		if _, dup := duplicateKey([]byte(line)); !dup {
			t.Errorf("missed a duplicate in %s", line)
		}
	}
}

// FALSE POSITIVES WOULD BREAK REAL TRAFFIC, which is worse than the bug: MCP
// messages legitimately reuse a key NAME at different levels and across sibling
// objects. Only a repeat within ONE object is ambiguous.
func TestLegitimateRepeatedKeyNamesAreNotRefused(t *testing.T) {
	for _, line := range []string{
		// "name" at two levels -- the ordinary shape of every tools/call.
		`{"params":{"name":"create_refund","arguments":{"name":"x"}}}`,
		// The same keys in sibling objects inside an array.
		`{"tools":[{"name":"a","desc":"1"},{"name":"b","desc":"2"}]}`,
		// Repeats across siblings at depth.
		`{"a":{"k":1},"b":{"k":2}}`,
		// Arrays of scalars must not be mistaken for keys.
		`{"list":["k","k","k"],"other":1}`,
		// A string VALUE equal to a key already seen in the same object.
		`{"method":"method","id":1}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_SYN01","amount":24000}}}`,
	} {
		if k, dup := duplicateKey([]byte(line)); dup {
			t.Errorf("legitimate message refused for key %q:\n  %s", k, line)
		}
	}
}

// Malformed JSON is the caller's problem -- handleAgentLine reports the parse
// error. This must not claim a duplicate it never saw.
func TestMalformedInputReportsNoDuplicate(t *testing.T) {
	for _, line := range []string{`{`, `{"a":}`, `not json`, ``, `[1,2`} {
		if _, dup := duplicateKey([]byte(line)); dup {
			t.Errorf("claimed a duplicate in malformed input %q", line)
		}
	}
}
