package main

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Pinning the canonical worksheet to what was actually distributed.
//
// Field-by-field verification compares a returned file to the canonical CSV on
// disk. That is only meaningful if the canonical CSV is still the file the rater
// received. Nothing stopped the author editing it after distribution and making
// a returned file verify cleanly against the altered copy -- the verification
// would pass and prove nothing.
//
// So the canonical file is checked against its PRE-DISTRIBUTION hash, recorded
// in SHA256SUMS-audit-armC.txt at emission time. And because that sums file is
// itself editable, it must be COMMITTED AND UNMODIFIED: git history is what
// makes the recorded hash something the author cannot silently revise. The
// commit carrying it is published in the report, so a reader can check the hash
// against a revision rather than against a working tree.

type canonicalPin struct {
	File   string // basename of the canonical CSV
	SHA256 string // hash recorded before distribution
	Commit string // commit that carries the sums file
	Sums   string // path of the sums file
}

func sha256File(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

func parseSums(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			continue
		}
		out[parts[1]] = parts[0]
	}
	return out, sc.Err()
}

// verifyCanonicalPin fails closed unless the canonical CSV still hashes to the
// value recorded before distribution, in a sums file that is committed and
// unmodified.
func verifyCanonicalPin(canonicalPath, sumsPath string) (canonicalPin, error) {
	pin := canonicalPin{File: filepath.Base(canonicalPath), Sums: filepath.Base(sumsPath)}

	rel := filepath.ToSlash(sumsPath)
	if _, err := git("rev-parse", "--git-dir"); err != nil {
		return pin, fmt.Errorf("git is required to pin the distributed worksheet, " +
			"and is unavailable here")
	}
	if _, err := git("ls-files", "--error-unmatch", rel); err != nil {
		return pin, fmt.Errorf("%s is not tracked by git. The recorded hashes must "+
			"be committed before distribution, or they are not evidence of what "+
			"was sent", rel)
	}
	if out, err := git("status", "--porcelain", "--", rel); err == nil && strings.TrimSpace(out) != "" {
		return pin, fmt.Errorf("%s has uncommitted changes (%q). A recorded hash "+
			"that can be edited alongside the file it pins is not a control", rel, out)
	}
	// Compare the working copy with the committed blob by OBJECT ID, not text.
	//
	// The obvious version -- `git show HEAD:path` against the file's bytes --
	// cannot work here: the shared git() helper TrimSpace's its output, so the
	// committed copy always arrives without its trailing newline and every
	// comparison fails. That produced a control which refused everything,
	// including a file byte-identical to HEAD. A check that always fires is as
	// useless as one that never does, and it invites being "fixed" by loosening
	// it.
	//
	// Blob ids sidestep the question and are what git itself compares.
	committedOID, err := git("rev-parse", "HEAD:"+rel)
	if err != nil {
		return pin, fmt.Errorf("%s is not present in HEAD: %w", rel, err)
	}
	diskOID, err := git("hash-object", rel)
	if err != nil {
		return pin, fmt.Errorf("hashing %s: %w", rel, err)
	}
	if strings.TrimSpace(committedOID) != strings.TrimSpace(diskOID) {
		return pin, fmt.Errorf("%s differs from the committed copy (HEAD %s, disk %s)",
			rel, committedOID, diskOID)
	}

	sums, err := parseSums(sumsPath)
	if err != nil {
		return pin, err
	}
	want, ok := sums[pin.File]
	if !ok {
		return pin, fmt.Errorf("%s records no hash for %s; the worksheet was not "+
			"pinned at emission time", rel, pin.File)
	}
	got, err := sha256File(canonicalPath)
	if err != nil {
		return pin, err
	}
	if got != want {
		return pin, fmt.Errorf("%s DOES NOT MATCH the hash recorded before "+
			"distribution.\n  recorded %s\n  on disk  %s\nThe canonical worksheet has "+
			"changed since it was sent, so verifying a returned file against it "+
			"would prove nothing", pin.File, want, got)
	}
	pin.SHA256 = got

	if c, err := git("log", "-1", "--format=%H", "--", rel); err == nil {
		pin.Commit = strings.TrimSpace(c)
	}
	return pin, nil
}

// ---------------------------------------------------------------- markdown --

// mdCode renders untrusted text as an inline code span.
//
// A rater's `reason` is free text that reaches a published document. Rejecting
// formula prefixes and control characters stops a spreadsheet executing it and
// stops it corrupting the file, but neither prevents `## heading`, `**bold**`,
// a table pipe, or a line that reads like a conclusion from changing how the
// audit renders. A code span with a fence longer than any backtick run inside
// it cannot be escaped by its own content.
func mdCode(s string) string {
	if s == "" {
		return "`(no reason given)`"
	}
	// Longest run of backticks in the text.
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	pad := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		pad = " "
	}
	// A pipe inside a code span still splits a Markdown table cell.
	s = strings.ReplaceAll(s, "|", "\\|")
	return fence + pad + s + pad + fence
}
