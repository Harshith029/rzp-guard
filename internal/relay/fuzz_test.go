package relay

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/harshith/rzp-guard/internal/lifecycle"
)

// The relay parses attacker-influenced bytes on every single request. An agent
// under prompt injection emits whatever the injecting text asked for, and the
// arguments of a tools/call are entirely caller-controlled.
//
// Nothing had ever fuzzed it. These do not look for crashes alone -- a crash is
// the least interesting failure here, because the process dying is fail-closed.
// They assert the INVARIANT THAT MATTERS: whatever bytes arrive, a create_refund
// the mandate does not authorize must never reach the child.
//
// Run longer than the seed corpus with:
//   go test ./internal/relay/ -run Fuzz -fuzz FuzzAgentLine -fuzztime 60s

// The one guarantee, under arbitrary input.
func FuzzAgentLineNeverLeaksAnUnauthorizedRefund(f *testing.F) {
	seeds := []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":50000}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_EVIL","amount":1}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":1e9}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":50000.9}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":-50000}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":["pay_SYN0001"],"amount":50000}}}`,
		`{"jsonrpc":"2.0","id":null,"method":"tools/call","params":{"name":"create_refund","arguments":{}}}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":50000}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"CREATE_REFUND","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"initiate_payment","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`,
		`{}`,
		`[]`,
		`null`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":null}`,
		`{"jsonrpc":"2.0","id":{"a":1},"method":"tools/call","params":{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":50000}}}`,
		"\x00\x01\x02",
		strings.Repeat("{", 200),
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, line string) {
		// A newline would make PumpAgent see two frames; handleAgentLine takes
		// one, so keep the unit honest.
		if strings.ContainsAny(line, "\n\r") {
			t.Skip()
		}

		g := newGuard(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":50000}]`)
		r, child, _ := newRelay(t, g)

		// Must not panic. A panic here is a remote crash triggered by agent
		// input, which is a denial of service even though it fails closed.
		_ = r.handleAgentLine([]byte(line))

		sent := child.String()
		if sent == "" {
			return
		}
		// Detection must PARSE, not substring-match.
		//
		// The first version of this check asked whether "create_refund" appeared
		// anywhere in the forwarded bytes, and the fuzzer broke it in half a
		// second with `{"":"create_refund"}` -- an object carrying that text as a
		// VALUE, with no method at all. The relay forwards non-tools/call
		// messages byte-for-byte by design, and the child ignores a frame that
		// invokes nothing. A substring is not a tool call, and a test that
		// confuses the two reports a hole that does not exist.
		refunds := forwardedRefunds(t, sent)
		for _, args := range refunds {
			if args["payment_id"] != "pay_SYN0001" {
				t.Fatalf("a create_refund for an unauthorized payment reached the "+
					"child.\ninput: %q\nsent:  %s", line, sent)
			}
			// The forwarded amount must be the canonical authorized integer, not
			// whatever the caller wrote. json decodes it back as a float64 here;
			// the guard's own typing is asserted in policy's tests.
			if amt, ok := args["amount"].(float64); !ok || int64(amt) != 50000 {
				t.Fatalf("a create_refund reached the child with amount %v, want the "+
					"authorized 50000.\ninput: %q\nsent:  %s", args["amount"], line, sent)
			}
			rcpt, _ := args["receipt"].(string)
			if !strings.HasPrefix(rcpt, "rzpg_") {
				t.Fatalf("a create_refund reached the child with no injected "+
					"receipt.\ninput: %q\nsent:  %s", line, sent)
			}
		}
		// One authorization, one forward. A single input line must never produce
		// two money-moving calls.
		if len(refunds) > 1 {
			t.Fatalf("one input produced %d create_refund calls.\ninput: %q\nsent: %s",
				len(refunds), line, sent)
		}
		// Whatever happened, the ledger must never exceed what the mandate allows.
		if enc := g.Encumbered(); enc > 50000 {
			t.Fatalf("encumbered %d paise, mandate authorizes at most 50000.\ninput: %q",
				enc, line)
		}
	})
}

// Child replies are also untrusted: they come from a container talking to a
// remote API. A reply must never move an action to COMMITTED unless it carries
// a refund entity matching payment, amount AND the injected receipt.
func FuzzChildReplyNeverFalselyCommits(f *testing.F) {
	for _, s := range []string{
		`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"{}"}]}}`,
		`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"{\"entity\":\"refund\",\"id\":\"rfnd_x\",\"payment_id\":\"pay_SYN0001\",\"amount\":50000,\"receipt\":\"wrong\"}"}]}}`,
		`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"{\"entity\":\"refund\",\"id\":\"\",\"payment_id\":\"pay_SYN0001\",\"amount\":50000}"}]}}`,
		`{"jsonrpc":"2.0","id":7,"result":{"isError":true}}`,
		`{"jsonrpc":"2.0","id":7,"error":{"code":-1}}`,
		`{"jsonrpc":"2.0","id":7}`,
		`{"jsonrpc":"2.0","id":8,"result":{}}`,
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, reply string) {
		if strings.ContainsAny(reply, "\n\r") {
			t.Skip()
		}
		g := newGuard(t, `[{"action_id":"rfa_001","payment_id":"pay_SYN0001","amount_paise":50000}]`)
		r, _, _ := newRelay(t, g)

		if err := r.handleAgentLine([]byte(
			`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":` +
				`{"name":"create_refund","arguments":{"payment_id":"pay_SYN0001","amount":50000}}}`)); err != nil {
			t.Fatal(err)
		}
		want := inflightReceipt(t, r, "7")

		_ = r.PumpChild(strings.NewReader(reply + "\n"))

		if g.State("rfa_001") == lifecycle.Committed {
			// Committing is only legitimate when the reply really did carry the
			// matching entity. Anything else is a false commit: a single-use
			// action consumed on evidence that does not exist.
			if !strings.Contains(reply, want) ||
				!strings.Contains(reply, "pay_SYN0001") ||
				!strings.Contains(reply, "50000") {
				t.Fatalf("COMMITTED on a reply lacking the authorized "+
					"payment/amount/receipt (%s).\nreply: %q", want, reply)
			}
		}
	})
}

// forwardedRefunds returns the arguments of every genuine create_refund
// tools/call in the bytes written toward the child. It decodes rather than
// scanning, because the question is "did a refund invocation cross the
// boundary", and only a parse can answer that.
func forwardedRefunds(t *testing.T, sent string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, ln := range strings.Split(strings.TrimSpace(sent), "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var m struct {
			Method string `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			continue // not a JSON-RPC frame; the child will reject it
		}
		if m.Method == "tools/call" && m.Params.Name == "create_refund" {
			out = append(out, m.Params.Arguments)
		}
	}
	return out
}
