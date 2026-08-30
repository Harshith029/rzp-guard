// Command mcp-stub is a synthetic MCP child for the Phase 4b trace study.
//
// It stands in for Razorpay's official container so 45 traces do not require
// dozens of real captured payments and do not move Test Mode money on every
// run. What Phase 4b measures is the GUARD'S AUTHORIZATION DECISION on calls
// the agent emitted, and that decision does not depend on the provider actually
// executing anything.
//
// The shipped binary running against the REAL pinned container is proven
// separately and end to end by gate live-block and by the captured G1.6
// allow-path evidence, which includes a real refund at Razorpay. The two claims
// are deliberately kept apart, and this stub is never part of the second one.
//
// FIDELITY, and its limits:
//
//   - Tool schemas are the REAL ones, captured from the pinned container and
//     embedded verbatim (see study/stub_fixtures.py). The agent under test sees
//     byte-identical schemas to production -- including create_refund declaring
//     amount as {"type":"number"}, which is what makes fractional amounts
//     expressible at all.
//   - Payment records are derived by regex from the frozen briefs, so a
//     fetch_payment can never contradict the order the agent was shown.
//   - pay_SYN8099, referenced only by C02's injected text, deliberately has no
//     record and returns a not-found error.
//
// THE STUB NEVER REJECTS A REFUND. It has no policy of its own: any
// create_refund that reaches it succeeds. That is on purpose -- it means every
// block observed in a trace is attributable to the guard and to nothing else. A
// stub that enforced its own limits would silently become a second detector and
// confound the measurement.
package main

import (
	"bufio"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

//go:embed fixtures.json
var fixturesJSON []byte

type fixtures struct {
	Tools    []json.RawMessage          `json:"tools"`
	Payments map[string]json.RawMessage `json:"payments"`
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  struct {
		Name      string `json:"name"`
		Arguments struct {
			PaymentID string      `json:"payment_id"`
			Amount    json.Number `json:"amount"`
			Receipt   string      `json:"receipt"`
		} `json:"arguments"`
	} `json:"params"`
}

var out = bufio.NewWriter(os.Stdout)

func send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	out.Write(b)
	out.WriteByte('\n')
	out.Flush()
}

func result(id json.RawMessage, payload any) {
	send(map[string]any{"jsonrpc": "2.0", "id": id, "result": payload})
}

// toolText wraps a value the way an MCP tool result carries it: a content array
// whose single text element is the JSON entity.
func toolText(id json.RawMessage, v any) {
	b, _ := json.Marshal(v)
	result(id, map[string]any{
		"content": []any{map[string]string{"type": "text", "text": string(b)}},
	})
}

// intentionalUnknown are synthetic ids the study references DELIBERATELY
// without a record. pay_SYN8099 appears only inside C02's injected text: the
// agent is told about a payment that does not exist, and fetch_payment must say
// so. It is a fixture in the sense that matters -- a known, intended value --
// even though it has no payment body.
var intentionalUnknown = map[string]struct{}{
	"pay_SYN8099": {},
	// The recovery gate and examples/mandate.json use these against the stub.
	"pay_SYN00000000001": {},
	"pay_SYN99999999999": {},
}

// knownSyntheticID reports whether an id is one this stub is willing to
// impersonate a provider for.
//
// EXACT MEMBERSHIP, not a prefix. `pay_SYN` was a naming convention: it made
// pay_SYNanything acceptable, so a reproducer could invent an id the study
// never contained and get a plausible-looking refund back. Review called that a
// convention rather than a boundary and was right.
//
// This is identifier provenance, not authorization. The stub still has no
// policy -- every refund for a known id succeeds, exactly as before, so the
// stub cannot become a second detector and confound a measurement.
func knownSyntheticID(fx *fixtures, id string) bool {
	if _, ok := fx.Payments[id]; ok {
		return true
	}
	_, ok := intentionalUnknown[id]
	return ok
}

// refuseUnknownID is a real MCP tool error, not a success carrying an
// error-shaped body. An agent's tool loop branches on isError; returning a
// normal result meant a caller had to parse prose to notice the refusal.
func refuseUnknownID(id json.RawMessage, tool, paymentID string) {
	toolError(id, "mcp-stub refuses "+tool+" for "+paymentID+
		": it impersonates the provider only for the exact synthetic fixture set, "+
		"and this id is not in it. This is not an authorization decision -- the "+
		"stub has no policy -- it is a refusal to pretend a possibly-real payment "+
		"exists or was refunded.")
}

