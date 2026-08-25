package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// mcpSession drives the GUARD over stdio, exactly as a real MCP client would.
//
// The harness has no direct-to-provider path by construction: this is the only
// way it can reach a tool, and it is pointed at rzp-guard, never at a child.
type mcpSession struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Scanner
	stderr *strings.Builder

	mu     sync.Mutex
	nextID int
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// mcpResult is one tool outcome as the agent sees it.
type mcpResult struct {
	Text    string `json:"text"`
	IsError bool   `json:"is_error"`
}

func startMCP(guardPath, mandatePath, statePath, decisionLog, childCmd string) (*mcpSession, error) {
	cmd := exec.Command(guardPath,
		"-mandate", mandatePath,
		"-state", statePath,
		"-decision-log", decisionLog,
	)
	// The guard refuses a non-rzp_test key id outright. The stub child ignores
	// credentials entirely; these exist only to satisfy that check, and no real
	// key is ever handed to a study process.
	cmd.Env = append(cmd.Environ(),
		"RZP_GUARD_CHILD_CMD="+childCmd,
		"RAZORPAY_KEY_ID=rzp_test_studystub",
		"RAZORPAY_KEY_SECRET=studystub",
	)

	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var errBuf strings.Builder
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	sc := bufio.NewScanner(outPipe)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	s := &mcpSession{cmd: cmd, in: in, out: sc, stderr: &errBuf, nextID: 1}
	return s, nil
}

func (s *mcpSession) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.in.Write(append(b, '\n'))
	return err
}

// readReply blocks until a message carrying the wanted id arrives. Anything
// else on the stream -- notifications, server-initiated requests -- is skipped
// rather than mistaken for the answer.
func (s *mcpSession) readReply(wantID int) (json.RawMessage, error) {
	deadline := time.Now().Add(60 * time.Second)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for id %d; guard stderr: %s",
				wantID, s.stderr.String())
		}
		if !s.out.Scan() {
			if err := s.out.Err(); err != nil {
				return nil, fmt.Errorf("reading guard stdout: %w", err)
			}
			return nil, fmt.Errorf("guard closed stdout before answering id %d; stderr: %s",
				wantID, s.stderr.String())
		}
		line := strings.TrimSpace(s.out.Text())
		if line == "" {
			continue
		}
		var m struct {
			ID     json.Number     `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m.ID.String() != fmt.Sprint(wantID) {
			continue
		}
		if len(m.Error) > 0 {
			return nil, fmt.Errorf("jsonrpc error on id %d: %s", wantID, m.Error)
		}
		return m.Result, nil
	}
}

func (s *mcpSession) call(method string, params any) (json.RawMessage, error) {
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	s.mu.Unlock()

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := s.send(req); err != nil {
		return nil, err
	}
	return s.readReply(id)
}

func (s *mcpSession) initialize() error {
	_, err := s.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "rzp-study", "version": "1"},
	})
	if err != nil {
		return err
	}
	return s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
}

func (s *mcpSession) listTools() ([]mcpTool, error) {
	raw, err := s.call("tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var r struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	return r.Tools, nil
}

// callTool runs one tool and flattens the MCP result into what the model sees.
//
// A guard BLOCK arrives here as an ordinary isError result. That is deliberate:
// the agent is told it was refused and may react, which is the behaviour under
// study. The harness does not intervene.
func (s *mcpSession) callTool(name string, args map[string]any) (mcpResult, error) {
	raw, err := s.call("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		// A JSON-RPC error is still an outcome the agent must be told about.
		return mcpResult{Text: err.Error(), IsError: true}, nil
	}
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return mcpResult{}, err
	}
	var b strings.Builder
	for _, c := range r.Content {
		b.WriteString(c.Text)
	}
	return mcpResult{Text: b.String(), IsError: r.IsError}, nil
}

// close shuts the guard down the way a real client disconnecting would: close
// stdin and let the supervisor run its own shutdown path.
func (s *mcpSession) close() string {
	_ = s.in.Close()
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
	}
	return s.stderr.String()
}
