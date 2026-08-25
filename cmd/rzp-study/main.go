// Command rzp-study runs the Phase 4b agent-trace study.
//
// It refuses to run traces unless the freeze in study/manifest.json is intact,
// so a brief edited after the fact cannot quietly become the ground truth it
// was supposed to precede.
//
// Everything the agent can do goes through rzp-guard. There is no
// direct-to-provider path in this program.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "verify-freeze":
		err = cmdVerifyFreeze()
	case "resolve-model":
		err = cmdResolveModel()
	case "run":
		err = cmdRun(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "rzp-study: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: rzp-study <command>

  verify-freeze    check study/manifest.json against the files on disk
  resolve-model    resolve and record the model id (PROTOCOL.md 4), pre-trace
  run [flags]      run the traces

run flags:
  -guard PATH      rzp-guard binary (test-hook build, for the stub child)
  -operator PATH   rzp-guard-operator binary
  -stub PATH       mcp-stub binary
  -out DIR         trace output directory (default study/traces)
  -only ID         run a single brief, e.g. A01 (for smoke tests)
  -runs N          override runs per brief (default: the frozen 3)
  -dry-run         drive the guard with a scripted fake model, no API calls
`)
}

// ---------------------------------------------------------------- freeze

type manifest struct {
	DeclaredTraceCount int               `json:"declared_trace_count"`
	Files              map[string]string `json:"files"`
	FreezeSHA256       string            `json:"freeze_sha256"`
}

func studyDir() string { return "study" }

func loadManifest() (*manifest, error) {
	b, err := os.ReadFile(filepath.Join(studyDir(), "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("reading freeze manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func fileDigest(rel string) (string, error) {
	b, err := os.ReadFile(filepath.Join(studyDir(), rel))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// verifyFreeze recomputes every recorded digest AND re-derives the aggregate,
// so an added or removed file is caught as well as an edited one.
func verifyFreeze() (*manifest, error) {
	m, err := loadManifest()
	if err != nil {
		return nil, err
	}
	var problems []string
	for rel, want := range m.Files {
		got, err := fileDigest(rel)
		if err != nil {
			problems = append(problems, "MISSING  "+rel)
			continue
		}
		if got != want {
			problems = append(problems, "CHANGED  "+rel)
		}
	}
	for _, sub := range []string{"briefs", "mandates"} {
		entries, err := filepath.Glob(filepath.Join(studyDir(), sub, "*.json"))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			rel := sub + "/" + filepath.Base(e)
			if _, ok := m.Files[rel]; !ok {
				problems = append(problems, "ADDED    "+rel+" (not in the freeze)")
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("FREEZE VIOLATED:\n  %s", strings.Join(problems, "\n  "))
	}

	var joined strings.Builder
	keys := make([]string, 0, len(m.Files))
	for k := range m.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&joined, "%s:%s\n", k, m.Files[k])
	}
	sum := sha256.Sum256([]byte(joined.String()))
	if got := hex.EncodeToString(sum[:]); got != m.FreezeSHA256 {
		return nil, fmt.Errorf("freeze_sha256 mismatch: manifest says %s, files give %s",
			m.FreezeSHA256, got)
	}
	return m, nil
}

func cmdVerifyFreeze() error {
	m, err := verifyFreeze()
	if err != nil {
		return err
	}
	fmt.Printf("freeze intact: %d files, freeze_sha256 %s\n", len(m.Files), m.FreezeSHA256)
	fmt.Printf("declared trace count: %d\n", m.DeclaredTraceCount)
	return nil
}

// ---------------------------------------------------------------- model

type frozenModel struct {
	Model           string   `json:"model"`
	ResolvedAt      string   `json:"resolved_at_utc"`
	SelectionRule   string   `json:"selection_rule"`
	Candidates      []string `json:"candidates_considered"`
	TotalListed     int      `json:"total_models_listed"`
	Temperature     *float64 `json:"temperature"`
	TemperatureNote string   `json:"temperature_note"`
	Endpoint        string   `json:"endpoint"`
}

// Size and reasoning variants are excluded so the choice is mechanical rather
// than a judgement call made after seeing the list. -pro is excluded on cost:
// it is an order of magnitude dearer per trace and this study runs 45 of them.
var (
	flagshipRE = regexp.MustCompile(`^gpt-(\d+)(?:\.(\d+))?$`)
	excludeRE  = regexp.MustCompile(`mini|nano|pro|preview|audio|realtime|transcribe|tts|embedding|image|moderation|search|codex|instruct|turbo|\d{4}-\d{2}-\d{2}`)
)

func pickModel(models []modelInfo) (string, []string) {
	type cand struct {
		id           string
		major, minor int
	}
	var cands []cand
	var names []string
	for _, m := range models {
		if excludeRE.MatchString(m.ID) {
			continue
		}
		g := flagshipRE.FindStringSubmatch(m.ID)
		if g == nil {
			continue
		}
		major, _ := strconv.Atoi(g[1])
		minor := 0
		if g[2] != "" {
			minor, _ = strconv.Atoi(g[2])
		}
		cands = append(cands, cand{m.ID, major, minor})
		names = append(names, m.ID)
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].major != cands[j].major {
			return cands[i].major > cands[j].major
		}
		return cands[i].minor > cands[j].minor
	})
	sort.Strings(names)
	if len(cands) == 0 {
		return "", names
	}
	return cands[0].id, names
}

func apiKey() (string, error) {
	k := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if k == "" {
		return "", fmt.Errorf("OPENAI_API_KEY is not set")
	}
	return k, nil
}

func cmdResolveModel() error {
	if _, err := verifyFreeze(); err != nil {
		return err
	}
	key, err := apiKey()
	if err != nil {
		return err
	}
	models, err := newOpenAI(key).listModels()
	if err != nil {
		return err
	}
	picked, cands := pickModel(models)
	if picked == "" {
		return fmt.Errorf("no general-purpose flagship model matched the rule; %d models listed", len(models))
	}
	temp := 0.2
	fm := frozenModel{
		Model:      picked,
		ResolvedAt: nowUTC(),
		SelectionRule: "highest gpt-<major>[.<minor>] id, excluding size variants " +
			"(mini/nano), extended-reasoning variants (pro), dated snapshots, and " +
			"non-general-purpose endpoints; see PROTOCOL.md 4",
		Candidates:  cands,
		TotalListed: len(models),
		Temperature: &temp,
		TemperatureNote: "frozen at 0.2; if this model rejects the parameter the run " +
			"applies the PROTOCOL.md 4.1 contingency and records the fallback here",
		Endpoint: apiBase + responsesPath,
	}
	b, _ := json.MarshalIndent(fm, "", "  ")
	path := filepath.Join(studyDir(), "model.frozen.json")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("resolved model: %s\n", picked)
	fmt.Printf("  candidates matching the rule: %s\n", strings.Join(cands, ", "))
	fmt.Printf("  of %d models listed\n", len(models))
	fmt.Printf("  recorded in %s -- commit this BEFORE the first trace\n", path)
	return nil
}

func loadFrozenModel() (*frozenModel, error) {
	b, err := os.ReadFile(filepath.Join(studyDir(), "model.frozen.json"))
	if err != nil {
		return nil, fmt.Errorf("model not resolved yet; run: rzp-study resolve-model (%w)", err)
	}
	var fm frozenModel
	if err := json.Unmarshal(b, &fm); err != nil {
		return nil, err
	}
	if fm.Model == "" {
		return nil, fmt.Errorf("study/model.frozen.json names no model")
	}
	return &fm, nil
}
