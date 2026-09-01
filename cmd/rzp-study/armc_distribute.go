package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Pre-distribution gate for the blocked-call audit.
//
// WHAT THE LOCAL HASH CHECK IS, AND IS NOT.
//
// verifyCanonicalPin confirms that a canonical worksheet still matches the
// SHA-256 recorded when it was emitted, in a sums file that is committed and
// unmodified. That is a **local workflow-integrity control**. It catches an
// uncommitted edit, an accidental regeneration, or a stale working tree.
//
// It is NOT immutable pre-distribution evidence, and describing it that way was
// wrong. This repository can have its history rewritten -- it has been, in this
// very project, to purge a refund launcher -- and a rewrite can carry the
// canonical CSV and the sums file together. Local `HEAD` attests to nothing an
// author could not restage. A control that only an honest author cannot bypass
// is a workflow aid, not evidence.
//
// The external anchors are:
//
//	1. a PUBLIC commit, pushed before distribution, that a third party can fetch
//	   and hash independently;
//	2. the hash sent to each rater in the distribution message, which the rater
//	   holds and the author cannot retroactively change.
//
// Neither exists until someone does them, so this command checks for the first
// and prints the second, and refuses to pretend the audit is anchored when it
// is not.
//
// WHY THE OUTPUT IS SPLIT IN TWO.
//
// The rater message used to include the public commit URL, so that a rater
// could verify the attachment's hash against the published commit. That handed
// a supposedly blind rater a link to the repository -- and therefore to the
// study design, the labelling rule, the protocol, and the guard's own decisions
// on the very rows they were about to label. It defeated the blinding it was
// printed alongside.
//
// So there are now two artifacts and they never mix:
//
//	raterMessage      the file name, its SHA-256, how to edit and return it, and
//	                  an instruction not to go looking. No URL, no repository
//	                  name, no commit, no protocol, no study context.
//	reviewerRecord    the public anchor and the hashes, for the reviewer and the
//	                  release record. Never sent to a rater.
//
// THE TRADE THIS MAKES. A rater can no longer independently verify their
// attachment against a published commit; they hold the hash and nothing else.
// That is the cost of keeping them blind, and it is the right way round: the
// anchor exists so a THIRD PARTY can check the author did not swap the
// worksheets, and a third party reads the reviewer record, not the rater
// message.

type distributionRecord struct {
	Note       string            `json:"note"`
	RepoURL    string            `json:"public_repo_url"`
	CommitSHA  string            `json:"public_commit_sha"`
	CommitURL  string            `json:"public_commit_url"`
	DistFiles  map[string]string `json:"distributed_file_sha256"`
	RecordedAt string            `json:"recorded_at_utc"`
}

type deliverable struct {
	rater string
	file  string
	pin   canonicalPin
}

func distributionRecordPath() string {
	return filepath.Join(studyDir(), "adjudication", "DISTRIBUTION-armC.json")
}

func loadDistributionRecord() (*distributionRecord, error) {
	b, err := os.ReadFile(distributionRecordPath())
	if err != nil {
		return nil, err
	}
	var d distributionRecord
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// externalAnchorState reports what this CHECKOUT is configured with. It proves
// nothing about the public host: only fetching the commit from that host would,
// and this command does not make network calls.
func externalAnchorState() (remotes, upstream string) {
	r, _ := git("remote", "-v")
	u, err := git("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil {
		u = ""
	}
	return strings.TrimSpace(r), strings.TrimSpace(u)
}

// raterMessage is the ONLY text that may be sent to a rater.
//
// Everything in it is either the attachment's identity or an instruction. It
// names no repository, carries no URL, no commit, no protocol reference, and no
// description of what the rows are or why they were selected -- because a rater
// who knows the study design is no longer blind to it, and the audit's whole
// value is that they did not know.
//
// armc_distribute_test.go asserts these absences on the real output rather than
// trusting this comment.
func raterMessage(fileName, sha256 string) string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }
	p("Attached: %s", fileName)
	p("SHA-256:  %s", sha256)
	p("")
	p("Please label every row and return the file with ONLY the `label` and")
	p("`reason` columns edited. Do not add, remove, reorder or rename columns,")
	p("and do not change any other cell -- the returned file is checked field by")
	p("field against the copy that was sent and will be rejected if anything")
	p("else differs.")
	p("")
	p("Please judge each row only on what the row itself contains. Do not look")
	p("anything up about these rows, where they came from, or what produced")
	p("them, and do not ask anyone about them, until after you have returned the")
	p("file. If you already know where they came from, please say so instead of")
	p("labelling.")
	p("")
	p("Please do not discuss the rows or your labels with anyone until you have")
	p("returned the file.")
	p("")
	p("Please keep this message. The SHA-256 above is how the file you received")
	p("is identified afterwards, and your copy of it is the one record of that")
	p("which is not held by the person who sent it.")
	return b.String()
}

// reviewerRecord carries the public anchor. It is for a reviewer, a judge, or
// the release record, and MUST NOT be sent to a rater.
func reviewerRecord(rec *distributionRecord, out []deliverable) string {
	var b strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&b, f+"\n", v...) }
	p("Distributed worksheets for the arm C blocked-call audit.")
	p("")
	for _, d := range out {
		p("  %-22s sha256 %s", d.pin.File, d.pin.SHA256)
	}
	p("")
	p("Published before distribution at:")
	p("  repository: %s", rec.RepoURL)
	p("  commit:     %s", rec.CommitSHA)
	p("  url:        %s", rec.CommitURL)
	p("")
	p("Fetch that commit and hash the files to confirm the worksheets were fixed")
	p("before anyone labelled them. That check is the point of the anchor, and it")
	p("is why the anchor is here and not in the message the raters received: a")
	p("rater who follows the link is no longer blind to the study.")
	return b.String()
}

