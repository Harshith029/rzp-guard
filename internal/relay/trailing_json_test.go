package relay

import (
	"strings"
	"testing"
)

// One line, two JSON-RPC values: a permitted read, then an unauthorized refund.
//
// json.Decoder.Decode reads the FIRST value and reports nothing about what
// follows, so the relay classified on the read and the second value was silently
// discarded -- the agent asked for something that was neither forwarded nor
// refused.
//
// It was never a bypass: every forwarded message is re-encoded from the parsed
// value rather than echoed from the line, so create_refund did not ride along.
// This pins both halves -- nothing rides along, AND the line is now refused
// outright rather than half-honoured.
func TestTrailingJSONValueOnOneLine(t *testing.T) {
	g := newGuard(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`)
	r, child, agent := newRelay(t, g)

	line := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` +
		`{"name":"fetch_payment","arguments":{"payment_id":"pay_SYN0001"}}}` +
		` {"jsonrpc":"2.0","id":2,"method":"tools/call","params":` +
		`{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":99999}}}`

	_ = r.PumpAgent(strings.NewReader(line + "\n"))

	got, back := child.String(), agent.String()
	if strings.Contains(got, "create_refund") {
		t.Fatalf("a second JSON value rode along: the relay classified the first "+
			"value (a read) and the child received a create_refund it never "+
			"authorized.\n%s", got)
	}
	// Nothing at all may be forwarded: the line could not be fully inspected.
	if got != "" {
		t.Fatalf("forwarded part of a line carrying two values: %q", got)
	}
	if !strings.Contains(back, "more than one JSON value") {
		t.Fatalf("the agent was not told its line was refused; it got %q", back)
	}
}

// The ordinary case must be unaffected: one value, trailing newline, forwarded.
func TestSingleValueLineStillForwards(t *testing.T) {
	g := newGuard(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":20000}]`)
	r, child, _ := newRelay(t, g)
	feed(t, r, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+
		`{"name":"fetch_payment","arguments":{"payment_id":"pay_SYN0001"}}}`)
	if !strings.Contains(child.String(), "fetch_payment") {
		t.Fatalf("an ordinary single-value line was refused: %q", child.String())
	}
}
