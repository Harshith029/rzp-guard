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
	BriefID     string            `json:"brief_id"`
	Family      string            `json:"family"`
	RunIndex    int               `json:"run_index"`
	Model       string            `json:"model"`
	Temperature *float64          `json:"temperature"`
	StartedAt   string            `json:"started_at_utc"`
	Status      string            `json:"status"`
	VoidReason  string            `json:"void_reason,omitempty"`
	Turns       int               `json:"turns"`
	ToolCalls   []toolCallRecord  `json:"tool_calls"`
	FinalText   string            `json:"final_text"`
	Decisions   []json.RawMessage `json:"guard_decisions"`
	// Smoke marks a trace produced by the integration check rather than by the
	// study. It is written into the file itself so a stray trace can never be
	// mistaken for a result.
	Smoke bool `json:"smoke,omitempty"`
	// ServedModel is the model the endpoint said answered, as distinct from the
	// alias that was requested. An alias is a pointer and pointers move, so
	// "which model produced this trace" is evidenced rather than assumed. Per
	// turn as well, in Messages.
	ServedModel  string `json:"served_model,omitempty"`
	InputTokens  int    `json:"input_tokens"`
	OutputTokens int    `json:"output_tokens"`
	GuardStderr  string `json:"guard_stderr,omitempty"`
	FreezeSHA    string `json:"freeze_sha256"`

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
	Turn int `json:"turn"`
	// The endpoint's account of this specific exchange. Recorded per turn so a
	// mid-run alias repoint is visible in the evidence rather than invisible.
	ServedModel string            `json:"served_model,omitempty"`
	ResponseID  string            `json:"response_id,omitempty"`
	Input       []json.RawMessage `json:"input"`
	Output      []json.RawMessage `json:"output"`
}

