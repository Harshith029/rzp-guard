// Package relay is a transparent JSON-RPC stdio interposer.
//
// It sits between an MCP client and Razorpay's official MCP server, which it
// runs as a child process. Every message except tools/call is forwarded
// byte-for-byte; the relay never originates a JSON-RPC request of its own, so
// "transparent relay" is literally true rather than aspirational.
//
// The ordering that matters: a denied tools/call is answered from here and its
// bytes are NEVER written to the child's stdin. A call whose bytes never
// entered the child process cannot have produced an HTTP request to Razorpay,
// which is what makes the block provable rather than asserted.
//
// Amounts are decoded with json.Decoder.UseNumber so paise are never silently
// turned into float64 and rounded.
package relay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/harshith/rzp-guard/internal/policy"
)

// DecisionSink receives every decision for the log and dashboard.
type DecisionSink func(policy.Decision, json.RawMessage)

// ErrDuplicateRequestID is returned to the agent when a JSON-RPC id is reused
// while the original is still outstanding.
var ErrDuplicateRequestID = errors.New("duplicate in-flight JSON-RPC id")

// pending is everything needed to decide whether a child reply really is the
// reply to THIS refund. Correlating on the JSON-RPC id alone is not enough: an
// id can be reused, and a reply carrying a matching id proves nothing about
// which refund the provider executed.
type pending struct {
	actionID  string
	paymentID string
	amount    int64
	receipt   string
	isRefund  bool
}

// Relay wires an agent-facing stream to a child MCP server.
type Relay struct {
	guard *policy.Guard
	now   func() time.Time
	sink  DecisionSink

	mu       sync.Mutex
	childIn  io.Writer
	agentOut io.Writer
	// Keyed by JSON-RPC id. EVERY outstanding request is tracked, reads
	// included, so a read cannot reuse a refund's id and have its success
	// commit the refund.
	inflight map[string]pending
}

func New(g *policy.Guard, childIn, agentOut io.Writer, sink DecisionSink) *Relay {
	if sink == nil {
		sink = func(policy.Decision, json.RawMessage) {}
	}
	return &Relay{
		guard: g, childIn: childIn, agentOut: agentOut, sink: sink,
		now:      func() time.Time { return time.Now().UTC() },
		inflight: map[string]pending{},
	}
}

