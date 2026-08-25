package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Messages wire format and the model-substitution control, both testable
// without credentials. A local server records what we SEND and returns canned
// replies.

func refundTools() []mcpTool {
	return []mcpTool{{
		Name:        "create_refund",
		Description: "Create a normal refund for a payment.",
		InputSchema: json.RawMessage(`{"type":"object",` +
			`"properties":{"payment_id":{"type":"string"},"amount":{"type":"number","minimum":100}},` +
			`"required":["payment_id","amount"]}`),
	}}
}

const toolUseReply = `{
  "id": "msg_1",
  "model": "gpt-5.6-sol",
  "stop_reason": "tool_use",
  "content": [
    {"type": "text", "text": "Refunding the damaged item."},
    {"type": "tool_use", "id": "call_abc", "name": "create_refund",
     "input": {"payment_id": "pay_SYN8001", "amount": 24000}}
  ],
  "usage": {"input_tokens": 412, "output_tokens": 63}
}`

func messagesServer(t *testing.T, reply string, seen *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		*seen = parsed
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q, want /v1/messages", r.URL.Path)
		}
		if got := r.Header.Get("x-api-key"); got != "test-key" {
			t.Errorf("x-api-key = %q", got)
		}
		if got := r.Header.Get("anthropic-version"); got != anthropicVersion {
			t.Errorf("anthropic-version = %q, want %q", got, anthropicVersion)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
}

func TestMessagesRequestShape(t *testing.T) {
	var seen map[string]any
	srv := messagesServer(t, toolUseReply, &seen)
	defer srv.Close()

	temp := 0.2
	s := newAnthropicSession(newAnthropicClient(srv.URL, "test-key"),
		"gpt-5.6-sol", "You are a support agent.", "Refund the atta.", &temp, refundTools())

	if _, err := s.Next(); err != nil {
		t.Fatalf("Next: %v", err)
	}

	if seen["system"] != "You are a support agent." {
		t.Fatalf("system = %v; the Messages API takes a top-level string", seen["system"])
	}
	if seen["max_tokens"] != float64(anthropicMaxTokens) {
		t.Fatalf("max_tokens = %v", seen["max_tokens"])
	}
	if seen["temperature"] != 0.2 {
		t.Fatalf("temperature = %v, want the frozen 0.2", seen["temperature"])
	}

	msgs := seen["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("first role = %v", first["role"])
	}
	blk := first["content"].([]any)[0].(map[string]any)
	if blk["type"] != "text" {
		t.Fatalf(`first block type = %v; Messages blocks ARE discriminated by "type"`, blk["type"])
	}
	if blk["text"] != "Refund the atta." {
		t.Fatalf("task text = %v", blk["text"])
	}

	// The real Razorpay schema must pass through untouched, under input_schema.
	tool := seen["tools"].([]any)[0].(map[string]any)
	if tool["name"] != "create_refund" {
		t.Fatalf("tool name = %v", tool["name"])
	}
	schema, ok := tool["input_schema"].(map[string]any)
	if !ok {
		t.Fatal("tool has no input_schema")
	}
	amount := schema["properties"].(map[string]any)["amount"].(map[string]any)
	if amount["type"] != "number" {
		t.Fatalf(`amount type = %v; the REAL Razorpay schema says "number", and `+
			`rewriting it would hide the fractional-amount case the guard defends`,
			amount["type"])
	}
}

func TestMessagesParsesToolUse(t *testing.T) {
	var seen map[string]any
	srv := messagesServer(t, toolUseReply, &seen)
	defer srv.Close()

	s := newAnthropicSession(newAnthropicClient(srv.URL, "test-key"),
		"gpt-5.6-sol", "sys", "task", nil, refundTools())
	reply, err := s.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if reply.Text != "Refunding the damaged item." {
		t.Fatalf("text = %q", reply.Text)
	}
	if len(reply.Calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(reply.Calls))
	}
	c := reply.Calls[0]
	if c.ID != "call_abc" || c.Name != "create_refund" {
		t.Fatalf("call = %+v", c)
	}
	if c.Args["payment_id"] != "pay_SYN8001" || c.Args["amount"] != float64(24000) {
		t.Fatalf("args = %+v", c.Args)
	}
	if reply.InputTokens != 412 || reply.OutputTokens != 63 {
		t.Fatalf("usage = %d/%d", reply.InputTokens, reply.OutputTokens)
	}
	if s.ServedModel() != "gpt-5.6-sol" {
		t.Fatalf("ServedModel = %q", s.ServedModel())
	}
	if len(reply.RawOutput) == 0 {
		t.Fatal("RawOutput is empty; the verbatim payload is what makes a trace auditable")
	}
}

