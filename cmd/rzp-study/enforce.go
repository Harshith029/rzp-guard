package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The model freeze was a manual promise. PROTOCOL.md 4 says the resolved model
// id is "recorded verbatim and committed BEFORE the first trace runs", but
// nothing enforced any part of that: study/model.frozen.json sat outside
// manifest.json, unhashed, and a run would happily proceed with an uncommitted
// or locally edited file. A control nobody can fail is documentation.
//
// These checks make the promise mechanical. All of them fail closed.

type modelFreeze struct {
	SHA256 string
	Commit string
}

func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// requireCommittedModelFreeze refuses to proceed unless the model choice is
// tracked, committed, and identical to what is on disk.
//
// Requiring git rather than trusting a flag is deliberate: the whole point is
// that the model was fixed in version control before any trace existed, and
// only version control can attest to that.
func requireCommittedModelFreeze(path string) (*modelFreeze, error) {
	rel := filepath.ToSlash(path)

	if _, err := os.Stat(rel); err != nil {
		return nil, fmt.Errorf("model not resolved: %s is missing; run resolve-model first", rel)
	}
	if _, err := git("rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("git is required to verify the model freeze, and is unavailable here")
	}
	if _, err := git("ls-files", "--error-unmatch", rel); err != nil {
		return nil, fmt.Errorf("%s is not tracked by git; the model choice must be "+
			"committed BEFORE any trace runs, or it is not a freeze", rel)
	}
	if out, err := git("status", "--porcelain", "--", rel); err == nil && out != "" {
		return nil, fmt.Errorf("%s has uncommitted changes (%q); commit the model "+
			"freeze before running traces", rel, out)
	}
	// The committed blob must equal the working file. status --porcelain covers
	// this, but comparing bytes directly does not depend on git's index state.
	committed, err := git("show", "HEAD:"+rel)
	if err != nil {
		return nil, fmt.Errorf("%s is not present in HEAD; commit it before running traces", rel)
	}
	onDisk, err := os.ReadFile(rel)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(committed) != strings.TrimSpace(string(onDisk)) {
		return nil, fmt.Errorf("%s on disk differs from the committed version", rel)
	}

	sum := sha256.Sum256(onDisk)
	commit, _ := git("rev-parse", "HEAD")
	return &modelFreeze{SHA256: hex.EncodeToString(sum[:]), Commit: commit}, nil
}

// requireEmptyTraceDir keeps traces immutable. A study that silently overwrites
// its own output can be re-run until it produces a nicer number, which is
// exactly the freedom the pre-registration exists to remove.
func requireEmptyTraceDir(dir string) error {
	entries, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("%s already holds %d trace(s). Traces are immutable: "+
			"move or delete the directory deliberately, and say so, rather than "+
			"overwriting a recorded run", dir, len(entries))
	}
	return nil
}

// requireFullTraceSet enforces the pre-declared design. A real run is all 45
// traces or it is not the pre-registered study.
func requireFullTraceSet(briefs, perBrief, declared int, only string, runsOverride int) error {
	if only != "" {
		return fmt.Errorf("-only is a dry-run debugging flag; a real run must be the " +
			"complete pre-declared trace set")
	}
	if runsOverride != 0 {
		return fmt.Errorf("-runs is a dry-run debugging flag; runs per brief is frozen " +
			"at 3 by PROTOCOL.md 4")
	}
	if got := briefs * perBrief; got != declared {
		return fmt.Errorf("trace set is %d (%d briefs x %d runs) but the freeze declares %d",
			got, briefs, perBrief, declared)
	}
	return nil
}

// refuseStudyPath keeps non-study output out of the study directory.
//
// A smoke trace is real model output but is NOT the pre-registered experiment.
// Letting one land in study/traces/ would put a trace with no committed model
// freeze behind it among the 45 that do have one.
func refuseStudyPath(dir string) error {
	clean := filepath.ToSlash(filepath.Clean(dir))
	if clean == studyDir() || strings.HasPrefix(clean, studyDir()+"/") {
		return fmt.Errorf("refusing to write smoke output to %s: %s/ is where real "+
			"study artifacts live. Send it elsewhere, e.g. -out .gotmp/smoke", dir, studyDir())
	}
	return nil
}
