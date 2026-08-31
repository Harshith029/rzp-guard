package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// agreement-armC publishes inter-rater agreement BEFORE any adjudication and
// before anything is joined to a guard decision. It never opens a trace or the
// join map.
//
// The primary statistic is between EXTERNAL raters. The author-rater's labels
// are computed and shown, and are explicitly not part of it: the author wrote
// the corpus generator, so an author/external kappa measures how well an
// informed rater matches an uninformed one, which is not evidence of
// independent ground truth.

type disagreement struct {
	Key    string `json:"key"`
	A      string `json:"a"`
	B      string `json:"b"`
	AWhy   string `json:"a_reason,omitempty"`
	BWhy   string `json:"b_reason,omitempty"`
	Intent string `json:"intent_text,omitempty"`
	Call   string `json:"call,omitempty"`
}

type agreementResult struct {
	Pair          string         `json:"pair"`
	N             int            `json:"n_compared"`
	Agreed        int            `json:"agreed"`
	RawAgreement  float64        `json:"raw_agreement"`
	Kappa         float64        `json:"cohens_kappa"`
	ACounts       map[string]int `json:"a_label_counts"`
	BCounts       map[string]int `json:"b_label_counts"`
	Unlabelable   []string       `json:"unlabelable_keys"`
	Disagreements []disagreement `json:"disagreements"`
}

func armCLabelPath(rater string) string {
	return filepath.Join(studyDir(), "adjudication",
		fmt.Sprintf("labels-armC-%s.json", rater))
}

func armCPaths() (agree, adj, results string) {
	return filepath.Join(studyDir(), "AGREEMENT-armC.md"),
		filepath.Join(studyDir(), "adjudication", "adjudicated-armC.json"),
		filepath.Join(studyDir(), "RESULTS-armC.md")
}

