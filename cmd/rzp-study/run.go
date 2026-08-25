package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

type brief struct {
	BriefID   string `json:"brief_id"`
	Family    string `json:"family"`
	AgentTask string `json:"agent_task"`
	// intent_text is deliberately NOT loaded. It is the adjudication ground
	// truth and must never reach the model or influence a run.
}

// systemPrompt is read from the FROZEN protocol, not duplicated in Go source.
// One copy exists, it is hash-covered by the freeze, and the prompt the model
// receives is provably the prompt that was registered.
func systemPrompt() (string, error) {
	b, err := os.ReadFile(filepath.Join(studyDir(), "PROTOCOL.md"))
	if err != nil {
		return "", err
	}
	const anchor = "### 6.1 System prompt"
	i := strings.Index(string(b), anchor)
	if i < 0 {
		return "", fmt.Errorf("PROTOCOL.md has no %q section", anchor)
	}
	rest := string(b)[i+len(anchor):]
	start := strings.Index(rest, "```")
	if start < 0 {
		return "", fmt.Errorf("no fenced block after %q", anchor)
	}
	rest = rest[start+3:]
	if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
		rest = rest[nl+1:]
	}
	end := strings.Index(rest, "```")
	if end < 0 {
		return "", fmt.Errorf("unterminated fenced block after %q", anchor)
	}
	p := strings.TrimSpace(rest[:end])
	if p == "" {
		return "", fmt.Errorf("system prompt in PROTOCOL.md is empty")
	}
	return p, nil
}

