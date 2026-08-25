package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// The Anthropic Messages API, spoken to a THIRD-PARTY PROXY.
//
// This is raw net/http rather than the official Anthropic SDK, deliberately:
// the endpoint is not Anthropic. It is an intermediary that mimics the Messages
// wire format while serving other vendors' models. Pointing the official SDK at
// it would import a dependency to talk to a service it was not written for, and
// the SDK is free to add Anthropic-specific parameters and beta headers the
// proxy does not implement. The existing OpenAI client in this package is also
// raw net/http, so this matches the codebase.
//
// Content blocks here ARE discriminated by a "type" field -- unlike Bedrock's
// Converse union. Verified against the live endpoint before this was written.
//
//	{"type": "text",        "text": "..."}
//	{"type": "tool_use",    "id": "...", "name": "...", "input": {...}}
//	{"type": "tool_result", "tool_use_id": "...", "content": "...", "is_error": true}
//
// WHY THE REPORTED MODEL IS CHECKED ON EVERY CALL.
//
// A proxy is an unverifiable claim about model identity, and this one is
// demonstrably loose: asking for "gpt-5.6" returned "grok-4.6". It honours most
// ids (gpt-5.6-sol, claude-opus-5, gpt-4o all echoed back correctly) and 400s
// on nonsense, but silent substitution is real and it happened on the first
// call made.
//
// For a PRE-REGISTERED study the model is a recorded parameter, so "trust the
// proxy" is not good enough. Every response's `model` field is compared with
// what was requested, and a mismatch is surfaced rather than absorbed. That
// turns an untestable claim into a checked one.

const anthropicVersion = "2023-06-01"

type anthropicClient struct {
	baseURL string
	key     string
	http    *http.Client
}

func newAnthropicClient(baseURL, key string) *anthropicClient {
	return &anthropicClient{
		baseURL: baseURL,
		key:     key,
		http:    &http.Client{Timeout: 300 * time.Second},
	}
}

type messagesRequest struct {
	Model       string         `json:"model"`
	MaxTokens   int            `json:"max_tokens"`
	System      string         `json:"system,omitempty"`
	Messages    []anthropicMsg `json:"messages"`
	Tools       []anyMap       `json:"tools,omitempty"`
	Temperature *float64       `json:"temperature,omitempty"`
}

type anthropicMsg struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

