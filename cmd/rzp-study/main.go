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
	"flag"
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
		err = cmdResolveModel(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "worksheet":
		err = cmdWorksheet(os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
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
  resolve-model    resolve and record provider+endpoint+model (PROTOCOL.md 4),
                   pre-trace. Flags: -provider proxy|openai, -model <id>
  run [flags]      run the traces
  worksheet        emit the BLINDED adjudication worksheet from the traces
  report           join filled verdicts onto the traces -> confusion matrix

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
	Provider        string   `json:"provider"`
	API             string   `json:"api"`
	BaseURL         string   `json:"base_url"`
	Model           string   `json:"model"`
	ResolvedAt      string   `json:"resolved_at_utc"`
	SelectionRule   string   `json:"selection_rule"`
	SelectionMethod string   `json:"selection_method"`
	Candidates      []string `json:"candidates_considered"`
	TotalListed     int      `json:"total_models_listed"`
	Temperature     *float64 `json:"temperature"`
	TemperatureNote string   `json:"temperature_note"`
	Endpoint        string   `json:"endpoint"`
}

// The selection rule, mechanical so the choice cannot be made after seeing a
// result.
//
// The original rule matched a bare gpt-<major>[.<minor>] id. That shape is no
// longer how the flagship line is named: it is TIERED, and on Bedrock ids also
// carry a provider prefix and, on the cross-Region route, a geography prefix.
// So `us.openai.gpt-5.6-terra` must parse to {version 5.6, tier terra}.
//
// Tier preference is decided here, in advance, on the same reasoning already
// used to exclude -pro:
//
//	terra  everyday-production tier -- SELECTED. It is what a real support
//	       deployment would run, and this study is a simulation of one.
//	sol    advanced-reasoning tier -- excluded on cost, exactly as -pro was.
//	luna   fast/high-volume tier -- excluded as a size/speed variant, exactly
//	       as mini and nano were.
//
// An unrecognised tier is excluded rather than guessed at. That fails closed:
// resolve-model then prints every visible id so the rule can be amended from
// real data, pre-trace and committed, instead of silently picking something.
var (
	modelRE   = regexp.MustCompile(`(?:^|\.)gpt-(\d+)(?:\.(\d+))?(?:-([a-z]+))?$`)
	excludeRE = regexp.MustCompile(`mini|nano|pro|preview|audio|realtime|transcribe|tts|embedding|image|moderation|search|codex|instruct|turbo|\d{4}-\d{2}-\d{2}`)
)

const selectionRule = "highest version of the everyday-production tier (terra), " +
	"excluding the advanced-reasoning tier (sol) on cost, the fast/high-volume " +
	"tier (luna) as a size variant, and mini/nano/pro/preview/dated snapshots " +
	"and non-general-purpose endpoints; see PROTOCOL.md 4"

func tierRank(tier string) int {
	switch tier {
	case "terra":
		return 2 // everyday production: the deployment tier being simulated
	case "":
		return 1 // untiered flagship, if a provider still publishes one
	default:
		return 0 // sol, luna, or anything unrecognised: not eligible
	}
}

func pickModel(models []modelInfo) (string, []string) {
	type cand struct {
		id           string
		major, minor int
		rank         int
	}
	var cands []cand
	var names []string
	for _, m := range models {
		if excludeRE.MatchString(m.ID) {
			continue
		}
		g := modelRE.FindStringSubmatch(m.ID)
		if g == nil {
			continue
		}
		rank := tierRank(g[3])
		if rank == 0 {
			continue
		}
		major, _ := strconv.Atoi(g[1])
		minor := 0
		if g[2] != "" {
			minor, _ = strconv.Atoi(g[2])
		}
		cands = append(cands, cand{m.ID, major, minor, rank})
		names = append(names, m.ID)
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].rank != cands[j].rank {
			return cands[i].rank > cands[j].rank
		}
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

// endpointFor records the concrete URL a run will POST to, so the freeze names
// an address rather than a base.
func endpointFor(p *provider) string {
	if p.API == apiMessages {
		return p.BaseURL + "/v1/messages"
	}
	return p.BaseURL + responsesPath
}

// ruleFor records the rule that was ACTUALLY applied.
//
// Recording the tier rule on an operator-supplied choice produced a freeze that
// contradicted itself: it said "excluding the advanced-reasoning tier (sol)"
// while naming gpt-5.6-sol as the model. The tier rule governs enumerable
// endpoints; where an operator supplies the id, saying so is the honest record.
func ruleFor(method string) string {
	if method == "operator-supplied" {
		return "operator-supplied: the endpoint publishes no trustworthy model list " +
			"(it has been observed substituting models), so the tier rule in " +
			"PROTOCOL.md 4 is inapplicable and does not govern this choice. The id " +
			"was chosen because it is one the endpoint routes without substitution, " +
			"and every response is checked against it -- see PROTOCOL.md 4.3."
	}
	return selectionRule
}

func cmdResolveModel(args []string) error {
	fs := flag.NewFlagSet("resolve-model", flag.ExitOnError)
	providerName := fs.String("provider", providerProxy, "proxy | openai")
	explicit := fs.String("model", "",
		"record this model id verbatim instead of enumerating (required when the "+
			"endpoint does not expose a model list)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := verifyFreeze(); err != nil {
		return err
	}

	p, err := resolveProvider(*providerName)
	if err != nil {
		return err
	}
	key, err := p.credential()
	if err != nil {
		return err
	}

	var (
		picked, method string
		cands          []string
		listed         int
	)

	// Where the endpoint cannot be enumerated meaningfully, an operator supplies
	// the id. The proxy publishes no trustworthy list -- it substitutes models
	// silently -- so a "mechanical pick" there would be theatre dressed as rigour.
	if *explicit == "" && !p.enumerates() {
		return fmt.Errorf("provider %q needs an explicit -model.\n"+
			"Its model list is a claim about routing, not a guarantee of what is\n"+
			"served, so a mechanical pick would be meaningless. Supply the id and\n"+
			"it is recorded as operator-supplied; every response is then checked\n"+
			"against it. Example:\n"+
			"  ./run.sh study-model -model gpt-5.6-sol", p.Name)
	}

	if *explicit != "" {
		// Bedrock's OpenAI-compatible route is an inference endpoint and is not
		// guaranteed to enumerate models. Recording an operator-supplied id
		// verbatim is a legitimate resolution, PROVIDED it is recorded as such:
		// the freeze then shows the choice was made by a person, not derived by
		// the rule, and a reader can weigh it accordingly.
		picked, method = *explicit, "operator-supplied"
	} else {
		models, lerr := newOpenAI(p, key).listModels()
		if lerr != nil {
			return fmt.Errorf("could not list models from %s: %w\n"+
				"if this endpoint does not expose a model list, pass -model <id> to "+
				"record one verbatim (it is stored as operator-supplied, not as a "+
				"rule-derived choice)", p.BaseURL, lerr)
		}
		listed = len(models)
		picked, cands = pickModel(models)
		method = "enumerated"
		if picked == "" {
			var all []string
			for _, m := range models {
				all = append(all, m.ID)
			}
			sort.Strings(all)
			fmt.Fprintf(os.Stderr, "no model matched the selection rule. %d models visible:\n", listed)
			for _, id := range all {
				fmt.Fprintf(os.Stderr, "  %s\n", id)
			}
			return fmt.Errorf("selection rule matched nothing; amend PROTOCOL.md 4 from " +
				"the list above BEFORE running any trace, and commit the amendment")
		}
	}

	if err := validateModelID(p, picked); err != nil {
		return err
	}

	temp := 0.2
	fm := frozenModel{
		Provider:        p.Name,
		API:             p.API,
		BaseURL:         p.BaseURL,
		Model:           picked,
		ResolvedAt:      nowUTC(),
		SelectionRule:   ruleFor(method),
		SelectionMethod: method,
		Candidates:      cands,
		TotalListed:     listed,
		Temperature:     &temp,
		TemperatureNote: "frozen at 0.2; if this model rejects the parameter the run " +
			"applies the PROTOCOL.md 4.1 contingency and records the fallback per-trace",
		Endpoint: endpointFor(p),
	}
	b, _ := json.MarshalIndent(fm, "", "  ")
	path := filepath.Join(studyDir(), "model.frozen.json")
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Printf("provider  %s (%s API)\n", p.Name, p.API)
	fmt.Printf("endpoint  %s\n", fm.Endpoint)
	fmt.Printf("model     %s  (%s)\n", picked, method)
	if method == "enumerated" {
		fmt.Printf("  candidates matching the rule: %s\n", strings.Join(cands, ", "))
		fmt.Printf("  of %d models listed\n", listed)
	}
	fmt.Printf("\nrecorded in %s\n", path)
	fmt.Printf("COMMIT IT before running any trace -- the runner refuses an " +
		"uncommitted or modified model freeze.\n")
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