// SetClock is for tests that need a fixed time.
func (r *Relay) SetClock(f func() time.Time) { r.now = f }

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type toolCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// PumpAgent reads the agent's stream and forwards or blocks each message.
func (r *Relay) PumpAgent(agentIn io.Reader) error {
	sc := bufio.NewScanner(agentIn)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		out := make([]byte, len(line))
		copy(out, line)
		if err := r.handleAgentLine(out); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (r *Relay) handleAgentLine(line []byte) error {
	var msg rpcMessage
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&msg); err != nil {
		// Unparseable input is not silently dropped and not forwarded: the child
		// must never receive bytes this relay could not inspect.
		return r.writeAgent(errorResponse(nil, -32700,
			fmt.Sprintf("rzp-guard: could not parse JSON-RPC message: %v", err)))
	}

	// An agent RESPONSE to a server-initiated request is forwarded untouched and
	// never tracked. Tracking it would leak its id forever, because the child
	// does not reply to a reply.
	if isResponse(msg) {
		_, err := r.writeChild(line)
		return err
	}

	// Duplicate-id refusal applies to every outbound REQUEST, not just
	// tools/call. Otherwise an agent could send an authorized refund as id 8 and
	// then tools/list as id 8: the read is forwarded, its reply resolves the
	// refund's correlation entry, and the refund is forced into IN_DOUBT by a
	// response that has nothing to do with it.
	tracked := isRequest(msg)
	if tracked && r.isInFlight(string(msg.ID)) {
		return r.writeAgent(errorResponse(msg.ID, -32600,
			fmt.Sprintf("rzp-guard: %v: id %s is still outstanding",
				ErrDuplicateRequestID, msg.ID)))
	}

	if msg.Method != "tools/call" {
		// Everything else -- initialize, tools/list, notifications, resources --
		// is forwarded byte-for-byte, but an outbound request is still tracked
		// so its id cannot be reused while outstanding.
		if tracked {
			r.mu.Lock()
			r.inflight[string(msg.ID)] = pending{}
			r.mu.Unlock()
		}
		_, err := r.writeChild(line)
		if err != nil && tracked {
			r.mu.Lock()
			delete(r.inflight, string(msg.ID))
			r.mu.Unlock()
		}
		return err
	}

	var tp toolCallParams
	pdec := json.NewDecoder(bytes.NewReader(msg.Params))
	pdec.UseNumber()
	if err := pdec.Decode(&tp); err != nil {
		return r.writeAgent(errorResponse(msg.ID, -32602,
			fmt.Sprintf("rzp-guard: could not parse tools/call params: %v", err)))
	}

	// A money-moving tool REQUIRES a request id.
	//
	// Without one there is no correlation entry and no reply lifecycle: the
	// reservation would sit RESERVED forever in a running process, and
	// CloseInflight could not promote it for operator resolution. An
	// un-answerable refund is not a refund anyone can be accountable for.
	if tp.Name == policy.RefundTool && !tracked {
		return r.writeAgent(errorResponse(nil, -32600,
			"rzp-guard: create_refund requires a non-null JSON-RPC id; a refund "+
				"sent as a notification has no reply lifecycle and could never be "+
				"resolved or recovered"))
	}

	d := r.guard.Decide(tp.Name, tp.Arguments, r.now())
	r.sink(d, msg.ID)

	if !d.Allowed {
		// The blocked path: answer the agent, write nothing to the child.
		return r.writeAgent(toolDenied(msg.ID, d))
	}

	if tracked {
		p := pending{actionID: d.MatchedActionID, isRefund: d.MatchedActionID != ""}
		if p.isRefund {
			p.paymentID, _ = d.Forwarded["payment_id"].(string)
			p.amount = d.AuthorizedPaise
			p.receipt = d.Receipt
		}
		r.mu.Lock()
		r.inflight[string(msg.ID)] = p
		r.mu.Unlock()
	}

	forwarded, err := rewriteArguments(line, msg, tp, d)
	if err != nil {
		// Fail closed: if the approved call cannot be re-encoded exactly, the
		// original is NOT forwarded, and the reservation is rolled back. Nothing
		// has been written, so releasing is safe here.
		if d.MatchedActionID != "" {
			_ = r.guard.ReleaseConfirmedRejection(d.MatchedActionID)
		}
		r.mu.Lock()
		delete(r.inflight, string(msg.ID))
		r.mu.Unlock()
		return r.writeAgent(errorResponse(msg.ID, -32603,
			fmt.Sprintf("rzp-guard: refusing to forward, re-encode failed: %v", err)))
	}

	n, werr := r.writeChild(forwarded)
	if werr != nil {
		// A partial write is ambiguous: bytes the child accepted may already have
		// reached Razorpay. Only a write that moved ZERO bytes is provably
		// pre-dispatch and safe to release.
		if d.MatchedActionID != "" {
			if n == 0 {
				_ = r.guard.ReleaseConfirmedRejection(d.MatchedActionID)
			} else {
				_ = r.guard.MarkInDoubt(d.MatchedActionID)
			}
		}
		r.mu.Lock()
		delete(r.inflight, string(msg.ID))
		r.mu.Unlock()
		return werr
	}
	return nil
}