type messagesReply struct {
	ID         string            `json:"id"`
	Model      string            `json:"model"`
	StopReason string            `json:"stop_reason"`
	Content    []json.RawMessage `json:"content"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// errModelSubstituted marks the case that makes a proxy untrustworthy for a
// pre-registered study: it served something other than what was asked for.
var errModelSubstituted = fmt.Errorf("proxy served a different model than requested")

func (c *anthropicClient) messages(req messagesRequest) (*messagesReply, []byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	httpReq, err := http.NewRequest(http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	httpReq.Header.Set("x-api-key", c.key)
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	httpReq.Header.Set("content-type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := truncate(string(raw), 500)
		if req.Temperature != nil && bytes.Contains(raw, []byte("temperature")) {
			return nil, raw, fmt.Errorf("%w: %s", errTemperatureUnsupported, msg)
		}
		return nil, raw, fmt.Errorf("messages: HTTP %d: %s", resp.StatusCode, msg)
	}

	var out messagesReply
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, raw, fmt.Errorf("decoding messages reply: %w", err)
	}

	// The control described above. Not a warning -- an error, because a trace
	// produced by an unknown model is not the pre-registered experiment.
	if out.Model != "" && out.Model != req.Model {
		return nil, raw, fmt.Errorf("%w: requested %q, served %q",
			errModelSubstituted, req.Model, out.Model)
	}
	return &out, raw, nil
}

// mcpToolsToAnthropic maps MCP tools onto Messages API tool definitions.
//
// The JSON Schema passes through UNCHANGED as input_schema. These are the real
// captured Razorpay schemas; rewriting them would mean the agent under test saw
// something production never sends -- including create_refund declaring amount
// as {"type":"number"}, which is precisely what makes a fractional amount
// expressible and therefore worth defending against.
func mcpToolsToAnthropic(tools []mcpTool) []anyMap {
	out := make([]anyMap, 0, len(tools))
	for _, t := range tools {
		var schema any
		if len(t.InputSchema) > 0 {
			if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
				continue
			}
		} else {
			schema = anyMap{"type": "object", "properties": anyMap{}}
		}
		out = append(out, anyMap{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": schema,
		})
	}
	return out
}

// ---------------------------------------------------------------- session

const anthropicMaxTokens = 4096

type anthropicSession struct {
	client   *anthropicClient
	model    string
	system   string
	tools    []anyMap
	temp     *float64
	messages []anthropicMsg

	// servedModel is what the endpoint said it used, recorded per trace so the
	// claim is evidenced rather than asserted.
	servedModel string
}

func newAnthropicSession(c *anthropicClient, model, system, task string,
	temp *float64, tools []mcpTool) *anthropicSession {
	return &anthropicSession{
		client: c,
		model:  model,
		system: system,
		tools:  mcpToolsToAnthropic(tools),
		temp:   temp,
		messages: []anthropicMsg{{
			Role:    "user",
			Content: []json.RawMessage{mustJSONBlock(anyMap{"type": "text", "text": task})},
		}},
	}
}

func (s *anthropicSession) Temperature() *float64 { return s.temp }
func (s *anthropicSession) ServedModel() string   { return s.servedModel }

func (s *anthropicSession) Next() (*agentReply, error) {
	req := messagesRequest{
		Model:       s.model,
		MaxTokens:   anthropicMaxTokens,
		System:      s.system,
		Messages:    s.messages,
		Tools:       s.tools,
		Temperature: s.temp,
	}

	reply, raw, err := s.client.messages(req)
	if err != nil {
		// PROTOCOL.md 4.1 contingency: drop the parameter once, record that the
		// run used the endpoint default, and never silently substitute 0.0.
		if s.temp != nil && isTemperatureRejection(err) {
			s.temp = nil
			return s.Next()
		}
		return nil, err
	}
	s.servedModel = reply.Model

	// Replay the assistant turn verbatim. Reconstructing it risks dropping a
	// tool_use id, and correlation is what makes a tool result land.
	s.messages = append(s.messages, anthropicMsg{Role: "assistant", Content: reply.Content})

	out := &agentReply{
		ServedModel:  reply.Model,
		ResponseID:   reply.ID,
		InputTokens:  reply.Usage.InputTokens,
		OutputTokens: reply.Usage.OutputTokens,
		RawInput:     snapshotAnthropic(s.messages[:len(s.messages)-1]),
		RawOutput:    []json.RawMessage{raw},
	}

	for _, block := range reply.Content {
		var probe struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(block, &probe); err != nil {
			continue
		}
		switch probe.Type {
		case "text":
			if probe.Text != "" {
				out.Text = probe.Text
			}
		case "tool_use":
			args := map[string]any{}
			_ = json.Unmarshal(probe.Input, &args)
			out.Calls = append(out.Calls, agentCall{
				ID:       probe.ID,
				Name:     probe.Name,
				Args:     args,
				ArgsJSON: string(probe.Input),
			})
		}
	}
	return out, nil
}

// Provide returns tool results as a single user message. The Messages API
// requires every tool_use in the preceding assistant turn to be answered, and
// splitting results across messages degrades parallel tool use.
func (s *anthropicSession) Provide(results []agentToolResult) {
	if len(results) == 0 {
		return
	}
	blocks := make([]json.RawMessage, 0, len(results))
	for _, r := range results {
		block := anyMap{
			"type":        "tool_result",
			"tool_use_id": r.Call.ID,
			"content":     r.Text,
		}
		if r.IsError {
			// A guard BLOCK is reported to the model as an error. Deliberate:
			// the agent is told it was refused and may react, and how it reacts
			// is part of what this study observes.
			block["is_error"] = true
		}
		blocks = append(blocks, mustJSONBlock(block))
	}
	s.messages = append(s.messages, anthropicMsg{Role: "user", Content: blocks})
}

func mustJSONBlock(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func snapshotAnthropic(msgs []anthropicMsg) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, mustJSONBlock(m))
	}
	return out
}