func loadBriefs(only string) ([]brief, error) {
	paths, err := filepath.Glob(filepath.Join(studyDir(), "briefs", "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []brief
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var br brief
		if err := json.Unmarshal(b, &br); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if only != "" && br.BriefID != only {
			continue
		}
		out = append(out, br)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no briefs matched")
	}
	return out, nil
}

// ---------------------------------------------------------------- trace

type toolCallRecord struct {
	Turn       int    `json:"turn"`
	CallID     string `json:"call_id"`
	Name       string `json:"name"`
	Arguments  string `json:"arguments"`
	ResultText string `json:"result_text"`
	IsError    bool   `json:"is_error"`
	Blocked    bool   `json:"blocked_by_guard"`
}

type trace struct {
	BriefID      string            `json:"brief_id"`
	Family       string            `json:"family"`
	RunIndex     int               `json:"run_index"`
	Model        string            `json:"model"`
	Temperature  *float64          `json:"temperature"`
	StartedAt    string            `json:"started_at_utc"`
	Status       string            `json:"status"`
	VoidReason   string            `json:"void_reason,omitempty"`
	Turns        int               `json:"turns"`
	ToolCalls    []toolCallRecord  `json:"tool_calls"`
	FinalText    string            `json:"final_text"`
	Decisions    []json.RawMessage `json:"guard_decisions"`
	InputTokens  int               `json:"input_tokens"`
	OutputTokens int               `json:"output_tokens"`
	GuardStderr  string            `json:"guard_stderr,omitempty"`
	FreezeSHA    string            `json:"freeze_sha256"`

	// Which model freeze this trace ran under, and the commit that fixed it.
	// Recorded per-trace so a reader can check the model was committed before
	// the trace rather than taking the claim on trust.
	ModelFreezeSHA string `json:"model_freeze_sha256,omitempty"`
	ModelCommit    string `json:"model_freeze_commit,omitempty"`

	// Messages is the COMPLETE exchange: the exact input array sent on every
	// turn and the exact output array returned. PROTOCOL.md 7 claimed the run
	// records every message while the code recorded only tool calls and the
	// final text -- the claim was ahead of the implementation.
	Messages []turnRecord `json:"messages"`
}

// turnRecord is one full request/response pair, stored verbatim.
type turnRecord struct {
	Turn   int               `json:"turn"`
	Input  []json.RawMessage `json:"input"`
	Output []json.RawMessage `json:"output"`
}

type runner struct {
	guard, operator, stub string
	outDir                string
	client                *openAI
	model                 string
	temp                  *float64
	sysPrompt             string
	freezeSHA             string
	dryRun                bool
	modelFreeze           *modelFreeze
	maxTurns              int
}

const maxTurnsFrozen = 12

// dryRunModel stamps every trace produced without a real model, so a
// downstream step can refuse to treat scripted output as a measurement.
const dryRunModel = "DRY-RUN-SCRIPTED-FAKE"

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	guard := fs.String("guard", ".gotmp/linux/rzp-guard-th", "rzp-guard binary (test-hook build)")
	operator := fs.String("operator", ".gotmp/linux/rzp-guard-operator-th", "operator binary")
	stub := fs.String("stub", ".gotmp/linux/mcp-stub", "stub MCP child binary")
	outDir := fs.String("out", "study/traces", "trace output directory")
	only := fs.String("only", "", "run a single brief id")
	runs := fs.Int("runs", 0, "runs per brief (0 = the frozen 3)")
	dry := fs.Bool("dry-run", false, "scripted fake model, no API calls")
	if err := fs.Parse(args); err != nil {
		return err
	}

	m, err := verifyFreeze()
	if err != nil {
		return err
	}
	sp, err := systemPrompt()
	if err != nil {
		return err
	}
	briefs, err := loadBriefs(*only)
	if err != nil {
		return err
	}

	perBrief := 3
	if *dry {
		if *runs > 0 {
			perBrief = *runs
		}
	} else if err := requireFullTraceSet(len(briefs), perBrief, m.DeclaredTraceCount, *only, *runs); err != nil {
		return err
	}

	r := &runner{
		guard: *guard, operator: *operator, stub: *stub, outDir: *outDir,
		sysPrompt: sp, freezeSHA: m.FreezeSHA256, dryRun: *dry, maxTurns: maxTurnsFrozen,
	}

	if *dry {
		r.model = dryRunModel
	} else {
		// Every one of these fails closed, before a single token is spent.
		mf, err := requireCommittedModelFreeze()
		if err != nil {
			return err
		}
		if err := requireEmptyTraceDir(r.outDir); err != nil {
			return err
		}
		fm, err := loadFrozenModel()
		if err != nil {
			return err
		}
		// Provider and endpoint come from the COMMITTED freeze, never from the
		// environment, so a run cannot be redirected at a different service
		// after the fact.
		prov, err := providerFromFrozen(fm)
		if err != nil {
			return err
		}
		if err := validateModelID(prov, fm.Model); err != nil {
			return err
		}
		key, err := prov.credential()
		if err != nil {
			return err
		}
		r.modelFreeze = mf
		r.model, r.temp, r.client = fm.Model, fm.Temperature, newOpenAI(prov, key)
		fmt.Printf("provider %s (%s)\n", prov.Name, fm.Endpoint)
	}

	if err := os.MkdirAll(r.outDir, 0o755); err != nil {
		return err
	}

	total := len(briefs) * perBrief
	fmt.Printf("freeze %s\n", m.FreezeSHA256)
	fmt.Printf("model  %s\n", r.model)
	fmt.Printf("traces %d (%d briefs x %d runs)\n\n", total, len(briefs), perBrief)

	done := 0
	for _, br := range briefs {
		for run := 1; run <= perBrief; run++ {
			done++
			t := r.runTrace(br, run)
			path := filepath.Join(r.outDir, fmt.Sprintf("%s_run%d.json", br.BriefID, run))
			b, _ := json.MarshalIndent(t, "", "  ")
			if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
				return err
			}
			blocked := 0
			refunds := 0
			for _, c := range t.ToolCalls {
				if c.Name == "create_refund" {
					refunds++
					if c.Blocked {
						blocked++
					}
				}
			}
			fmt.Printf("  [%2d/%2d] %-4s run%d  %-10s turns=%-2d refund_calls=%d blocked=%d %s\n",
				done, total, br.BriefID, run, t.Status, t.Turns, refunds, blocked, t.VoidReason)
		}
	}
	fmt.Printf("\ntraces written to %s\n", r.outDir)
	return nil
}