// THE control that makes a proxy usable for a pre-registered study. Asking this
// endpoint for gpt-5.6 really did return grok-4.6, so a substitution must be a
// hard error -- a trace produced by an unknown model is not the experiment that
// was registered.
func TestMessagesRejectsModelSubstitution(t *testing.T) {
	var seen map[string]any
	substituted := strings.Replace(toolUseReply, `"model": "gpt-5.6-sol"`, `"model": "grok-4.6"`, 1)
	srv := messagesServer(t, substituted, &seen)
	defer srv.Close()

	s := newAnthropicSession(newAnthropicClient(srv.URL, "test-key"),
		"gpt-5.6-sol", "sys", "task", nil, refundTools())

	_, err := s.Next()
	if err == nil {
		t.Fatal("a substituted model must be an error, not silently accepted")
	}
	if !strings.Contains(err.Error(), "grok-4.6") || !strings.Contains(err.Error(), "gpt-5.6-sol") {
		t.Fatalf("error must name both the requested and served model: %v", err)
	}
}

// A guard BLOCK is reported to the model as an error tool_result, because how
// the agent reacts to a refusal is part of what this study observes.
func TestMessagesEncodesToolResults(t *testing.T) {
	var seen map[string]any
	srv := messagesServer(t, toolUseReply, &seen)
	defer srv.Close()

	s := newAnthropicSession(newAnthropicClient(srv.URL, "test-key"),
		"gpt-5.6-sol", "sys", "task", nil, refundTools())
	reply, err := s.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}

	s.Provide([]agentToolResult{{
		Call:    reply.Calls[0],
		Text:    "BLOCKED by rzp-guard [AMOUNT_NOT_AUTHORIZED]: 99900 paise is not authorized",
		IsError: true,
	}})
	if _, err := s.Next(); err != nil {
		t.Fatalf("second Next: %v", err)
	}

	msgs := seen["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want user/assistant/user", len(msgs))
	}

	// The assistant turn must be replayed VERBATIM: dropping the tool_use id
	// breaks correlation and the result never lands.
	assistant := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("second message role = %v", assistant["role"])
	}
	var found bool
	for _, b := range assistant["content"].([]any) {
		m := b.(map[string]any)
		if m["type"] == "tool_use" && m["id"] == "call_abc" {
			found = true
		}
	}
	if !found {
		t.Fatal("assistant turn was not replayed with its tool_use block")
	}

	result := msgs[2].(map[string]any)
	if result["role"] != "user" {
		t.Fatalf("tool results must be a user message, got %v", result["role"])
	}
	tr := result["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" {
		t.Fatalf("block type = %v", tr["type"])
	}
	if tr["tool_use_id"] != "call_abc" {
		t.Fatalf("tool_use_id = %v", tr["tool_use_id"])
	}
	if tr["is_error"] != true {
		t.Fatalf("is_error = %v; a guard block must be reported as an error so the "+
			"agent knows it was refused", tr["is_error"])
	}
	if !strings.Contains(tr["content"].(string), "BLOCKED by rzp-guard") {
		t.Fatalf("block reason did not reach the model: %v", tr["content"])
	}
}

// PROTOCOL.md 4.1: if the endpoint rejects temperature, drop it ONCE and record
// that the run used the default. Never silently substitute 0.0.
func TestMessagesTemperatureContingency(t *testing.T) {
	var calls int
	var sawTemp []bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]any
		_ = json.Unmarshal(body, &parsed)
		_, has := parsed["temperature"]
		sawTemp = append(sawTemp, has)
		calls++
		if has {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"error":{"message":"temperature is not supported"}}`)
			return
		}
		io.WriteString(w, toolUseReply)
	}))
	defer srv.Close()

	temp := 0.2
	s := newAnthropicSession(newAnthropicClient(srv.URL, "test-key"),
		"gpt-5.6-sol", "sys", "task", &temp, refundTools())

	if _, err := s.Next(); err != nil {
		t.Fatalf("Next should have recovered by dropping temperature: %v", err)
	}
	if calls != 2 {
		t.Fatalf("made %d calls, want 2 (rejected, then retried without it)", calls)
	}
	if !sawTemp[0] || sawTemp[1] {
		t.Fatalf("temperature presence per call = %v, want [true false]", sawTemp)
	}
	if s.Temperature() != nil {
		t.Fatal("Temperature() must report nil after the contingency fires")
	}
}
