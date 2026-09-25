package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// labels_sha256 is the published record of the ground truth, so it has to be a
// property of the labels and nothing else.
//
// THIS TEST EXISTS BECAUSE IT WASN'T. The digest hashed raw bytes, and the
// value published for labels-armE-r2.csv was the hash of a Windows working copy
// with CRLF endings. The committed blob is LF, so anyone who cloned the
// repository recomputed a different digest for a file whose content had never
// changed -- and nothing noticed, because verify never read the field.
func TestALabelDigestIgnoresLineEndingsButNotContent(t *testing.T) {
	lf := "request_id,label\nE001,in-intent\nE002,out-of-intent\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")

	if textSHA256([]byte(lf)) != textSHA256([]byte(crlf)) {
		t.Fatal("an LF and a CRLF checkout of the same labels hash differently; " +
			"the published digest would depend on the reader's git config")
	}

	// The negative control. Normalising must not make the digest blind: one
	// flipped label is a different ground truth and has to read as one.
	flipped := strings.Replace(lf, "E002,out-of-intent", "E002,in-intent", 1)
	if textSHA256([]byte(lf)) == textSHA256([]byte(flipped)) {
		t.Fatal("a changed label produced the same digest; the check is blind")
	}
}

// The published digests must reproduce from the files actually in the tree.
// This is the property that failed: it held on one Windows machine and nowhere
// else.
//
// It does not skip when the manifest is missing. A skipped integrity check
// reads as a pass in a CI summary, which is the same failure this test exists
// to prevent: a check that looks like it is guarding something and is not.
func TestThePublishedLabelDigestsReproduce(t *testing.T) {
	dir := filepath.Join("..", "..", armEDir)
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("the arm E manifest is missing, so the published result has "+
			"nothing protecting it: %v", err)
	}
	var man struct {
		Labels map[string]string `json:"labels_sha256"`
	}
	if err := json.Unmarshal(b, &man); err != nil {
		t.Fatal(err)
	}
	if len(man.Labels) == 0 {
		t.Fatal("the manifest records no label digests, so the ground truth is " +
			"unprotected")
	}
	for f, want := range man.Labels {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if got := textSHA256(raw); got != want {
			t.Errorf("%s: recomputes to %.16s, manifest records %.16s", f, got, want)
		}
	}
}