// runTrace executes one (brief, run) pair against a FRESH guard and a fresh
// state file. Nothing carries between traces: a consumed action in one must
// never affect another.
// The result is NAMED on purpose. The deferred block below records the guard's
// stderr and its decision log, and with an unnamed result the return value is
// copied BEFORE defers run -- so those fields silently arrived empty. Caught by
// reading a trace file instead of trusting the summary line.
func (r *runner) runTrace(br brief, run int) (t trace) {
	t = trace{
		BriefID: br.BriefID, Family: br.Family, RunIndex: run,
		Model: r.model, Temperature: r.temp,
		StartedAt: nowUTC(), FreezeSHA: r.freezeSHA, Status: "complete",
	}
	if r.modelFreeze != nil {
		t.ModelFreezeSHA = r.modelFreeze.SHA256
		t.ModelCommit = r.modelFreeze.Commit
	}

	dir, err := os.MkdirTemp("", "rzpstudy")
	if err != nil {
		t.Status, t.VoidReason = "void", "tempdir: "+err.Error()
		return t
	}
	defer os.RemoveAll(dir)

	statePath := filepath.Join(dir, "state.db")
	tokenPath := filepath.Join(dir, "token")
	decisionLog := filepath.Join(dir, "decisions.jsonl")
	mandatePath := filepath.Join(studyDir(), "mandates", br.BriefID+".json")

	// Provisioning is a deployment step and the guard refuses an unprovisioned
	// state file, so the study performs it explicitly, exactly as an operator
	// would. The token is discarded with the temp dir.
	prov := exec.Command(r.operator, "-mandate", mandatePath, "-state", statePath,
		"init", "-out", tokenPath)
	if out, err := prov.CombinedOutput(); err != nil {
		t.Status, t.VoidReason = "void", "provisioning: "+strings.TrimSpace(string(out))
		return t
	}

	sess, err := startMCP(r.guard, mandatePath, statePath, decisionLog, r.stub)
	if err != nil {
		t.Status, t.VoidReason = "void", "starting guard: "+err.Error()
		return t
	}
	defer func() {
		t.GuardStderr = strings.TrimSpace(sess.close())
		t.Decisions = readDecisions(decisionLog)
	}()

	if err := sess.initialize(); err != nil {
		t.Status, t.VoidReason = "void", "mcp initialize: "+err.Error()
		return t
	}
	tools, err := sess.listTools()
	if err != nil {
		t.Status, t.VoidReason = "void", "tools/list: "+err.Error()
		return t
	}

	if r.dryRun {
		r.driveScripted(&t, sess, br)
		return t
	}
	r.driveModel(&t, sess, br, mcpToolsToOpenAI(tools))
	return t
}

func readDecisions(path string) []json.RawMessage {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []json.RawMessage
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, json.RawMessage(line))
	}
	return out
}

// blockedByGuard identifies the guard's own refusal, as distinct from a child
// or transport error. The guard prefixes every denial with its own name.
func blockedByGuard(res mcpResult) bool {
	return res.IsError && strings.Contains(res.Text, "BLOCKED by rzp-guard")
}

// ---------------------------------------------------------------- model loop

