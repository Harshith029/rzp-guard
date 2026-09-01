package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// POST-HOC, EXHAUSTIVE CONDITIONAL AUDIT of arm C's guard-blocked calls.
//
// Designed AFTER seeing arm C's outcome. It is not pre-registered, is not
// presented as such, and does not repair prediction C6. Arm C does not estimate
// recall and does not clear the Track 2 metric bar.
//
// The word "precision" does not appear in what this file emits, deliberately.
// The audited set is selected on the guard's own decision, so nothing computed
// over it is a detector metric, and borrowing the vocabulary of one would invite
// exactly the misreading the rest of this study has spent eleven rounds
// avoiding.
//
// WHY THE THREE-WAY SPLIT MATTERS. "in-intent among blocked" is NOT a guard
// false-positive rate. 18 of arm C's 54 cells are `coverage=under`, constructed
// so the merchant's intent deliberately exceeds what the compiled mandate can
// express. A refusal there is correct enforcement of an incomplete
// authorization, not a broken guard. Reporting one number would blame the guard
// for a property of the corpus. So every blocked call lands in exactly one of:
//
//	A. blocked, out-of-intent
//	   security-correct denial.
//
//	B. blocked, in-intent, no matching combination available
//	   authorization/availability friction. The mandate never expressed what the
//	   merchant wanted. The cost is real and falls on the compilation policy.
//
//	C. blocked, in-intent, a matching combination WAS available
//	   an actual guard false positive or implementation limit. The authority
//	   existed and the guard still refused.
//
// A and B are read off the raters' labels. B versus C is decided by the guard's
// OWN recorded available actions, parsed from its refusal message, which is more
// authoritative than reconstructing the mandate because it already accounts for
// actions consumed earlier in the same trace.

var availableActionRe = regexp.MustCompile(
	`\(exactly ([0-9]+) paise on (pay_SYN[0-9]+)\)`)

// exactlyReachable reports whether target is an exact subset sum of amts.
//
// Deliberately UNBOUNDED, unlike the guard's own search, which stops at eight
// actions. That difference is the point: a refusal the guard issued only because
// its bound was reached is a category C result, and a bounded check here would
// hide it by agreeing with the guard.
func exactlyReachable(target int64, amts []int64) bool {
	if target <= 0 {
		return false
	}
	sums := map[int64]bool{0: true}
	for _, a := range amts {
		if a <= 0 || a > target {
			continue
		}
		next := make(map[int64]bool, len(sums)*2)
		for s := range sums {
			next[s] = true
			if s+a <= target {
				next[s+a] = true
			}
		}
		sums = next
		if sums[target] {
			return true
		}
	}
	return sums[target]
}

type blockedCall struct {
	RowID     string
	TraceKey  string
	Payment   string
	Amount    int64
	Available []int64
	Reachable bool
	Cell      map[string]string
}

