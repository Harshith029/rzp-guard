package main

import "encoding/json"

// Provider-neutral agent loop.
//
// The study originally spoke one API. It now has to speak two, and they are not
// variations on a theme: OpenAI's Responses API is an item stream correlated by
// call_id, while Bedrock's Converse API is a message list whose content blocks
// are a union keyed by member name. Trying to express one in the other's shape
// produces silent misencodings, so each provider owns its own history format
// and only these neutral types cross the boundary.
//
// What must NOT differ between providers is the experiment: same system prompt,
// same task text, same tool schemas, same temperature, same turn cap, same
// recording. Everything below the interface is transport.

// agentCall is one tool invocation the model asked for.
type agentCall struct {
	ID       string
	Name     string
	Args     map[string]any
	ArgsJSON string
}

// agentToolResult is what the harness hands back for one call.
type agentToolResult struct {
	Call    agentCall
	Text    string
	IsError bool
}

// agentReply is one model turn.
//
// RawInput and RawOutput are the verbatim wire payloads. They are what makes a
// trace auditable: a reader can see exactly what the model was sent and exactly
// what it returned, rather than this program's interpretation of either.
type agentReply struct {
	Text string
	// ServedModel and ResponseID are the endpoint's own account of what
	// answered. Recorded per TURN, not per trace: an alias can be repointed
	// mid-run, and a trace storing only the requested name could not show it.
	ServedModel  string
	ResponseID   string
	Calls        []agentCall
	InputTokens  int
	OutputTokens int
	RawInput     []json.RawMessage
	RawOutput    []json.RawMessage
}

// llmSession is a stateful conversation with one model.
//
// The implementation owns the conversation history, because preserving it
// correctly is provider-specific: OpenAI needs reasoning items echoed back or
// multi-step tool use degrades, and Bedrock needs the assistant message
// replayed verbatim so toolUseId correlation survives.
type llmSession interface {
	// Next sends the conversation so far and returns the model's turn.
	Next() (*agentReply, error)
	// Provide appends results for the calls returned by the previous Next.
	Provide(results []agentToolResult)
	// Temperature reports the value actually in force. It goes nil if the
	// PROTOCOL.md 4.1 contingency fired, so the trace records what really ran.
	Temperature() *float64
}