func (r *runner) driveModel(t *trace, sess *mcpSession, br brief, tools []anyMap) {
	input := []any{
		anyMap{"role": "user", "content": br.AgentTask},
	}
	temp := r.temp

	for turn := 1; turn <= r.maxTurns; turn++ {
		t.Turns = turn
		reply, err := r.client.respond(responsesRequest{
			Model:        r.model,
			Instructions: r.sysPrompt,
			Input:        input,
			Tools:        tools,
			Temperature:  temp,
		})
		if err != nil {
			// PROTOCOL.md 4.1 contingency: retry once without the parameter and
			// record that the run used the model default, rather than silently
			// running a different experiment.
			if strings.Contains(err.Error(), errTemperatureUnsupported.Error()) && temp != nil {
				temp = nil
				t.Temperature = nil
				continue
			}
			t.Status, t.VoidReason = "void", "responses api: "+err.Error()
			return
		}

		snapshot := make([]json.RawMessage, 0, len(input))
		for _, it := range input {
			b, err := json.Marshal(it)
			if err != nil {
				continue
			}
			snapshot = append(snapshot, b)
		}
		t.Messages = append(t.Messages, turnRecord{
			Turn: turn, Input: snapshot, Output: reply.rawOutput,
		})
		t.InputTokens += reply.Usage.InputTokens
		t.OutputTokens += reply.Usage.OutputTokens

		// Echo every returned item back on the next turn, reasoning items
		// included; dropping them degrades multi-step tool use.
		for _, raw := range reply.rawOutput {
			input = append(input, json.RawMessage(raw))
		}

		var calls []outputItem
		for _, item := range reply.Output {
			switch item.Type {
			case "function_call":
				calls = append(calls, item)
			case "message":
				if txt := extractText(item.Raw); txt != "" {
					t.FinalText = txt
				}
			}
		}
		if len(calls) == 0 {
			t.Status = "complete"
			return
		}

		for _, c := range calls {
			args := map[string]any{}
			if c.Arguments != "" {
				_ = json.Unmarshal([]byte(c.Arguments), &args)
			}
			res, err := sess.callTool(c.Name, args)
			if err != nil {
				t.Status, t.VoidReason = "void", "tool call: "+err.Error()
				return
			}
			t.ToolCalls = append(t.ToolCalls, toolCallRecord{
				Turn: turn, CallID: c.CallID, Name: c.Name, Arguments: c.Arguments,
				ResultText: res.Text, IsError: res.IsError, Blocked: blockedByGuard(res),
			})
			input = append(input, anyMap{
				"type": "function_call_output", "call_id": c.CallID, "output": res.Text,
			})
		}
	}
	t.Status = "turn_limit"
}

func extractText(raw json.RawMessage) string {
	var m struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range m.Content {
		b.WriteString(c.Text)
	}
	return b.String()
}

// ---------------------------------------------------------------- dry run

// driveScripted replaces the model with a fixed script so the whole path --
// provisioning, guard, stub, decision logging, trace recording -- can be
// exercised without an API key and without spending anything.
//
// It is NOT a study result and never produces one: every trace it writes is
// stamped with the DRY-RUN model name.
func (r *runner) driveScripted(t *trace, sess *mcpSession, br brief) {
	mandate := struct {
		Actions []struct {
			PaymentID   string `json:"payment_id"`
			AmountPaise int64  `json:"amount_paise"`
		} `json:"authorized_refund_actions"`
	}{}
	b, err := os.ReadFile(filepath.Join(studyDir(), "mandates", br.BriefID+".json"))
	if err == nil {
		_ = json.Unmarshal(b, &mandate)
	}

	type scripted struct {
		name string
		args map[string]any
	}
	var script []scripted

	// A read the mandate always permits.
	pid := "pay_SYN8001"
	if len(mandate.Actions) > 0 {
		pid = mandate.Actions[0].PaymentID
	}
	script = append(script, scripted{"fetch_payment", map[string]any{"payment_id": pid}})

	if len(mandate.Actions) > 0 {
		a := mandate.Actions[0]
		// Should be ALLOWED.
		script = append(script, scripted{"create_refund",
			map[string]any{"payment_id": a.PaymentID, "amount": a.AmountPaise}})
		// Should be BLOCKED: same payment, an amount no action authorizes.
		script = append(script, scripted{"create_refund",
			map[string]any{"payment_id": a.PaymentID, "amount": a.AmountPaise + 77777}})
	} else {
		// Mandate authorizes nothing: any refund must be blocked.
		script = append(script, scripted{"create_refund",
			map[string]any{"payment_id": pid, "amount": 12345}})
	}

	t.Turns = 1
	for i, s := range script {
		res, err := sess.callTool(s.name, s.args)
		if err != nil {
			t.Status, t.VoidReason = "void", "tool call: "+err.Error()
			return
		}
		argsJSON, _ := json.Marshal(s.args)
		t.ToolCalls = append(t.ToolCalls, toolCallRecord{
			Turn: 1, CallID: fmt.Sprintf("dry_%d", i), Name: s.name,
			Arguments: string(argsJSON), ResultText: res.Text,
			IsError: res.IsError, Blocked: blockedByGuard(res),
		})
	}
	t.FinalText = "(dry run: scripted, no model was called)"
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}
