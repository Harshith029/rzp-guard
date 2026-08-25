package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// The Responses API, not Chat Completions.
//
// CORRECTION: an earlier version of this comment claimed Chat Completions was
// legacy with a removal window closed in early 2026. That was FALSE and is
// withdrawn -- /v1/chat/completions is current and supported; the 2026 shutdown
// is the Assistants API. See FAILURES.md F17.
//
// The real reason to use Responses here is reasoning-item preservation: items
// returned alongside a tool call are echoed back on the next turn, and dropping
// them degrades multi-step tool use. This study is entirely multi-step, so an
// agent losing its reasoning between turns would re-issue calls and behave
// worse than a real deployment for reasons unrelated to the guard -- inflating
// the very count being measured. Chat Completions would likely also work; it is
// simply worse-suited to this shape of task.
// Endpoint constants live in provider.go: the base URL is chosen at
// resolve-model time, recorded in the freeze, and read back from there.

type openAI struct {
	key     string
	baseURL string
	http    *http.Client
}

func newOpenAI(p *provider, key string) *openAI {
	return &openAI{
		key:     key,
		baseURL: p.BaseURL,
		http:    &http.Client{Timeout: 180 * time.Second},
	}
}

func (c *openAI) do(method, path string, body any) ([]byte, int, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	return raw, resp.StatusCode, err
}

type modelInfo struct {
	ID      string `json:"id"`
	Created int64  `json:"created"`
	Object  string `json:"object"`
}

func (c *openAI) listModels() ([]modelInfo, error) {
	raw, code, err := c.do(http.MethodGet, modelsPath, nil)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("models list: HTTP %d: %s", code, truncate(string(raw), 300))
	}
	var r struct {
		Data []modelInfo `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return r.Data, nil
}

// responsesRequest mirrors the Responses API request body.
//
// Temperature is a pointer so it can be OMITTED entirely rather than sent as
// zero. Support varies by model -- several current reasoning models reject the
// parameter outright -- and a silent 0.0 would be a different experiment than
// the frozen one.
type responsesRequest struct {
	Model        string   `json:"model"`
	Instructions string   `json:"instructions,omitempty"`
	Input        []any    `json:"input"`
	Tools        []anyMap `json:"tools,omitempty"`
	Temperature  *float64 `json:"temperature,omitempty"`
}

type anyMap = map[string]any

// outputItem is one typed item from the response output array. Items are echoed
// back verbatim on the next turn, INCLUDING reasoning items -- current guidance
// is explicit that dropping them degrades multi-step tool use.
type outputItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

type responsesReply struct {
	ID     string       `json:"id"`
	Model  string       `json:"model"`
	Status string       `json:"status"`
	Output []outputItem `json:"output"`
	Usage  struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`

	rawOutput []json.RawMessage
}

var errTemperatureUnsupported = fmt.Errorf("model rejected the temperature parameter")

// errModelDrift fires when the endpoint answers with a model other than the one
// requested.
//
// A trace stores the model ALIAS that was asked for. An alias is a pointer, and
// pointers move: a provider can repoint one mid-study, and across 45 calls that
// would be invisible in a trace recording only what was requested. Worse, a
// rejected third-party proxy demonstrated the extreme form of this, serving
// grok-4.6 for a gpt-5.6 request.
//
// This matters to every number the study reports, not just the model-specific
// one. Each denominator counts calls the agent ACTUALLY EMITTED, so a change of
// model changes the call distribution and moves the measured rates while the
// guard stands still. Drift is therefore a hard error, not a note.
var errModelDrift = fmt.Errorf("endpoint answered with a different model than requested")

func (c *openAI) respond(req responsesRequest) (*responsesReply, error) {
	raw, code, err := c.do(http.MethodPost, responsesPath, req)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		msg := truncate(string(raw), 500)
		// Distinguished so the caller can apply the documented contingency
		// instead of silently proceeding with different sampling.
		if req.Temperature != nil && strings.Contains(strings.ToLower(msg), "temperature") {
			return nil, fmt.Errorf("%w: %s", errTemperatureUnsupported, msg)
		}
		return nil, fmt.Errorf("responses: HTTP %d: %s", code, msg)
	}

	var r responsesReply
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	if r.Model != "" && r.Model != req.Model {
		return nil, fmt.Errorf("%w: requested %q, answered %q (response %s)",
			errModelDrift, req.Model, r.Model, r.ID)
	}
	// Keep the raw items so they can be replayed into the next turn without
	// lossy round-tripping through our own struct.
	var shape struct {
		Output []json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &shape); err == nil {
		r.rawOutput = shape.Output
		for i := range r.Output {
			if i < len(shape.Output) {
				r.Output[i].Raw = shape.Output[i]
			}
		}
	}
	return &r, nil
}

// mcpToolsToOpenAI converts MCP tool definitions into Responses API function
// tools. The schema is passed through UNCHANGED -- these are the real captured
// Razorpay schemas, and rewriting them would mean the agent under test saw
// something production never sends.
//
// `strict` is deliberately not set. Strict mode requires every property to be
// required and additionalProperties:false; Razorpay's schemas satisfy neither,
// so enabling it would reject the real tools.
func mcpToolsToOpenAI(tools []mcpTool) []anyMap {
	out := make([]anyMap, 0, len(tools))
	for _, t := range tools {
		var params any
		if len(t.InputSchema) > 0 {
			if err := json.Unmarshal(t.InputSchema, &params); err != nil {
				continue
			}
		} else {
			params = anyMap{"type": "object", "properties": anyMap{}}
		}
		out = append(out, anyMap{
			"type":        "function",
			"name":        t.Name,
			"description": t.Description,
			"parameters":  params,
		})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
