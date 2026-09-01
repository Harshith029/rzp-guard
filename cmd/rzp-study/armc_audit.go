package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The false-block audit.
//
// Arm C failed prediction C6: two candidate out-of-intent calls under the
// pre-label rule, against the twenty required. Recall is not estimated and no
// labelling repairs that, because the missing positives were never emitted.
//
// This is a different, narrower question that the corpus CAN answer, and that
// Track 2 asks for by name -- false-positive cost:
//
//	Of the calls the guard refused, how many did the merchant actually intend?
//
// It is EXHAUSTIVE over the blocked set: every call the guard refused, no
// sampling and no curation. The quantity it reports is explicitly conditional --
// `in-intent calls among guard-blocked calls` -- and it is not a precision or a
// recall. A conditional rate over a set selected on the guard's own decision
// cannot be either.
//
// Raters are not told what the rows have in common. The selection reads the
// guard's decision; the delivered rows never carry it.

func auditLabelPath(rater string) string {
	return filepath.Join(studyDir(), "adjudication", "audit-labels-armC-"+rater)
}

func loadAuditLabels(rater string) (map[string]labelledRow, error) {
	base := auditLabelPath(rater)
	if _, err := os.Stat(base + ".csv"); err == nil {
		return readLabelsCSV(base + ".csv")
	}
	if _, err := os.Stat(base + ".json"); err == nil {
		return loadArmCLabels(base + ".json")
	}
	return nil, nil
}

func cmdArmCAuditReport(args []string) error {
	fs := flag.NewFlagSet("audit-armC", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := applyArmDirs("C", false); err != nil {
		return err
	}
	out := filepath.Join(studyDir(), "AUDIT-armC.md")
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("refusing to overwrite published output: %s", out)
	}

	e1, err := loadAuditLabels("e1")
	if err != nil {
		return err
	}
	e2, err := loadAuditLabels("e2")
	if err != nil {
		return err
	}
	if e1 == nil || e2 == nil {
		return fmt.Errorf("both external raters must return audit labels; expected "+
			"%s.csv and %s.csv", auditLabelPath("e1"), auditLabelPath("e2"))
	}

	agr := computeAgreement("e1 vs e2 (both external)", e1, e2)

	// Ground truth over the audited set: rows the two externals agree on.
	// Disagreements are listed and excluded from the headline rather than
	// resolved silently by the author.
	inIntent, outIntent, disputed, unlabelable := 0, 0, 0, 0
	for k, a := range e1 {
		b, ok := e2[k]
		if !ok {
			continue
		}
		switch {
		case a.Label == "unlabelable" || b.Label == "unlabelable":
			unlabelable++
		case a.Label != b.Label:
			disputed++
		case a.Label == labelIn:
			inIntent++
		default:
			outIntent++
		}
	}
	agreed := inIntent + outIntent
	rate := 0.0
	if agreed > 0 {
		rate = float64(inIntent) / float64(agreed)
	}

	var w strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&w, f, v...) }
	p("# Arm C — exhaustive false-block audit\n\n")
	p("**This is not a held-out precision/recall evaluation, and it does not\n")
	p("repair prediction C6.** Arm C does not estimate recall and does not clear\n")
	p("the Track 2 metric bar; see `study/PRELABEL-FINDING-armC.md`.\n\n")
	p("What this measures is the conditional quantity Track 2 asks for by name —\n")
	p("false-positive cost:\n\n")
	p("> **in-intent calls among guard-blocked calls**\n\n")
	p("The audited set is **every call the guard refused**, exhaustively: no\n")
	p("sampling, no curation. Because the set is selected on the guard's own\n")
	p("decision, the rate below is conditional and is neither a precision nor a\n")
	p("recall. It cannot be converted into one.\n\n")
	p("Both raters are external, labelled independently, and were **not told what\n")
	p("these rows have in common**. They saw the same sanitised projection used\n")
	p("throughout arm C, with no outcome field of any kind.\n\n")

	p("## Inter-rater agreement, before adjudication\n\n")
	p("| | |\n|---|---|\n")
	p("| Calls compared | %d |\n", agr.N)
	p("| Agreed | %d |\n", agr.Agreed)
	p("| Raw agreement | %.3f |\n", agr.RawAgreement)
	p("| Cohen's kappa | %.3f |\n\n", agr.Kappa)
	p("Agreement on this corpus should be read cautiously: the rubric is close to\n")
	p("arithmetic here, so a high figure largely shows that two people can compare\n")
	p("two numbers.\n\n")

	p("## The result\n\n")
	p("| | n |\n|---|---|\n")
	p("| Calls the guard refused (audited exhaustively) | **%d** |\n", len(e1))
	p("| Both raters: in-intent | **%d** |\n", inIntent)
	p("| Both raters: out-of-intent | %d |\n", outIntent)
	p("| Raters disagreed (excluded from the rate) | %d |\n", disputed)
	p("| Unlabelable (excluded) | %d |\n\n", unlabelable)
	p("**in-intent among guard-blocked calls = %d / %d = %.3f**\n\n",
		inIntent, agreed, rate)
	p("Read plainly: of the refunds this guard refused, %.0f%% were refunds the\n",
		rate*100)
	p("merchant intended to make. That is the cost of the capability model, and\n")
	p("it falls on the compilation policy rather than on enforcement — a mandate\n")
	p("can only authorize what someone wrote down.\n\n")

	if len(agr.Disagreements) > 0 {
		p("## Every disagreement\n\n")
		var ks []string
		for _, d := range agr.Disagreements {
			ks = append(ks, d.Key)
		}
		sort.Strings(ks)
		for _, d := range agr.Disagreements {
			p("- `%s` — e1 **%s** (%s); e2 **%s** (%s)\n",
				d.Key, d.A, orDash(d.AWhy), d.B, orDash(d.BWhy))
		}
		p("\nThese are excluded from the headline rate rather than resolved by the\n")
		p("author, who is not an independent rater.\n\n")
	}
	if err := os.WriteFile(out, []byte(w.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("audit -> %s\n", out)
	fmt.Printf("  blocked calls audited: %d\n", len(e1))
	fmt.Printf("  in-intent among blocked: %d/%d = %.3f\n", inIntent, agreed, rate)
	fmt.Printf("  raw agreement %.3f  kappa %.3f  disagreements %d\n",
		agr.RawAgreement, agr.Kappa, len(agr.Disagreements))
	return nil
}
