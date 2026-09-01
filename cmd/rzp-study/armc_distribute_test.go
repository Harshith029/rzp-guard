package main

import (
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

// body is the message minus its first two lines, which are the attachment's
// own name and hash. The file is called audit-armC-e1.csv and the rater will
// see that name on the attachment no matter what this message says, so those
// two lines are identity rather than context and are excluded from the scan.
func body(t *testing.T, msg string) string {
	t.Helper()
	lines := strings.Split(msg, "\n")
	if len(lines) < 3 {
		t.Fatalf("rater message is only %d lines", len(lines))
	}
	if !strings.HasPrefix(lines[0], "Attached:") || !strings.HasPrefix(lines[1], "SHA-256:") {
		t.Fatalf("first two lines are no longer the attachment identity:\n%q\n%q",
			lines[0], lines[1])
	}
	return strings.Join(lines[2:], "\n")
}

func TestTheRaterMessageCarriesNoLinkAndNoStudyContext(t *testing.T) {
	msg := raterMessage("audit-armC-e1.csv",
		"09b3142b5e20c7cf96148b97d2c0c73e99fe08690f5f9caea6752edd5c0587c5")

	got := strings.ToLower(body(t, msg))
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
	msg := raterMessage("audit-armC-e1.csv", "09b3142b5e20c7cf9614")
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
	msg := raterMessage(file, sum)
	for _, want := range []string{file, sum, "label", "reason", "return"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the rater message no longer contains %q", want)
		}
	}
	if !strings.Contains(msg, "Do not look") {
		t.Error("the rater message no longer tells the rater not to look the rows up, " +
			"which is the only thing keeping them blind now that the anchor is gone " +
			"from this message")
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
