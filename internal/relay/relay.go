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
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/harshith/rzp-guard/internal/policy"
)

// DecisionSink receives every decision for the log and dashboard.
type DecisionSink func(policy.Decision, json.RawMessage)

// Relay wires an agent-facing stream to a child MCP server.
type Relay struct {
	guard *policy.Guard
	now   func() time.Time
	sink  DecisionSink

	mu       sync.Mutex
	childIn  io.Writer
	agentOut io.Writer
	inflight map[string]string // JSON-RPC id -> reserved action id
}

func New(g *policy.Guard, childIn, agentOut io.Writer, sink DecisionSink) *Relay {
	if sink == nil {
		sink = func(policy.Decision, json.RawMessage) {}
	}
	return &Relay{
		guard: g, childIn: childIn, agentOut: agentOut, sink: sink,
		now:      func() time.Time { return time.Now().UTC() },
		inflight: map[string]string{},
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

	if msg.Method != "tools/call" {
		// Everything else -- initialize, tools/list, notifications, resources --
		// is forwarded byte-for-byte.
		return r.writeChild(line)
	}

	var tp toolCallParams
	pdec := json.NewDecoder(bytes.NewReader(msg.Params))
	pdec.UseNumber()
	if err := pdec.Decode(&tp); err != nil {
		return r.writeAgent(errorResponse(msg.ID, -32602,
			fmt.Sprintf("rzp-guard: could not parse tools/call params: %v", err)))
	}

	d := r.guard.Decide(tp.Name, tp.Arguments, r.now())
	r.sink(d, msg.ID)

	if !d.Allowed {
		// The blocked path: answer the agent, write nothing to the child.
		return r.writeAgent(toolDenied(msg.ID, d))
	}

	if d.MatchedActionID != "" && len(msg.ID) > 0 {
		r.mu.Lock()
		r.inflight[string(msg.ID)] = d.MatchedActionID
		r.mu.Unlock()
	}

	forwarded, err := rewriteArguments(line, msg, tp, d)
	if err != nil {
		// Fail closed: if the approved call cannot be re-encoded exactly, the
		// original is NOT forwarded, and the reservation is rolled back.
		if d.MatchedActionID != "" {
			_ = r.guard.ReleaseConfirmedRejection(d.MatchedActionID)
			r.mu.Lock()
			delete(r.inflight, string(msg.ID))
			r.mu.Unlock()
		}
		return r.writeAgent(errorResponse(msg.ID, -32603,
			fmt.Sprintf("rzp-guard: refusing to forward, re-encode failed: %v", err)))
	}
	return r.writeChild(forwarded)
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
		if err := dec.Decode(&msg); err == nil && len(msg.ID) > 0 {
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
// A previous revision released the authorization on any JSON-RPC error or any
// isError result, on the assumption that an error means the request was
// rejected before execution. That assumption was never verified and is not
// safe: the child can fail after dispatching the HTTP request, or while
// formatting a response to a refund Razorpay actually processed. Either shape
// can represent a processed-but-unreported refund, so both hold the action and
// the budget.
//
// Releasing after forwarding requires a typed rejection demonstrated in Test
// Mode to occur strictly before provider execution. Until gate G1.6 establishes
// that, every non-success is ambiguous.
func (r *Relay) resolve(id string, msg rpcMessage) {
	r.mu.Lock()
	actionID, ok := r.inflight[id]
	if ok {
		delete(r.inflight, id)
	}
	r.mu.Unlock()
	if !ok {
		return
	}
	if len(msg.Error) > 0 || isToolError(msg.Result) || len(msg.Result) == 0 {
		_ = r.guard.MarkInDoubt(actionID)
		return
	}
	_ = r.guard.Commit(actionID)
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

// CloseInflight marks every unresolved reservation IN_DOUBT. Call when the child
// exits or the session ends: an unanswered refund is exactly the ambiguous case.
func (r *Relay) CloseInflight() []string {
	r.mu.Lock()
	pending := make([]string, 0, len(r.inflight))
	for id, actionID := range r.inflight {
		pending = append(pending, actionID)
		delete(r.inflight, id)
	}
	r.mu.Unlock()
	for _, actionID := range pending {
		_ = r.guard.MarkInDoubt(actionID)
	}
	return pending
}

func (r *Relay) writeChild(line []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.childIn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("relay: write to child: %w", err)
	}
	return nil
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