func cmdArmCPreDistribute(args []string) error {
	fs := flag.NewFlagSet("predistribute-armC", flag.ExitOnError)
	force := fs.Bool("acknowledge-no-anchor", false,
		"proceed and print the messages even though no public anchor exists. The "+
			"printed text will say the pin is local-only.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := applyArmDirs("C", false); err != nil {
		return err
	}

	sumsPath := filepath.Join(studyDir(), "adjudication", "SHA256SUMS-audit-armC.txt")
	var out []deliverable
	for _, r := range []string{"e1", "e2"} {
		canonical := filepath.Join(studyDir(), "adjudication",
			fmt.Sprintf("audit-armC-%s.csv", r))
		pin, err := verifyCanonicalPin(canonical, sumsPath)
		if err != nil {
			return fmt.Errorf("local workflow check failed: %w", err)
		}
		out = append(out, deliverable{rater: r, file: canonical, pin: pin})
	}

	head, _ := git("rev-parse", "HEAD")
	head = strings.TrimSpace(head)
	remotes, upstream := externalAnchorState()
	rec, recErr := loadDistributionRecord()

	anchored := recErr == nil && rec != nil &&
		strings.TrimSpace(rec.CommitSHA) != "" && strings.TrimSpace(rec.RepoURL) != ""

	fmt.Println("=== arm C blocked-call audit: pre-distribution check ===")
	fmt.Println()
	fmt.Println("LOCAL workflow-integrity control (passed):")
	for _, d := range out {
		fmt.Printf("  %-22s sha256 %s\n", d.pin.File, d.pin.SHA256)
	}
	fmt.Printf("  sums file committed and unmodified, last changed in %s\n",
		shortSHA(out[0].pin.Commit))
	fmt.Println()
	fmt.Println("  This proves the worksheets have not changed since emission IN THIS")
	fmt.Println("  WORKING TREE. It is not external evidence: local history can be")
	fmt.Println("  rewritten carrying the CSV and the sums file together.")
	fmt.Println()

	fmt.Println("EXTERNAL anchor:")
	if remotes == "" {
		fmt.Println("  git remotes:      none configured in this checkout")
	} else {
		fmt.Printf("  git remotes:      %s\n", strings.Split(remotes, "\n")[0])
	}
	fmt.Print(upstreamLine(upstream))
	if anchored {
		fmt.Printf("  public commit:    %s\n", rec.CommitSHA)
		fmt.Printf("  public URL:       %s\n", rec.CommitURL)
	} else {
		fmt.Println("  public commit:    NOT RECORDED")
	}
	fmt.Println()
	fmt.Println("  This command makes no network calls. It does not fetch the recorded")
	fmt.Println("  commit, does not confirm it exists on the public host, and does not")
	fmt.Println("  confirm the published files hash to the values above. Everything in")
	fmt.Println("  this section is read from local configuration and a local JSON file.")
	fmt.Println("  Verify the anchor independently before relying on it.")
	fmt.Println()

	if !anchored {
		msg := "DO NOT DISTRIBUTE YET.\n\n" +
			"No public anchor exists, so \"recorded before distribution\" would be a\n" +
			"self-authored local claim. Before sending:\n\n" +
			"  1. Push the commit containing the canonical worksheets and\n" +
			"     SHA256SUMS-audit-armC.txt to the public repository that will be\n" +
			"     submitted.\n" +
			"  2. Record the repository URL, the full commit SHA and its URL in\n" +
			"     " + filepath.ToSlash(distributionRecordPath()) + "\n" +
			"  3. Re-run this command. It will then print the reviewer record with\n" +
			"     the public commit included.\n\n" +
			"Current local HEAD is " + head + ", which is not yet evidence of anything\n" +
			"outside this machine."
		if !*force {
			return fmt.Errorf("%s", msg)
		}
		fmt.Println(msg)
		fmt.Println()
		fmt.Println("--- proceeding under -acknowledge-no-anchor ---")
		fmt.Println()
	}

	for _, d := range out {
		fmt.Printf("=== BEGIN RATER MESSAGE (%s) -- send exactly this, and nothing else ===\n\n",
			d.rater)
		fmt.Print(raterMessage(d.pin.File, d.pin.SHA256))
		fmt.Printf("\n=== END RATER MESSAGE (%s) ===\n\n", d.rater)
	}

	fmt.Println("=== BEGIN REVIEWER / RELEASE RECORD -- NOT for raters ===")
	fmt.Println()
	if anchored {
		fmt.Print(reviewerRecord(rec, out))
	} else {
		fmt.Println("No public anchor is recorded, so there is no reviewer record to")
		fmt.Println("print. The worksheets above are pinned only in this working tree.")
	}
	fmt.Println()
	fmt.Println("=== END REVIEWER / RELEASE RECORD ===")
	return nil
}

// upstreamLine states exactly what an absent upstream means and no more.
//
// The old wording was "NONE (nothing has been pushed)", which is a claim this
// command cannot make. Absent upstream configuration says only that THIS
// checkout has no tracking branch set. The commit may have been pushed from
// another clone, pushed by explicit refspec, or pushed and the configuration
// removed afterwards. Reading "not configured here" as "never published"
// inverts the safe direction: it would report an anchored audit as unanchored,
// and an author who knew better would learn to ignore the warning.
func upstreamLine(upstream string) string {
	if strings.TrimSpace(upstream) == "" {
		return "  upstream branch:  none configured for this checkout\n" +
			"                    (this says only that no tracking branch is set here;\n" +
			"                     it is not evidence about whether anything was pushed)\n"
	}
	return fmt.Sprintf("  upstream branch:  %s\n", upstream)
}

func shortSHA(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "(unknown)"
	}
	return s
}