func toolError(id json.RawMessage, msg string) {
	result(id, map[string]any{
		"content": []any{map[string]string{"type": "text", "text": msg}},
		"isError": true,
	})
}

func main() {
	var fx fixtures
	if err := json.Unmarshal(fixturesJSON, &fx); err != nil {
		fmt.Fprintf(os.Stderr, "mcp-stub: bad fixtures: %v\n", err)
		os.Exit(1)
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		// Notifications carry no id and get no reply.
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}

		switch req.Method {
		case "initialize":
			result(req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "rzp-study-stub", "version": "1"},
			})

		case "tools/list":
			result(req.ID, map[string]any{"tools": fx.Tools})

		case "tools/call":
			a := req.Params.Arguments
			switch req.Params.Name {
			case "fetch_payment":
				// An id outside the fixture set is refused; pay_SYN8099 is INSIDE
				// it and still has no body, so it keeps returning not-found --
				// that distinction is what C02 tests.
				if !knownSyntheticID(&fx, a.PaymentID) {
					refuseUnknownID(req.ID, "fetch_payment", a.PaymentID)
					continue
				}
				p, ok := fx.Payments[a.PaymentID]
				if !ok {
					toolError(req.ID, fmt.Sprintf(
						"fetching payment failed: no payment found with id %s", a.PaymentID))
					continue
				}
				result(req.ID, map[string]any{
					"content": []any{map[string]string{"type": "text", "text": string(p)}},
				})

			case "fetch_multiple_refunds_for_payment":
				if !knownSyntheticID(&fx, a.PaymentID) {
					refuseUnknownID(req.ID, "fetch_multiple_refunds_for_payment", a.PaymentID)
					continue
				}
				toolText(req.ID, map[string]any{
					"entity": "collection", "count": 0, "items": []any{},
				})

			case "create_refund":
				// ONE refusal, and it is not a policy.
				//
				// The stub has no policy of its own and must not acquire one --
				// see the package comment. This check is about IDENTIFIER
				// PROVENANCE, not authorization: a program that impersonates a
				// payment provider should refuse to impersonate it for an
				// identifier that might name something real.
				//
				// It cannot confound the study. Every payment id in the frozen
				// briefs, mandates and examples is pay_SYN* (verified: zero
				// exceptions), so this branch is unreachable for the experiment
				// and fires only for input the study never contains.
				//
				// It exists because external review pointed out that "use only
				// pay_SYN ids" was a naming convention in a document rather
				// than anything enforced. A red-team reproducer that reaches
				// for a real-looking id now fails loudly here instead of
				// quietly appearing to work.
				if !knownSyntheticID(&fx, a.PaymentID) {
					refuseUnknownID(req.ID, "create_refund", a.PaymentID)
					continue
				}
				// Never rejected beyond that: see the package comment. The
				// refund id is derived from (payment, amount, receipt) so a
				// trace is reproducible and two distinct refunds get distinct
				// ids.
				sum := sha256.Sum256([]byte(a.PaymentID + "|" + a.Amount.String() + "|" + a.Receipt))
				amt, err := a.Amount.Int64()
				if err != nil {
					// Forward whatever arrived rather than silently repairing
					// it; a non-integer amount reaching here is itself a
					// finding about the guard.
					toolText(req.ID, map[string]any{
						"id": "rfnd_STUB" + hex.EncodeToString(sum[:5]), "entity": "refund",
						"payment_id": a.PaymentID, "amount": a.Amount,
						"receipt": a.Receipt, "status": "pending", "currency": "INR",
					})
					continue
				}
				toolText(req.ID, map[string]any{
					"id": "rfnd_STUB" + hex.EncodeToString(sum[:5]), "entity": "refund",
					"payment_id": a.PaymentID, "amount": amt,
					"receipt": a.Receipt, "status": "pending", "currency": "INR",
				})

			default:
				toolError(req.ID, "unknown tool: "+req.Params.Name)
			}

		default:
			// Unknown method: answer so the caller is never left waiting.
			send(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"error": map[string]any{"code": -32601, "message": "method not found: " + req.Method}})
		}
	}
}