type runner struct {
	guard, operator, stub string
	outDir                string
	model                 string
	temp                  *float64
	sysPrompt             string
	freezeSHA             string
	dryRun                bool
	smoke                 bool
	modelFreeze           *modelFreeze
	maxTurns              int

	// newSession builds one conversation. A factory rather than a client
	// because the providers speak different wire formats and each owns its own
	// history representation; see llm.go.
	newSession func(system, task string, tools []mcpTool) llmSession
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
	armName := fs.String("arm", "A", "which study arm (see study/arms.json)")
	outDir := fs.String("out", "", "trace output directory (default: the arm's)")
	only := fs.String("only", "", "run a single brief id")
	runs := fs.Int("runs", 0, "runs per brief (0 = the frozen 3)")
	dry := fs.Bool("dry-run", false, "scripted fake model, no API calls")
	smoke := fs.Bool("smoke", false,
		"one real trace to prove the integration; not a study run, cannot write under study/")
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
	switch {
	case *dry:
		if *runs > 0 {
			perBrief = *runs
		}
	case *smoke:
		// A smoke trace exists to prove the integration works BEFORE the real
		// run starts. Without it the only way to discover a broken transport is
		// to burn the pre-declared 45, then delete and re-run -- which is
		// exactly the "re-run until it works" freedom the pre-registration
		// exists to remove. It is one brief, one run, and it may not write
		// anywhere a study artifact lives.
		perBrief = 1
		if len(briefs) > 1 {
			briefs = briefs[:1]
		}
		if err := refuseStudyPath(*outDir); err != nil {
			return err
		}
	default:
		if err := requireFullTraceSet(len(briefs), perBrief, m.DeclaredTraceCount, *only, *runs); err != nil {
			return err
		}
	}

	// Resolve the arm's paths. A dry or smoke run supplies its own and needs no
	// registry entry.
	modelFreezePath := filepath.Join(studyDir(), "model.frozen.json")
	if !*dry && !*smoke {
		reg, err := loadArms()
		if err != nil {
			return err
		}
		a, err := reg.find(*armName)
		if err != nil {
			return err
		}
		if a.Status == "complete" {
			return fmt.Errorf("arm %s is already complete (%d traces under %s). "+
				"Re-running it would overwrite a recorded run; declare a new arm instead",
				a.Arm, m.DeclaredTraceCount, a.tracePath())
		}
		modelFreezePath = a.modelPath()
		if *outDir == "" {
			*outDir = a.tracePath()
		}
		fmt.Printf("arm    %s\n", a.Arm)
	}
	if *outDir == "" {
		*outDir = filepath.Join(studyDir(), "traces")
	}

	r := &runner{
		guard: *guard, operator: *operator, stub: *stub, outDir: *outDir,
		sysPrompt: sp, freezeSHA: m.FreezeSHA256, dryRun: *dry, smoke: *smoke,
		maxTurns: maxTurnsFrozen,
	}

	if *dry {
		r.model = dryRunModel
	} else {
		// Every one of these fails closed, before a single token is spent.
		//
		// The committed-model-freeze requirement is the single check a smoke
		// trace relaxes, and only because it cannot be satisfied yet: the point
		// of the smoke trace is to validate the model choice BEFORE committing
		// it. Everything else -- freeze integrity, immutable output, the
		// study/ path ban -- still applies.
		var mf *modelFreeze
		if r.smoke {
			fmt.Fprintln(os.Stderr,
				"SMOKE TRACE: not a study run. The model freeze is not required to be "+
					"committed, and output may not be written under study/.")
		} else {
			mf, err = requireCommittedModelFreeze(modelFreezePath)
			if err != nil {
				return err
			}
		}
		if err := requireEmptyTraceDir(r.outDir); err != nil {
			return err
		}
		fm, err := loadFrozenModel(modelFreezePath)
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
		r.model, r.temp = fm.Model, fm.Temperature
		switch prov.API {
		case apiMessages:
			ac := newAnthropicClient(prov.BaseURL, key)
			r.newSession = func(system, task string, tools []mcpTool) llmSession {
				return newAnthropicSession(ac, r.model, system, task, r.temp, tools)
			}
		default:
			oc := newOpenAI(prov, key)
			r.newSession = func(system, task string, tools []mcpTool) llmSession {
				return newOpenAISession(oc, r.model, system, task, r.temp, tools)
			}
		}
		fmt.Printf("endpoint %s\n", fm.Endpoint)
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
		Smoke: r.smoke,
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
	r.driveModel(&t, sess, br, tools)
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

// driveModel is the agent loop, and is provider-agnostic on purpose: the
// experiment must not differ by transport. Same prompt, same task, same tool
// schemas, same turn cap, same recording, whichever API is underneath.
func (r *runner) driveModel(t *trace, sess *mcpSession, br brief, tools []mcpTool) {
	conv := r.newSession(r.sysPrompt, br.AgentTask, tools)

	for turn := 1; turn <= r.maxTurns; turn++ {
		t.Turns = turn

		reply, err := conv.Next()
		if err != nil {
			t.Status, t.VoidReason = "void", "model: "+err.Error()
			return
		}

		// Whatever temperature actually ended up in force, including nil if the
		// PROTOCOL.md 4.1 contingency fired. Recorded per-trace so the fallback
		// is visible rather than assumed.
		t.Temperature = conv.Temperature()

		t.Messages = append(t.Messages, turnRecord{
			Turn: turn, ServedModel: reply.ServedModel, ResponseID: reply.ResponseID,
			Input: reply.RawInput, Output: reply.RawOutput,
		})
		if reply.ServedModel != "" {
			t.ServedModel = reply.ServedModel
		}
		t.InputTokens += reply.InputTokens
		t.OutputTokens += reply.OutputTokens
		if reply.Text != "" {
			t.FinalText = reply.Text
		}

		if len(reply.Calls) == 0 {
			t.Status = "complete"
			return
		}

		results := make([]agentToolResult, 0, len(reply.Calls))
		for _, c := range reply.Calls {
			res, err := sess.callTool(c.Name, c.Args)
			if err != nil {
				t.Status, t.VoidReason = "void", "tool call: "+err.Error()
				return
			}
			t.ToolCalls = append(t.ToolCalls, toolCallRecord{
				Turn: turn, CallID: c.ID, Name: c.Name, Arguments: c.ArgsJSON,
				ResultText: res.Text, IsError: res.IsError, Blocked: blockedByGuard(res),
			})
			results = append(results, agentToolResult{
				Call: c, Text: res.Text, IsError: res.IsError,
			})
		}
		conv.Provide(results)
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
