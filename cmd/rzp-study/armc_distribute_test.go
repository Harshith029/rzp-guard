package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE DEFECT THIS GUARDS. The rater message used to end with the public commit
// URL so the rater could verify their attachment's hash. That handed a blind
// rater a link to the repository, and with it the study design, the labelling
// rule, the protocol and the guard's own decisions on the rows they were about
// to label. The blinding and the thing that defeated it were printed in the
// same paragraph.
//
// A comment saying "no context here" is worth nothing, because the next edit
// will not read it. These assertions run on the real output.

// forbiddenInRaterMessage is scanned case-insensitively. Each entry is
// something a rater could follow, search, or recognise as the study.
var forbiddenInRaterMessage = []string{
	"http", "://", "github", ".com", "www.", "git",
	"repo", "commit", "branch", "url", "link",
	"protocol", "readme", "study", "audit", "arm c",
	"guard", "mandate", "refund", "razorpay", "payment",
	"precision", "recall", "false positive", "blocked", "policy",
	"in-intent", "out-of-intent",
}

// body is the message with the two file names and their hashes removed. A rater
// sees those names on the attachments whatever the message says, so they are
// identity rather than context. Everything that remains has to be clean.
//
// Removal is by value rather than by line number, so the scan does not quietly
// stop covering the message the next time its layout changes.
func body(t *testing.T, msg, fileName, sha, instrName, instrSHA string) string {
	t.Helper()
	for _, id := range []string{fileName, sha, instrName, instrSHA} {
		if !strings.Contains(msg, id) {
			t.Fatalf("the message no longer identifies %q, so it cannot be sent", id)
		}
	}
	return strings.NewReplacer(
		fileName, "", sha, "", instrName, "", instrSHA, "").Replace(msg)
}

func TestTheRaterMessageCarriesNoLinkAndNoStudyContext(t *testing.T) {
	const file = "audit-armC-e1.csv"
	const sum = "09b3142b5e20c7cf96148b97d2c0c73e99fe08690f5f9caea6752edd5c0587c5"
	const instr, isum = "RATER-INSTRUCTIONS-armC.md", "aaaabbbbccccdddd"
	msg := raterMessage(file, sum, instr, isum)

	got := strings.ToLower(body(t, msg, file, sum, instr, isum))
	for _, bad := range forbiddenInRaterMessage {
		if strings.Contains(got, bad) {
			t.Errorf("the rater message contains %q. A rater who can follow or "+
				"search for it is no longer blind to the study, and every label "+
				"they return afterwards is worth less than it looks.", bad)
		}
	}
}

// The real record's values, so this fails on the exact regression rather than
// on a lookalike: no repository URL, no commit SHA, no commit URL, whatever
// the message is later edited to say.
func TestTheRaterMessageLeaksNothingFromTheDistributionRecord(t *testing.T) {
	rec := &distributionRecord{
		RepoURL:   "https://github.com/Harshith029/rzp-guard",
		CommitSHA: "65f97b0352e79611c48f99577a57a338d98a7ba9",
		CommitURL: "https://github.com/Harshith029/rzp-guard/commit/" +
			"65f97b0352e79611c48f99577a57a338d98a7ba9",
	}
	msg := raterMessage("audit-armC-e1.csv", "09b3142b5e20c7cf9614",
		"RATER-INSTRUCTIONS-armC.md", "aaaabbbbccccdddd")
	for _, secret := range []string{rec.RepoURL, rec.CommitSHA, rec.CommitURL} {
		if strings.Contains(msg, secret) {
			t.Errorf("the rater message contains %q from the distribution record", secret)
		}
	}
}

// The rater message must still do its job: name the attachment, pin it, and say
// how to return it. Stripping the context is only correct if what remains is
// enough to label and return the file.
func TestTheRaterMessageStillCarriesTheFileItsHashAndTheInstructions(t *testing.T) {
	const file, sum = "audit-armC-e1.csv", "09b3142b5e20c7cf9614"
	const instr, isum = "RATER-INSTRUCTIONS-armC.md", "aaaabbbbccccdddd"
	msg := raterMessage(file, sum, instr, isum)
	for _, want := range []string{file, sum, instr, isum, "label", "reason", "return"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the rater message no longer contains %q", want)
		}
	}
	// The instrument the rater must NOT be given.
	if strings.Contains(msg, "LABELLING-armC.md") {
		t.Error("the rater message names LABELLING-armC.md, which is the internal " +
			"rubric: it names the component under test, tells the rater which of its " +
			"behaviours to disregard, and carries the design and analysis sections")
	}
	if !strings.Contains(msg,
		"Do not browse or search this project until after returning labels") {
		t.Error("the rater message no longer carries the do-not-browse instruction, " +
			"which is the only thing keeping the rater blind now that the anchor is " +
			"gone from this message")
	}
}

