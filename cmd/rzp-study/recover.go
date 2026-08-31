package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Void recovery, implementing PROTOCOL-armC-AMENDMENT-2 A2.2.
//
// The runner refuses -only and -runs on a real run, for a good reason: they
// would let a trace be chosen, which is the "re-run until it works" freedom the
// pre-registration exists to remove. A recovery pass needs to re-run a subset,
// so it has to remove that same freedom a different way.
//
// It does so by SELECTING ON RECORDED STATUS AND NOTHING ELSE. The set is every
// trace whose file on disk says `"status": "void"`. A void carries no result, so
// there is nothing in it to select on; and a completed trace is refused at the
// point of writing, whatever the set claims.

// recordedStatus reads the status a trace file already holds. Used defensively
// before an overwrite, so a bug in the set computation cannot destroy data.
func recordedStatus(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var t struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return "", err
	}
	return t.Status, nil
}

// loadVoidSet fills keys of the form "G001_run1" for every void trace in dir.
func loadVoidSet(dir string, into map[string]bool) (int, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range paths {
		st, err := recordedStatus(p)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", p, err)
		}
		if st != "void" {
			continue
		}
		key := strings.TrimSuffix(filepath.Base(p), ".json")
		into[key] = true
		n++
	}
	return n, nil
}

// writeRecoveryLog records the outcome of every attempt, including the ones that
// failed again. A recovery that only reports its successes is a filter.
func writeRecoveryLog(recovered, stillVoid []string) error {
	sort.Strings(recovered)
	sort.Strings(stillVoid)

	var w strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&w, f, v...) }
	p("# Arm C void recovery\n\n")
	p("Executed under `PROTOCOL-armC-AMENDMENT-2.md` A2.2, which was committed\n")
	p("**before** this pass ran. One attempt per void trace, selected by recorded\n")
	p("status and by nothing else. Completed traces were not eligible and are\n")
	p("refused at the point of writing.\n\n")
	p("Run at %s.\n\n", time.Now().UTC().Format(time.RFC3339))

	p("| | n |\n|---|---|\n")
	p("| Attempted | %d |\n", len(recovered)+len(stillVoid))
	p("| Recovered | %d |\n", len(recovered))
	p("| Still void | %d |\n\n", len(stillVoid))

	if len(recovered) > 0 {
		p("## Recovered\n\n")
		for _, k := range recovered {
			p("- `%s`\n", k)
		}
		p("\n")
	}
	if len(stillVoid) > 0 {
		p("## Still void after one attempt\n\n")
		p("These are **retained as exclusions**, per A2.2 step 5. They are not\n")
		p("retried again, and `RESULTS-armC.md` publishes each one's scenario, run\n")
		p("index, exact cell and reason.\n\n")
		for _, k := range stillVoid {
			p("- `%s`\n", k)
		}
		p("\n")
	} else if len(recovered) > 0 {
		p("Every attempted trace recovered. No exclusions arise from this pass.\n\n")
	}
	return os.WriteFile(filepath.Join(studyDir(), "RECOVERY-armC.md"),
		[]byte(w.String()), 0o644)
}