// hasRequestID reports whether an id is present and not JSON null.
// A missing id and an explicit null are both notifications.
func hasRequestID(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

// JSON-RPC is bidirectional: MCP allows the server to send requests to the
// client (sampling, roots), so both streams carry a mix of requests, responses
// and notifications. Correlation state must key off REQUESTS only.
//
// An earlier revision keyed off "has an id", which conflated the two. A
// response envelope travelling agent->child was tracked as a new outstanding
// request whose id could never be released, because the child does not reply to
// a reply. In the other direction a child-initiated REQUEST was fed to resolve()
// and could settle an unrelated refund.

// isRequest reports whether a message asks for a reply: a method plus an id.
func isRequest(m rpcMessage) bool { return m.Method != "" && hasRequestID(m.ID) }

// isResponse reports whether a message answers an earlier request: an id and no
// method.
//
// A result or error is deliberately NOT required. A child message carrying an id
// and no method is a response even when malformed, and it must still settle the
// reservation -- as IN_DOUBT, via the missing-result branch in resolve. Requiring
// a well-formed body would leave the action RESERVED in a running process until
// session end, which is the same stuck-forever failure that id-less refunds have.
func isResponse(m rpcMessage) bool {
	return m.Method == "" && hasRequestID(m.ID)
}

func (r *Relay) isInFlight(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.inflight[id]
	return ok
}

// rewriteArguments replaces the arguments with the guard's canonical version
// (integer amount, injected receipt) and leaves every other field untouched.
func rewriteArguments(line []byte, msg rpcMessage, tp toolCallParams,
	d policy.Decision) ([]byte, error) {
	if d.Forwarded == nil {
		return line, nil
	}
	var envelope map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&envelope); err != nil {
		return nil, err
	}
	var params map[string]json.RawMessage
	pdec := json.NewDecoder(bytes.NewReader(msg.Params))
	pdec.UseNumber()
	if err := pdec.Decode(&params); err != nil {
		return nil, err
	}
	args, err := json.Marshal(d.Forwarded)
	if err != nil {
		return nil, err
	}
	params["arguments"] = args
	rawParams, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	envelope["params"] = rawParams
	return json.Marshal(envelope)
}

// PumpChild reads the child's stream, resolves reservations, and forwards
// results to the agent unchanged.
func (r *Relay) PumpChild(childOut io.Reader) error {
	sc := bufio.NewScanner(childOut)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		out := make([]byte, len(line))
		copy(out, line)

		var msg rpcMessage
		dec := json.NewDecoder(bytes.NewReader(out))
		dec.UseNumber()
		// Only a RESPONSE settles a reservation. A child-initiated request also
		// carries an id, and feeding it to resolve() could settle an unrelated
		// refund whose id happened to match.
		if err := dec.Decode(&msg); err == nil && isResponse(msg) {
			r.resolve(string(msg.ID), msg)
		}
		if err := r.writeAgent(out); err != nil {
			return err
		}
	}
	return sc.Err()
}

// resolve moves a reservation to its outcome.
//
// ONCE BYTES HAVE REACHED THE CHILD, THE ONLY AUTOMATIC OUTCOMES ARE COMMIT AND
// IN_DOUBT. There is deliberately no auto-release here.
//
// Two assumptions were removed from earlier revisions, both of which treated
// JSON syntax as execution evidence:
//
//  1. that a JSON-RPC error or isError result proves the request was rejected
//     BEFORE provider execution -- it does not; the child can fail after
//     dispatching the HTTP request;
//  2. that any non-error result proves the refund succeeded -- it does not;
//     `result: null`, an unparseable body, or a reply that merely shares the id
//     would all have committed.
//
// Commit now requires a refund entity that matches the payment, the amount AND
// the injected receipt. Anything else is IN_DOUBT.
func (r *Relay) resolve(id string, msg rpcMessage) {
	r.mu.Lock()
	p, ok := r.inflight[id]
	if ok {
		delete(r.inflight, id)
	}
	r.mu.Unlock()
	if !ok || !p.isRefund {
		// Reads carry no reservation; nothing to resolve.
		return
	}
	if len(msg.Error) > 0 || isToolError(msg.Result) || len(msg.Result) == 0 {
		_ = r.guard.MarkInDoubt(p.actionID)
		return
	}
	if !refundEntityMatches(msg.Result, p) {
		// The reply is not recognisably the refund we authorized. It may still
		// have executed, so hold it for an operator rather than guessing.
		_ = r.guard.MarkInDoubt(p.actionID)
		return
	}
	_ = r.guard.Commit(p.actionID)
}

