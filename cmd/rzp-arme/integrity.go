package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Integrity digests for arm E.
//
// WHY THIS EXISTS.
//
// `verify` used to recompute only the final confusion matrix. That is a weak
// gate: the matrix is four integers, and a change that moved one row from TP to
// TN while moving another the opposite way would reproduce it exactly. It also
// says nothing about WHICH inputs produced those numbers, so a swapped label
// file or an edited mandate could pass.
//
// It missed something real. `internal/policy` changed after the corpus existed
// (PROTOCOL-armE-AMENDMENT-2.md) and nothing here noticed, because the matrix
// happened not to move. It did not move -- every decision was verified identical
// afterwards -- but "we checked and it held" and "we could not have told" are
// different states, and only one of them is a control.
//
// So two digests are recorded at scoring time and checked on every verify:
//
//	decisions  the guard's answer for EVERY row, rule string included
//	inputs     the files those answers were computed from
//
// A change that alters any decision, any rule, any label, any mandate or the
// policy itself now fails, and names which of the two moved.

// decisionsDigest hashes the guard's decision for every row in the corpus.
//
// The rule string is included deliberately. A row that flips from one refusal
// reason to another is still a change in what the guard did, even though the
// allow/refuse bit and therefore the matrix are unchanged.
func decisionsDigest(reqs []request) (string, error) {
	lines := make([]string, 0, len(reqs))
	for _, r := range reqs {
		allowed, rule, err := decide(r)
		if err != nil {
			return "", err
		}
		lines = append(lines, fmt.Sprintf("%s,%t,%s", r.RequestID, allowed, rule))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return hex.EncodeToString(sum[:]), nil
}

// inputsDigest hashes every file the decisions and labels are computed from.
//
// Paths are relative and sorted so the digest is stable across machines, and the
// path is hashed alongside the content so moving a file is a change too.
func inputsDigest() (string, error) {
	var paths []string
	for _, p := range []string{
		filepath.Join(armEDir, "requests.json"),
		filepath.Join(armEDir, "rowmap.json"),
		worksheetPath(),
	} {
		paths = append(paths, p)
	}
	for _, dir := range []string{
		filepath.Join(armEDir, "mandates"),
		"internal/policy",
	} {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return "", fmt.Errorf("integrity: %s: %w", dir, err)
		}
		for _, e := range ents {
			if e.IsDir() || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	// Rater label files, whatever they are called.
	ents, err := os.ReadDir(armEDir)
	if err != nil {
		return "", err
	}
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "labels-armE-") {
			paths = append(paths, filepath.Join(armEDir, e.Name()))
		}
	}

	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("integrity: %s: %w", p, err)
		}
		// Normalise the separator so a Windows run and a Linux run agree.
		fmt.Fprintf(h, "%s\n", filepath.ToSlash(p))
		h.Write(b)
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
