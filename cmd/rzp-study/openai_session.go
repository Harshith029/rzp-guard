package main

import (
	"encoding/json"
	"strings"
)

// openaiSession adapts the Responses API to llmSession.
//
// Kept alongside the Bedrock path rather than deleted: the protocol's provider
// is a recorded parameter, and being able to re-run the same frozen study
// against a different provider is worth more than the few lines it costs.

type openaiSession struct {
	client *openAI
	model  string
	system string
	tools  []anyMap
	temp   *float64
	input  []any
}

func newOpenAISession(c *openAI, model, system, task string,
	temp *float64, tools []mcpTool) *openaiSession {
	return &openaiSession{
		client: c,
		model:  model,
		system: system,
		tools:  mcpToolsToOpenAI(tools),
		temp:   temp,
		input:  []any{anyMap{"role": "user", "content": task}},
	}
}

func (s *openaiSession) Temperature() *float64 { return s.temp }

func (s *openaiSession) Next() (*agentReply, error) {
	reply, err := s.client.respond(responsesRequest{
		Model:        s.model,
		Instructions: s.system,
		Input:        s.input,
		Tools:        s.tools,
		Temperature:  s.temp,
	})
	if err != nil {
		// PROTOCOL.md 4.1 contingency: drop the parameter once and record it.
		if s.temp != nil && isTemperatureRejection(err) {
			s.temp = nil
			return s.Next()
		}
		return nil, err
	}

	snapshot := make([]json.RawMessage, 0, len(s.input))
	for _, it := range s.input {
		if b, err := json.Marshal(it); err == nil {
			snapshot = append(snapshot, b)
		}
	}

	// Echo every returned item back next turn, reasoning items included;
	// dropping them degrades multi-step tool use, and this study is entirely
	// multi-step.
	for _, raw := range reply.rawOutput {
		s.input = append(s.input, json.RawMessage(raw))
	}

	out := &agentReply{
		InputTokens:  reply.Usage.InputTokens,
		OutputTokens: reply.Usage.OutputTokens,
		RawInput:     snapshot,
		RawOutput:    reply.rawOutput,
	}
	for _, item := range reply.Output {
		switch item.Type {
		case "function_call":
			args := map[string]any{}
			if item.Arguments != "" {
				_ = json.Unmarshal([]byte(item.Arguments), &args)
			}
			out.Calls = append(out.Calls, agentCall{
				ID:       item.CallID,
				Name:     item.Name,
				Args:     args,
				ArgsJSON: item.Arguments,
			})
		case "message":
			if txt := extractText(item.Raw); txt != "" {
				out.Text = txt
			}
		}
	}
	return out, nil
}

func (s *openaiSession) Provide(results []agentToolResult) {
	for _, r := range results {
		s.input = append(s.input, anyMap{
			"type":    "function_call_output",
			"call_id": r.Call.ID,
			"output":  r.Text,
		})
	}
}

func isTemperatureRejection(err error) bool {
	return err != nil && strings.Contains(err.Error(), errTemperatureUnsupported.Error())
}
