package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These run from the repository root: loadBriefs and verifyFreeze resolve
// study/ relative to the working directory, and `go test` starts in the package
// directory.
func atRepoRoot(t *testing.T) {
	t.Helper()
	t.Chdir("../..")
}

// A trace set that SHOULD pass, built from the real frozen briefs.
func goodTraceSet(t *testing.T) ([]trace, *manifest, *modelFreeze) {
	t.Helper()
	m, err := verifyFreeze()
	if err != nil {
		t.Fatalf("freeze must be intact for these tests: %v", err)
	}
	briefs, err := loadBriefs("")
	if err != nil {
		t.Fatalf("loadBriefs: %v", err)
	}
	mf := &modelFreeze{SHA256: "modelfreezesha", Commit: "abc123"}

	perBrief := m.DeclaredTraceCount / len(briefs)
	var out []trace
	for _, b := range briefs {
		for run := 1; run <= perBrief; run++ {
			out = append(out, trace{
				BriefID: b.BriefID, RunIndex: run, Status: "complete",
				Model: "gpt-5.6", FreezeSHA: m.FreezeSHA256, ModelFreezeSHA: mf.SHA256,
			})
		}
	}
	return out, m, mf
}

func TestValidateTraceSetAcceptsTheDeclaredSet(t *testing.T) {
	atRepoRoot(t)
	traces, m, mf := goodTraceSet(t)
	if len(traces) != m.DeclaredTraceCount {
		t.Fatalf("built %d traces, declared %d", len(traces), m.DeclaredTraceCount)
	}
	if err := validateTraceSet(traces, "study/traces", m, mf); err != nil {
		t.Fatalf("the complete declared set must be accepted: %v", err)
	}
}

// Each mutation is a way a number with a real-looking shape could be published
// with no experiment behind it. A reviewer demonstrated the first one against
// the previous code: pointing worksheet at the scripted dry run produced a
// worksheet for 27 calls, which is not the registered study and never could be.
func TestValidateTraceSetRefusesEverythingElse(t *testing.T) {
	atRepoRoot(t)

	for _, tc := range []struct {
		name   string
		mutate func([]trace) []trace
		expect string
	}{
		{"scripted dry-run output", func(ts []trace) []trace {
			ts[0].Model = dryRunModel
			return ts
		}, "scripted dry run"},

		{"a smoke trace", func(ts []trace) []trace {
			ts[3].Smoke = true
			return ts
		}, "smoke trace"},

		{"a void trace", func(ts []trace) []trace {
			ts[5].Status, ts[5].VoidReason = "void", "api error"
			return ts
		}, "void"},

		{"a partial set", func(ts []trace) []trace {
			return ts[:len(ts)-1]
		}, "missing"},

		{"an empty directory", func(ts []trace) []trace {
			return nil
		}, "no traces"},

		{"a duplicated trace", func(ts []trace) []trace {
			return append(ts, ts[0])
		}, "appears 2 times"},

		{"a foreign brief", func(ts []trace) []trace {
			ts[2].BriefID = "Z99"
			return ts
		}, "not part of the declared trace set"},

		{"a trace from an older freeze", func(ts []trace) []trace {
			ts[7].FreezeSHA = "0000000000000000"
			return ts
		}, "current freeze is"},

		{"a trace from an older model freeze", func(ts []trace) []trace {
			ts[9].ModelFreezeSHA = "0000000000000000"
			return ts
		}, "model freeze"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			traces, m, mf := goodTraceSet(t)
			traces = tc.mutate(traces)
			err := validateTraceSet(traces, "study/traces", m, mf)
			if err == nil {
				t.Fatal("must be refused; this is how a fake number gets published")
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Fatalf("error should mention %q, got: %v", tc.expect, err)
			}
		})
	}
}

// A published result is immutable. report used to overwrite RESULTS.md in
// place, so a second adjudication pass could replace the first with nothing
// recording that it had.
func TestRefuseOverwriteProtectsPublishedOutput(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "RESULTS.md")
	if err := refuseOverwrite(fresh); err != nil {
		t.Fatalf("a path that does not exist must be allowed: %v", err)
	}
	if err := os.WriteFile(fresh, []byte("published"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := refuseOverwrite(fresh)
	if err == nil {
		t.Fatal("an existing published result must not be silently overwritten")
	}
	if !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("error should explain why: %v", err)
	}
}