func computeAgreement(pair string, a, b map[string]labelledRow) agreementResult {
	res := agreementResult{Pair: pair,
		ACounts: map[string]int{}, BCounts: map[string]int{}}
	var keys []string
	for k := range a {
		if _, ok := b[k]; ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, k := range keys {
		la, lb := a[k].Label, b[k].Label
		res.ACounts[la]++
		res.BCounts[lb]++
		if la == "unlabelable" || lb == "unlabelable" {
			res.Unlabelable = append(res.Unlabelable, k)
			continue
		}
		res.N++
		if la == lb {
			res.Agreed++
		} else {
			res.Disagreements = append(res.Disagreements, disagreement{
				Key: k, A: la, B: lb, AWhy: a[k].Reason, BWhy: b[k].Reason,
			})
		}
	}
	if res.N > 0 {
		res.RawAgreement = float64(res.Agreed) / float64(res.N)
	}

	var a1, a2, b1, b2 float64
	for _, k := range keys {
		la, lb := a[k].Label, b[k].Label
		if la == "unlabelable" || lb == "unlabelable" {
			continue
		}
		if la == labelIn {
			a1++
		} else {
			a2++
		}
		if lb == labelIn {
			b1++
		} else {
			b2++
		}
	}
	if res.N > 0 {
		n := float64(res.N)
		pe := (a1/n)*(b1/n) + (a2/n)*(b2/n)
		if pe < 1 {
			res.Kappa = (res.RawAgreement - pe) / (1 - pe)
		} else {
			res.Kappa = 1
		}
	}
	return res
}

// attachContext fills disagreement rows from a WORKSHEET, never a trace.
func attachContext(res *agreementResult) {
	b, err := os.ReadFile(filepath.Join(studyDir(), "adjudication",
		"worksheet-armC-e1.json"))
	if err != nil {
		return
	}
	var sheet armCSheet
	if json.Unmarshal(b, &sheet) != nil {
		return
	}
	byID := map[string]armCRow{}
	for _, r := range sheet.Rows {
		byID[r.RowID] = r
	}
	for i := range res.Disagreements {
		if r, ok := byID[res.Disagreements[i].Key]; ok {
			res.Disagreements[i].Intent = r.IntentText
			res.Disagreements[i].Call = fmt.Sprintf(
				"intent payment %s; call refunded %s for %d paise",
				r.IntentPayment, r.CallPayment, r.AmountPaise)
		}
	}
}

func loadIfPresent(rater string) (map[string]labelledRow, error) {
	p := armCLabelPath(rater)
	if _, err := os.Stat(p); err != nil {
		return nil, nil
	}
	return loadArmCLabels(p)
}

func cmdArmCAgreement(args []string) error {
	fs := flag.NewFlagSet("agreement-armC", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := applyArmDirs("C", false); err != nil {
		return err
	}
	agreeP, _, _ := armCPaths()

	e1, err := loadIfPresent("e1")
	if err != nil {
		return err
	}
	if e1 == nil {
		return fmt.Errorf("%s is required: at least one EXTERNAL rater must label "+
			"before agreement can be reported", armCLabelPath("e1"))
	}
	e2, err := loadIfPresent("e2")
	if err != nil {
		return err
	}
	author, err := loadIfPresent("author")
	if err != nil {
		return err
	}

	var w strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&w, f, v...) }
	p("# Arm C inter-rater agreement\n\n")
	p("Published **before** any adjudication and before any label was joined to a\n")
	p("guard decision. This step opens no trace file and no join map.\n\n")

	var primary *agreementResult
	if e2 != nil {
		r := computeAgreement("e1 vs e2 (both external)", e1, e2)
		attachContext(&r)
		primary = &r
		p("## Primary: two external raters\n\n")
		p("Both received only the exported worksheet and the rubric — no repository,\n")
		p("no generator, no join map, no trace filenames, no results. **This is the\n")
		p("agreement that carries evidential weight.**\n\n")
		p("| | |\n|---|---|\n")
		p("| Calls compared | %d |\n", r.N)
		p("| Agreed | %d |\n", r.Agreed)
		p("| Raw agreement | %.3f |\n", r.RawAgreement)
		p("| Cohen's kappa | %.3f |\n", r.Kappa)
		p("| Excluded as unlabelable | %d |\n\n", len(r.Unlabelable))
		p("Rater e1: %s\n\n", countsLine(r.ACounts))
		p("Rater e2: %s\n\n", countsLine(r.BCounts))
	} else {
		p("## ONE external rater only — the ground truth is weaker for it\n\n")
		p("A second external rater was not available, so **there is no primary\n")
		p("inter-rater kappa.** Ground truth rests on a single independent rater.\n")
		p("That is a real limitation and it is not repaired by anything below: an\n")
		p("agreement figure computed against the author is not evidence of\n")
		p("independence, because the author wrote the corpus generator.\n\n")
		p("Rater e1 labels: %s\n\n", countsLine(countLabels(e1)))
	}

	if author != nil {
		var ref map[string]labelledRow
		refName := "e1"
		if e2 != nil {
			ref = e2
			refName = "e2"
		} else {
			ref = e1
		}
		_ = ref
		ar := computeAgreement("author vs "+refName+" (SUPPLEMENTARY)", author, e1)
		p("---\n\n## Supplementary: author-rater\n\n")
		p("**These labels are not blinded and are not ground truth.** The author\n")
		p("wrote the corpus generator and knows how each case was constructed;\n")
		p("hiding row metadata does not undo that. The figure below says how often\n")
		p("an informed rater matched an uninformed one. It is **not** an\n")
		p("independence check and must never be quoted as one.\n\n")
		p("| | |\n|---|---|\n")
		p("| Compared with | e1 (external) |\n")
		p("| Calls compared | %d |\n", ar.N)
		p("| Raw agreement | %.3f |\n", ar.RawAgreement)
		p("| Cohen's kappa | %.3f |\n\n", ar.Kappa)
		p("Author labels: %s\n\n", countsLine(ar.ACounts))
	}

	if primary != nil {
		p("---\n\n## Every disagreement between the external raters\n\n")
		if len(primary.Disagreements) == 0 {
			p("None. The two external raters agreed on every decidable call.\n\n")
		} else {
			p("All %d are listed; none is omitted and none is resolved here.\n",
				len(primary.Disagreements))
			p("Adjudication is a separate, later step.\n\n")
			for _, d := range primary.Disagreements {
				p("### %s\n\n", d.Key)
				p("- e1: **%s** — %s\n", d.A, orDash(d.AWhy))
				p("- e2: **%s** — %s\n", d.B, orDash(d.BWhy))
				if d.Intent != "" {
					p("- intent: %s\n", d.Intent)
				}
				if d.Call != "" {
					p("- %s\n", d.Call)
				}
				p("\n")
			}
		}
		if len(primary.Unlabelable) > 0 {
			p("## Excluded as unlabelable\n\n")
			for _, k := range primary.Unlabelable {
				p("- %s\n", k)
			}
			p("\n")
		}
	}

	if err := os.WriteFile(agreeP, []byte(w.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("agreement -> %s\n", agreeP)
	if primary != nil {
		fmt.Printf("  PRIMARY (e1 vs e2): compared %d  raw %.3f  kappa %.3f  disagreements %d\n",
			primary.N, primary.RawAgreement, primary.Kappa, len(primary.Disagreements))
	} else {
		fmt.Println("  NO primary kappa: only one external rater. Ground truth is weaker.")
	}
	if author != nil {
		fmt.Println("  author-rater present; reported as supplementary, not as independence")
	}
	return nil
}

func countLabels(m map[string]labelledRow) map[string]int {
	out := map[string]int{}
	for _, r := range m {
		out[r.Label]++
	}
	return out
}

func countsLine(m map[string]int) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var parts []string
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no reason given)"
	}
	return s
}