// The scan is only worth anything if it refuses something. Fed the internal
// rubric -- the document that WAS pre-registered for delivery -- it must refuse,
// and fed the rater-only instrument it must pass. If both passed, the scan would
// be decoration.
func TestTheContextScanSeparatesTheTwoInstruments(t *testing.T) {
	rater, err := os.ReadFile(filepath.Join("..", "..", "study", "RATER-INSTRUCTIONS-armC.md"))
	if err != nil {
		t.Fatal(err)
	}
	if hits := scanForbiddenContext(string(rater)); len(hits) > 0 {
		t.Errorf("the delivered rater instrument contains forbidden context: %v", hits)
	}

	internal, err := os.ReadFile(filepath.Join("..", "..", "study", "LABELLING-armC.md"))
	if err != nil {
		t.Fatal(err)
	}
	hits := scanForbiddenContext(string(internal))
	if len(hits) == 0 {
		t.Fatal("the internal rubric passes the context scan, so the scan cannot be " +
			"distinguishing the two instruments and proves nothing about either")
	}
	t.Logf("internal rubric refused on %d word(s): %v", len(hits), hits)
}

// Everything sent to a rater goes through the same scan, not just the document.
func TestTheRaterMessageItselfPassesTheContextScan(t *testing.T) {
	msg := raterMessage("audit-armC-e1.csv", "09b3142b5e20c7cf9614",
		"RATER-INSTRUCTIONS-armC.md", "aaaabbbbccccdddd")
	if hits := scanForbiddenContext(msg); len(hits) > 0 {
		t.Errorf("the rater message contains forbidden context: %v", hits)
	}
}

// And the reviewer record must NOT pass it -- it carries the anchor by design.
// This is what stops the two outputs being quietly merged back together.
func TestTheReviewerRecordIsNotRaterSafe(t *testing.T) {
	rec := &distributionRecord{
		RepoURL:   "https://github.com/Harshith029/rzp-guard",
		CommitSHA: "65f97b0352e79611c48f99577a57a338d98a7ba9",
		CommitURL: "https://github.com/Harshith029/rzp-guard/commit/x",
	}
	out := []deliverable{{rater: "e1", pin: canonicalPin{
		File: "audit-armC-e1.csv", SHA256: "09b3142b5e20c7cf9614"}}}
	if hits := scanForbiddenContext(reviewerRecord(rec, out)); len(hits) == 0 {
		t.Fatal("the reviewer record passes the rater context scan. It carries the " +
			"repository URL and commit on purpose, so if it passes, the scan is not " +
			"catching the exact class of leak it exists for")
	}
}

// The split is only correct if the anchor still reaches SOMEONE. Removing it
// from the rater message and nowhere else would leave the audit unanchored.
func TestTheReviewerRecordStillCarriesTheAnchor(t *testing.T) {
	rec := &distributionRecord{
		RepoURL:   "https://github.com/Harshith029/rzp-guard",
		CommitSHA: "65f97b0352e79611c48f99577a57a338d98a7ba9",
		CommitURL: "https://github.com/Harshith029/rzp-guard/commit/" +
			"65f97b0352e79611c48f99577a57a338d98a7ba9",
	}
	out := []deliverable{{rater: "e1", pin: canonicalPin{
		File: "audit-armC-e1.csv", SHA256: "09b3142b5e20c7cf9614"}}}
	got := reviewerRecord(rec, out)
	for _, want := range []string{rec.RepoURL, rec.CommitSHA, rec.CommitURL,
		"audit-armC-e1.csv", "09b3142b5e20c7cf9614"} {
		if !strings.Contains(got, want) {
			t.Errorf("the reviewer record no longer contains %q, so the anchor "+
				"reaches nobody", want)
		}
	}
}

// "No upstream configured here" is not "nothing has been pushed". The old
// wording asserted the second, which this command cannot know: the commit may
// have been pushed from another clone, pushed by explicit refspec, or pushed
// and the tracking configuration removed afterwards.
func TestTheUpstreamLineClaimsOnlyWhatItCanKnow(t *testing.T) {
	got := upstreamLine("")
	if strings.Contains(got, "nothing has been pushed") {
		t.Error("the upstream line still claims nothing has been pushed, which " +
			"absent tracking configuration does not establish")
	}
	if !strings.Contains(got, "none configured for this checkout") {
		t.Errorf("the upstream line no longer states its narrow meaning:\n%s", got)
	}
	if !strings.Contains(got, "not evidence about whether anything was pushed") {
		t.Errorf("the upstream line no longer disclaims the inference:\n%s", got)
	}
	if got := upstreamLine("origin/master"); !strings.Contains(got, "origin/master") {
		t.Errorf("a configured upstream is no longer reported: %q", got)
	}
}