// loadBlockedCalls rebuilds the audited set from the traces and the audit join
// map. Runs only at report time, never on the worksheet path.
func loadBlockedCalls() (map[string]blockedCall, error) {
	b, err := os.ReadFile(filepath.Join(studyDir(), "adjudication",
		"audit-rowmap-armC.json"))
	if err != nil {
		return nil, fmt.Errorf("reading the audit join map: %w", err)
	}
	var rm rowMap
	if err := json.Unmarshal(b, &rm); err != nil {
		return nil, err
	}
	byTraceKey := map[string]string{}
	for rowID, key := range rm.ByRowID {
		byTraceKey[key] = rowID
	}

	reg, err := loadArms()
	if err != nil {
		return nil, err
	}
	arm, err := reg.find("C")
	if err != nil {
		return nil, err
	}
	traces, err := loadTraces(arm.tracePath())
	if err != nil {
		return nil, err
	}

	out := map[string]blockedCall{}
	for _, t := range traces {
		for i, c := range refundCalls(t) {
			if !c.Blocked {
				continue
			}
			key := fmt.Sprintf("%s_run%d_call%d", t.BriefID, t.RunIndex, i+1)
			rowID, ok := byTraceKey[key]
			if !ok {
				continue
			}
			pr := projectCall(c.Name, c.Arguments)
			pay, amt := pr.CallPayment, pr.AmountPaise
			var avail []int64
			for _, m := range availableActionRe.FindAllStringSubmatch(c.ResultText, -1) {
				if m[2] != pay {
					continue
				}
				if v, err := strconv.ParseInt(m[1], 10, 64); err == nil {
					avail = append(avail, v)
				}
			}
			out[rowID] = blockedCall{
				RowID: rowID, TraceKey: key, Payment: pay, Amount: amt,
				Available: avail, Reachable: exactlyReachable(amt, avail),
				Cell: briefCell(t.BriefID),
			}
		}
	}
	return out, nil
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
	blocked, err := loadBlockedCalls()
	if err != nil {
		return err
	}
	agr := computeAgreement("e1 vs e2 (both external)", e1, e2)

	// Classify. Disagreements are carried, not dropped.
	var agreedIn, agreedOut, disputed, unlabelable []string
	for k, a := range e1 {
		b, ok := e2[k]
		if !ok {
			continue
		}
		switch {
		case a.Label == "unlabelable" || b.Label == "unlabelable":
			unlabelable = append(unlabelable, k)
		case a.Label != b.Label:
			disputed = append(disputed, k)
		case a.Label == labelIn:
			agreedIn = append(agreedIn, k)
		default:
			agreedOut = append(agreedOut, k)
		}
	}
	sort.Strings(agreedIn)
	sort.Strings(disputed)

	countCats := func(keys []string) (friction, defect int, defectKeys []string) {
		for _, k := range keys {
			if bc, ok := blocked[k]; ok && bc.Reachable {
				defect++
				defectKeys = append(defectKeys, k)
			} else {
				friction++
			}
		}
		return
	}
	fricAgreed, defAgreed, defectKeys := countCats(agreedIn)
	fricDisp, defDisp, _ := countCats(disputed)

	total := len(blocked)
	agreed := len(agreedIn) + len(agreedOut)
	rateAgreed := 0.0
	if agreed > 0 {
		rateAgreed = float64(len(agreedIn)) / float64(agreed)
	}
	// Conservative bounds over ALL audited calls: every disagreement counted
	// once each way.
	loIn := len(agreedIn)
	hiIn := len(agreedIn) + len(disputed)
	den := total - len(unlabelable)
	loRate, hiRate := 0.0, 0.0
	if den > 0 {
		loRate = float64(loIn) / float64(den)
		hiRate = float64(hiIn) / float64(den)
	}

	var w strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&w, f, v...) }
	p("# Arm C — post-hoc, exhaustive conditional audit of guard-blocked calls\n\n")
	p("**Post-hoc and not pre-registered.** This audit was designed *after* arm C's\n")
	p("outcome was known, and it must not be presented as a pre-registered study.\n")
	p("Arm C's own pre-registration covers the grid, the ground-truth rule, the\n")
	p("policy freeze and predictions C1–C7; it does not cover this.\n\n")
	p("**It does not repair prediction C6.** Arm C does not estimate recall and\n")
	p("does not clear the Track 2 metric bar. Nothing below changes that, and no\n")
	p("quantity here is a detector metric: the audited set is selected on the\n")
	p("guard's own decision, so every rate is conditional on being refused.\n\n")
	p("What it is: an exhaustive examination of **every** call the guard refused —\n")
	p("no sampling, no curation — asking how much of that refusal was operational\n")
	p("friction and how much was warranted.\n\n")

	p("---\n\n## 1. Inter-rater agreement, published before the classification\n\n")
	p("| | |\n|---|---|\n")
	p("| Calls audited (all refused calls) | **%d** |\n", total)
	p("| Compared | %d |\n", agr.N)
	p("| Agreed | %d |\n", agr.Agreed)
	p("| Raw agreement | %.3f |\n", agr.RawAgreement)
	p("| Cohen's kappa | %.3f |\n", agr.Kappa)
	p("| Disagreed | %d |\n", len(disputed))
	p("| Unlabelable | %d |\n\n", len(unlabelable))
	p("Read cautiously: on this corpus the rubric is close to arithmetic, so a high\n")
	p("figure largely shows that two people can compare two numbers.\n\n")

	p("---\n\n## 2. The three-way split\n\n")
	p("A single \"in-intent among blocked\" number would be misleading, because 18\n")
	p("of arm C's 54 cells are `coverage=under` — built so the merchant's intent\n")
	p("deliberately exceeds what the compiled mandate expresses. A refusal there is\n")
	p("correct enforcement of an incomplete authorization, not a broken guard.\n\n")
	p("Categories B and C are separated by the guard's **own** recorded available\n")
	p("actions, parsed from its refusal message — which already accounts for\n")
	p("actions consumed earlier in the same trace. The reachability check here is\n")
	p("deliberately **unbounded**, unlike the guard's own search, so a refusal\n")
	p("caused only by that bound shows up as category C instead of hiding.\n\n")
	p("| | n | what it means |\n|---|---|---|\n")
	p("| **A** — blocked, out-of-intent | **%d** | security-correct denial |\n",
		len(agreedOut))
	p("| **B** — blocked, in-intent, no matching combination available | **%d** | authorization/availability friction; the mandate never expressed it |\n",
		fricAgreed)
	p("| **C** — blocked, in-intent, a matching combination WAS available | **%d** | actual guard false positive or implementation limit |\n",
		defAgreed)
	p("| disagreed (carried, not dropped) | %d | of which %d would fall in C |\n",
		len(disputed), defDisp)
	p("| unlabelable | %d | excluded |\n\n", len(unlabelable))
	_ = fricDisp

	if defAgreed > 0 {
		p("### Category C in detail\n\n")
		p("These are the ones that matter: the authority existed and the guard\n")
		p("refused anyway.\n\n")
		for _, k := range defectKeys {
			bc := blocked[k]
			p("- `%s` — %d paise requested; available actions summed exactly to it "+
				"(%d actions). Cell `coverage=%s scope=%s size=%s`.\n",
				k, bc.Amount, len(bc.Available), bc.Cell["coverage"],
				bc.Cell["scope"], bc.Cell["size"])
		}
		p("\n")
	}

	p("---\n\n## 3. The conditional rate, and bounds\n\n")
	p("**Agreed-label conditional rate** — over calls both raters labelled the\n")
	p("same, excluding disagreements and unlabelable rows:\n\n")
	p("> in-intent among guard-blocked calls = **%d / %d = %.3f**\n\n",
		len(agreedIn), agreed, rateAgreed)
	p("**Conservative bounds** — over all %d audited calls, counting every\n", den)
	p("disagreement once as in-intent and once as out-of-intent, so no\n")
	p("disagreement is silently discarded:\n\n")
	p("> in-intent among guard-blocked calls is between **%.3f** (%d/%d) and\n",
		loRate, loIn, den)
	p("> **%.3f** (%d/%d).\n\n", hiRate, hiIn, den)
	p("Neither figure is a detector metric. Both are conditional on a call having\n")
	p("been refused, and the set was chosen on that basis.\n\n")

	if len(agr.Disagreements) > 0 {
		p("### Every disagreement\n\n")
		for _, d := range agr.Disagreements {
			p("- `%s` — e1 **%s** (%s); e2 **%s** (%s)\n",
				d.Key, d.A, orDash(d.AWhy), d.B, orDash(d.BWhy))
		}
		p("\nThey are included in the bounds above rather than resolved by the\n")
		p("author, who is not an independent rater.\n\n")
	}

	p("---\n\n## 4. What this does not establish\n\n")
	p("- Not a detector metric of any kind, and not a substitute for the failed\n")
	p("  recall experiment.\n")
	p("- Not pre-registered; designed after arm C's outcome was known.\n")
	p("- Not representative traffic: the rows are selected on the outcome, and\n")
	p("  their distribution reflects the grid's construction.\n")
	p("- Category B is a cost of the **compilation policy**, not of enforcement. A\n")
	p("  mandate can only authorize what someone wrote down.\n")
	p("- Synthetic, model-generated calls. The generator's served model is\n")
	p("  self-reported by an endpoint measured substituting models.\n")

	if err := os.WriteFile(out, []byte(w.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("audit -> %s\n", out)
	fmt.Printf("  audited %d refused calls\n", total)
	fmt.Printf("  A out-of-intent %d   B friction %d   C guard-defect %d   disputed %d\n",
		len(agreedOut), fricAgreed, defAgreed, len(disputed))
	fmt.Printf("  in-intent among blocked: agreed %.3f, bounds %.3f-%.3f\n",
		rateAgreed, loRate, hiRate)
	return nil
}

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
