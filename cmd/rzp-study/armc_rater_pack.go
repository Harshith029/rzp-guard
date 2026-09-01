package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The rater packet: the two things a rater receives, and the scan that decides
// whether they may be sent.
//
// WHY THIS EXISTS. The delivery pre-registered in PROTOCOL-armC-AUDIT.md was
// the worksheet CSV plus LABELLING-armC.md. That rubric is the internal
// instrument: it names the component under test, tells the rater which of its
// behaviours not to think about -- which is itself a strong hint about what
// every row has in common -- and carries sections about the design, the
// analysis plan and who else labels. Handing it to someone who is supposed to
// be blind defeats the blinding, in the same way the public commit URL in the
// rater message did.
//
// LABELLING-armC.md is NOT edited or deleted. It stays exactly as written, as
// the internal record of the instrument. RATER-INSTRUCTIONS-armC.md is a
// separate, rater-only document carrying the task, the labels, the definitions
// needed to judge a row, and neutral worked examples. The substitution is
// recorded in PROTOCOL-armC-AUDIT-AMENDMENT-3.md, before any label exists.
//
// The scan below is the enforcement. A document that merely intends to be
// neutral drifts; one that is refused at the gate does not.

// raterInstructionsPath is the ONLY instruction document that may be delivered.
func raterInstructionsPath() string {
	return filepath.Join(studyDir(), "RATER-INSTRUCTIONS-armC.md")
}

// forbiddenContextWords must not appear in anything sent to a rater.
//
// Each one either names the component being evaluated, describes the design, or
// gives a rater something to search for. Matching is case-insensitive and on
// substrings, so "author" also catches "authorization", "authorized" and
// "authority" -- the whole root is out, because a rater who learns the rows are
// about an authority boundary has learned what the rows have in common.
//
// Substring matching is deliberately blunt. A false positive costs one
// rewording; a false negative ships a link to the design.
var forbiddenContextWords = []string{
	"guard",
	"blocking",
	"mandate",
	"authoriz",
	"authoris",
	"author",
	"combining",
	"system under test",
	"study",
	"grid",
	"kappa",
	"amendment",
	"repositor",
	"model",
	"results",
	// Not on the reviewer's list, but the same class of leak: anything a rater
	// could follow, or that names the project or the experiment.
	"http",
	"://",
	"github",
	"protocol",
	"rzp-guard",
	"razorpay",
	"arm c",
	"corpus",
	"precision",
	"recall",
	"false positive",
	"ground truth",
	"blinded",
}

// scanForbiddenContext returns every forbidden word present in text, sorted and
// deduplicated. An empty result means the text carries no study context that
// this scan knows how to name -- which is a weaker statement than "carries no
// context at all", and the caller says so.
func scanForbiddenContext(text string) []string {
	low := strings.ToLower(text)
	seen := map[string]bool{}
	for _, w := range forbiddenContextWords {
		if strings.Contains(low, w) {
			seen[w] = true
		}
	}
	out := make([]string, 0, len(seen))
	for w := range seen {
		out = append(out, w)
	}
	sort.Strings(out)
	return out
}

// loadRaterInstructions reads the rater-only instrument and refuses to return it
// if it carries study context. The refusal is the point: the packet cannot be
// printed at all if the instrument has drifted back toward the internal rubric.
func loadRaterInstructions() (path string, body []byte, err error) {
	path = raterInstructionsPath()
	body, err = os.ReadFile(path)
	if err != nil {
		return path, nil, fmt.Errorf("the rater-only instructions are missing: %w.\n"+
			"LABELLING-armC.md is the INTERNAL rubric and must not be delivered; see "+
			"study/PROTOCOL-armC-AUDIT-AMENDMENT-3.md", err)
	}
	if hits := scanForbiddenContext(string(body)); len(hits) > 0 {
		return path, nil, fmt.Errorf(
			"REFUSING TO DELIVER %s: it contains %d forbidden context word(s): %s.\n"+
				"A rater who reads any of these is no longer blind to what the rows "+
				"have in common, and every label they return afterwards is worth less "+
				"than it looks. Reword the document; do not weaken the scan",
			filepath.ToSlash(path), len(hits), strings.Join(hits, ", "))
	}
	return path, body, nil
}
