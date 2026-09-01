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

type distributionRecord struct {
	Note       string            `json:"note"`
	RepoURL    string            `json:"public_repo_url"`
	CommitSHA  string            `json:"public_commit_sha"`
	CommitURL  string            `json:"public_commit_url"`
	DistFiles  map[string]string `json:"distributed_file_sha256"`
	RecordedAt string            `json:"recorded_at_utc"`
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

// externalAnchorState reports whether a public anchor plausibly exists. It
// deliberately does not try to prove one: only fetching the commit from the
// public host would do that, and this command cannot.
func externalAnchorState() (remotes, upstream string) {
	r, _ := git("remote", "-v")
	u, err := git("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil {
		u = ""
	}
	return strings.TrimSpace(r), strings.TrimSpace(u)
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
	type deliverable struct {
		rater string
		file  string
		pin   canonicalPin
	}
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
		fmt.Println("  git remotes:      NONE")
	} else {
		fmt.Printf("  git remotes:      %s\n", strings.Split(remotes, "\n")[0])
	}
	if upstream == "" {
		fmt.Println("  upstream branch:  NONE (nothing has been pushed)")
	} else {
		fmt.Printf("  upstream branch:  %s\n", upstream)
	}
	if anchored {
		fmt.Printf("  public commit:    %s\n", rec.CommitSHA)
		fmt.Printf("  public URL:       %s\n", rec.CommitURL)
	} else {
		fmt.Println("  public commit:    NOT RECORDED")
	}
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
			"  3. Re-run this command. It will then print the rater messages with\n" +
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
		fmt.Printf("=== message for rater %s ===\n\n", d.rater)
		fmt.Printf("Attached: %s\n", d.pin.File)
		fmt.Printf("SHA-256:  %s\n\n", d.pin.SHA256)
		fmt.Println("Please label every row and return the file with ONLY the `label`")
		fmt.Println("and `reason` columns edited. Do not add, remove, reorder or rename")
		fmt.Println("columns, and do not change any other cell -- the returned file is")
		fmt.Println("checked field by field against the copy above and will be rejected")
		fmt.Println("if anything else differs.")
		fmt.Println()
		fmt.Println("Please do not discuss the cases with the other rater.")
		fmt.Println()
		if anchored {
			fmt.Printf("This exact file is published at:\n  %s\n", rec.CommitURL)
			fmt.Println("You can verify the attachment's hash against that commit.")
		} else {
			fmt.Println("NOTE: this file is not yet published anywhere public, so the")
			fmt.Println("hash above is the author's own record. Keep your copy of this")
			fmt.Println("message: it is the only independent record of what you received.")
		}
		fmt.Println()
	}
	return nil
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