func isToolError(result json.RawMessage) bool {
	if len(result) == 0 {
		return false
	}
	var probe struct {
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(result, &probe); err != nil {
		return false
	}
	return probe.IsError
}

// refundEntityMatches reports whether a tool result carries a refund entity for
// exactly the payment, amount and receipt this relay authorized.
//
// VERIFIED against a real Test Mode envelope (gate G1.6, 2026-08-25): payment
// pay_TTwUH29tzhB4ME, 100 paise, refund rfnd_TTwf8Hhbx0sjZQ. The exact bytes the
// provider returned are pinned in testdata/live_refund_result.json and asserted
// by TestLiveRefundEnvelopeCommits, so this is no longer a compatibility guess.
//
// status is deliberately NOT read. The live envelope came back "pending" and
// only became "processed" asynchronously, after the MCP reply had been sent, so
// no synchronous reply can prove settlement. COMMITTED therefore means "the
// provider created the refund entity" -- enough to consume a single-use action
// and prevent a replay -- and never "the money has landed".
//
// An unrecognised success shape still yields IN_DOUBT and an operator look,
// never a wrong COMMIT.
func refundEntityMatches(result json.RawMessage, p pending) bool {
	var wrapper struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &wrapper); err != nil || len(wrapper.Content) == 0 {
		return false
	}
	for _, c := range wrapper.Content {
		if c.Type != "text" || c.Text == "" {
			continue
		}
		var e struct {
			ID        string      `json:"id"`
			Entity    string      `json:"entity"`
			PaymentID string      `json:"payment_id"`
			Amount    json.Number `json:"amount"`
			Receipt   string      `json:"receipt"`
		}
		dec := json.NewDecoder(bytes.NewReader([]byte(c.Text)))
		dec.UseNumber()
		if err := dec.Decode(&e); err != nil {
			continue
		}
		if e.Entity != "refund" || e.ID == "" {
			// A provider-assigned identifier is the minimum evidence that an
			// object was actually created. Without it, any structurally matching
			// echo of our own request would count as execution evidence.
			continue
		}
		amt, err := e.Amount.Int64()
		if err != nil {
			continue
		}
		if e.PaymentID == p.paymentID && amt == p.amount && e.Receipt == p.receipt {
			return true
		}
	}
	return false
}

// CloseInflight marks every unresolved reservation IN_DOUBT. Call when the child
// exits or the session ends: an unanswered refund is exactly the ambiguous case.
func (r *Relay) CloseInflight() []string {
	r.mu.Lock()
	stranded := make([]string, 0, len(r.inflight))
	for id, p := range r.inflight {
		if p.isRefund {
			stranded = append(stranded, p.actionID)
		}
		delete(r.inflight, id)
	}
	r.mu.Unlock()
	for _, actionID := range stranded {
		_ = r.guard.MarkInDoubt(actionID)
	}
	return stranded
}

// writeChild returns the byte count as well as the error, because the caller
// must distinguish "nothing left this process" from "some bytes were accepted
// and the provider-side outcome is unknown".
func (r *Relay) writeChild(line []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.childIn.Write(append(line, '\n'))
	if err != nil {
		return n, fmt.Errorf("relay: write to child: %w", err)
	}
	return n, nil
}

func (r *Relay) writeAgent(line []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.agentOut.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("relay: write to agent: %w", err)
	}
	return nil
}

// toolDenied renders a block as a normal tool error, so the calling agent sees
// a readable reason rather than a transport failure.
func toolDenied(id json.RawMessage, d policy.Decision) []byte {
	text := fmt.Sprintf("BLOCKED by rzp-guard [%s]: %s", d.Rule, d.Reason)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      rawOrNull(id),
		"result": map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": true,
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

func errorResponse(id json.RawMessage, code int, message string) []byte {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      rawOrNull(id),
		"error":   map[string]any{"code": code, "message": message},
	}
	b, _ := json.Marshal(payload)
	return b
}

func rawOrNull(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	return json.RawMessage(id)
}
