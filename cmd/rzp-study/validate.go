package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// validateTraceSet is the single gate between "some JSON files" and "the
// pre-registered study".
//
// It exists because worksheet and report used to accept whatever directory they
// were pointed at. A reviewer demonstrated the consequence: pointing worksheet
// at the scripted dry-run output produced a worksheet for 27 calls across 15
// traces, which is not the registered study and never could be. Anything
// downstream of that -- a confusion matrix, a precision figure -- would have
// been a number with a real-looking shape and no experiment behind it.
//
// Every check fails closed, and each rules out a specific way a wrong number
// gets published:
//
//	scripted        a fake model's output cannot become a result
//	smoke           the integration check is not the study
//	void            a mechanically failed trace is not evidence
//	freeze drift    traces must have run under the CURRENT committed freeze
//	model drift     ... and under the CURRENT committed model choice
//	wrong shape     exactly the declared (brief, run) set: none missing,
//	                none duplicated, none foreign
func validateTraceSet(traces []trace, dir string, m *manifest, mf *modelFreeze) error {
	if len(traces) == 0 {
		return fmt.Errorf("no traces in %s", dir)
	}

	briefs, err := loadBriefs("")
	if err != nil {
		return err
	}
	perBrief := 0
	if len(briefs) > 0 {
		perBrief = m.DeclaredTraceCount / len(briefs)
	}
	if perBrief < 1 || perBrief*len(briefs) != m.DeclaredTraceCount {
		return fmt.Errorf("declared trace count %d is not a whole multiple of %d briefs",
			m.DeclaredTraceCount, len(briefs))
	}

	want := map[string]bool{}
	for _, b := range briefs {
		for run := 1; run <= perBrief; run++ {
			want[fmt.Sprintf("%s/run%d", b.BriefID, run)] = false
		}
	}

	var problems []string
	seen := map[string]int{}

	for _, t := range traces {
		key := fmt.Sprintf("%s/run%d", t.BriefID, t.RunIndex)
		seen[key]++

		switch {
		case t.Model == dryRunModel:
			problems = append(problems, key+": produced by the scripted dry run, not a model")
		case t.Smoke:
			problems = append(problems, key+": a smoke trace, not part of the study")
		}
		if t.Status == "void" {
			problems = append(problems, fmt.Sprintf("%s: void (%s)", key, t.VoidReason))
		}
		if t.FreezeSHA != m.FreezeSHA256 {
			problems = append(problems, fmt.Sprintf(
				"%s: ran under freeze %.12s, current freeze is %.12s",
				key, t.FreezeSHA, m.FreezeSHA256))
		}
		if mf != nil && t.ModelFreezeSHA != mf.SHA256 {
			problems = append(problems, fmt.Sprintf(
				"%s: ran under model freeze %.12s, current is %.12s",
				key, t.ModelFreezeSHA, mf.SHA256))
		}
		if _, expected := want[key]; !expected {
			problems = append(problems, key+": not part of the declared trace set")
		} else {
			want[key] = true
		}
	}

	// Every trace must name the SAME served model.
	//
	// Per-turn equality only proves each response matched its own request. It
	// cannot see an alias being repointed between traces, or a router sending
	// half the study one way and half the other -- and the study runs through an
	// endpoint demonstrated to substitute (PROTOCOL.md 4.5). Since every
	// denominator counts calls the agent actually emitted, two generators means
	// two call distributions blended into one number that describes neither.
	served := map[string][]string{}
	for _, t := range traces {
		if t.ServedModel != "" {
			key := fmt.Sprintf("%s/run%d", t.BriefID, t.RunIndex)
			served[t.ServedModel] = append(served[t.ServedModel], key)
		}
	}
	if len(served) > 1 {
		var names []string
		for name, keys := range served {
			sort.Strings(keys)
			shown := keys
			if len(shown) > 3 {
				shown = append(append([]string{}, shown[:3]...), "…")
			}
			names = append(names, fmt.Sprintf("%s (%d traces: %s)",
				name, len(keys), strings.Join(shown, ", ")))
		}
		sort.Strings(names)
		problems = append(problems, "traces report MORE THAN ONE served model: "+
			strings.Join(names, "; ")+
			" -- two generators means two call distributions blended into one rate")
	}

	for key, n := range seen {
		if n > 1 {
			problems = append(problems, fmt.Sprintf("%s: appears %d times", key, n))
		}
	}
	var missing []string
	for key, got := range want {
		if !got {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		problems = append(problems, fmt.Sprintf("missing %d of %d declared traces: %s",
			len(missing), m.DeclaredTraceCount, strings.Join(missing, ", ")))
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%s is not the pre-registered trace set:\n  %s",
			dir, strings.Join(problems, "\n  "))
	}
	return nil
}

// refuseOverwrite keeps a published result immutable.
//
// report used to overwrite RESULTS.md and the published labels in place, so a
// second adjudication pass could quietly replace the first with no trace that
// it had. Re-publishing has to be a deliberate act -- move the old one aside --
// rather than a side effect of re-running a command.
func refuseOverwrite(paths ...string) error {
	var exists []string
	for _, p := range paths {
		if fileExists(p) {
			exists = append(exists, filepath.ToSlash(p))
		}
	}
	if len(exists) == 0 {
		return nil
	}
	return fmt.Errorf("refusing to overwrite published output: %s\n"+
		"A result is immutable once written. Re-publishing must be deliberate: "+
		"move the existing file aside, and say why in the commit",
		strings.Join(exists, ", "))
}
