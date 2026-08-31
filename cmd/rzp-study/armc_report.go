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

// agreement-armC computes inter-rater agreement and publishes it BEFORE any
// adjudication and before anything is joined to the guard's decisions.
//
// The ordering is the point. Agreement computed after adjudication measures how
// well the adjudicator reconciled two sheets, which is not a property of the
// labelling at all. This command therefore never opens a trace file.

type disagreement struct {
	Key    string `json:"key"`
	R1     string `json:"r1"`
	R2     string `json:"r2"`
	R1Why  string `json:"r1_reason,omitempty"`
	R2Why  string `json:"r2_reason,omitempty"`
	Intent string `json:"intent_text,omitempty"`
	Args   string `json:"arguments,omitempty"`
}

type agreementResult struct {
	N             int            `json:"n_compared"`
	Agreed        int            `json:"agreed"`
	RawAgreement  float64        `json:"raw_agreement"`
	Kappa         float64        `json:"cohens_kappa"`
	R1Counts      map[string]int `json:"r1_label_counts"`
	R2Counts      map[string]int `json:"r2_label_counts"`
	Unlabelable   []string       `json:"unlabelable_keys"`
	Disagreements []disagreement `json:"disagreements"`
}

func armCPaths() (r1, r2, agree, adj, results string) {
	ad := filepath.Join(studyDir(), "adjudication")
	return filepath.Join(ad, "labels-armC-r1.json"),
		filepath.Join(ad, "labels-armC-r2.json"),
		filepath.Join(studyDir(), "AGREEMENT-armC.md"),
		filepath.Join(ad, "adjudicated-armC.json"),
		filepath.Join(studyDir(), "RESULTS-armC.md")
}

func computeAgreement(a, b map[string]labelledRow) agreementResult {
	res := agreementResult{
		R1Counts: map[string]int{}, R2Counts: map[string]int{},
	}
	var keys []string
	for k := range a {
		if _, ok := b[k]; ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, k := range keys {
		la, lb := a[k].Label, b[k].Label
		res.R1Counts[la]++
		res.R2Counts[lb]++
		if la == "unlabelable" || lb == "unlabelable" {
			res.Unlabelable = append(res.Unlabelable, k)
			continue
		}
		res.N++
		if la == lb {
			res.Agreed++
		} else {
			res.Disagreements = append(res.Disagreements, disagreement{
				Key: k, R1: la, R2: lb, R1Why: a[k].Reason, R2Why: b[k].Reason,
			})
		}
	}
	if res.N > 0 {
		res.RawAgreement = float64(res.Agreed) / float64(res.N)
	}

	// Cohen's kappa over the two decidable categories.
	var p1in, p1out, p2in, p2out float64
	for _, k := range keys {
		la, lb := a[k].Label, b[k].Label
		if la == "unlabelable" || lb == "unlabelable" {
			continue
		}
		if la == labelIn {
			p1in++
		} else {
			p1out++
		}
		if lb == labelIn {
			p2in++
		} else {
			p2out++
		}
	}
	if res.N > 0 {
		n := float64(res.N)
		pe := (p1in/n)*(p2in/n) + (p1out/n)*(p2out/n)
		if pe < 1 {
			res.Kappa = (res.RawAgreement - pe) / (1 - pe)
		} else {
			res.Kappa = 1
		}
	}
	return res
}

func cmdArmCAgreement(args []string) error {
	fs := flag.NewFlagSet("agreement-armC", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := applyArmDirs("C", false); err != nil {
		return err
	}
	r1p, r2p, agreeP, _, _ := armCPaths()

	a, err := loadArmCLabels(r1p)
	if err != nil {
		return err
	}
	b, err := loadArmCLabels(r2p)
	if err != nil {
		return err
	}
	if len(a) != len(b) {
		return fmt.Errorf("the two label sets cover different rows (%d vs %d); "+
			"they must be the same worksheet", len(a), len(b))
	}
	res := computeAgreement(a, b)

	// Attach the row context to each disagreement, from the worksheet rather
	// than from a trace: this command must not read a guard decision.
	wsB, err := os.ReadFile(filepath.Join(studyDir(), "adjudication",
		"worksheet-armC-r1.json"))
	if err == nil {
		var sheet armCSheet
		if json.Unmarshal(wsB, &sheet) == nil {
			byKey := map[string]armCRow{}
			for _, r := range sheet.Rows {
				byKey[r.Key] = r
			}
			for i := range res.Disagreements {
				if r, ok := byKey[res.Disagreements[i].Key]; ok {
					res.Disagreements[i].Intent = r.IntentText
					res.Disagreements[i].Args = r.Arguments
				}
			}
		}
	}

	var w strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&w, f, v...) }
	p("# Arm C inter-rater agreement\n\n")
	p("Computed **before** any adjudication and before any label was joined to a\n")
	p("guard decision. This command does not open a trace file.\n\n")
	p("| | |\n|---|---|\n")
	p("| Calls compared | %d |\n", res.N)
	p("| Agreed | %d |\n", res.Agreed)
	p("| Raw agreement | %.3f |\n", res.RawAgreement)
	p("| Cohen's kappa | %.3f |\n", res.Kappa)
	p("| Excluded as unlabelable | %d |\n\n", len(res.Unlabelable))

	p("Rater 1 labels: ")
	p("%s\n\n", countsLine(res.R1Counts))
	p("Rater 2 labels: ")
	p("%s\n\n", countsLine(res.R2Counts))

	p("---\n\n## Every disagreement\n\n")
	if len(res.Disagreements) == 0 {
		p("None. The two raters agreed on every decidable call.\n\n")
	} else {
		p("All %d are listed. None is omitted, and none is resolved here --\n", len(res.Disagreements))
		p("adjudication is a separate, later step recorded in\n")
		p("`adjudication/adjudicated-armC.json`.\n\n")
		for _, d := range res.Disagreements {
			p("### %s\n\n", d.Key)
			p("- rater 1: **%s** — %s\n", d.R1, orDash(d.R1Why))
			p("- rater 2: **%s** — %s\n", d.R2, orDash(d.R2Why))
			if d.Intent != "" {
				p("- intent: %s\n", d.Intent)
			}
			if d.Args != "" {
				p("- call: `%s`\n", strings.TrimSpace(d.Args))
			}
			p("\n")
		}
	}
	if len(res.Unlabelable) > 0 {
		p("## Excluded as unlabelable\n\n")
		for _, k := range res.Unlabelable {
			p("- %s\n", k)
		}
		p("\n")
	}

	if err := os.WriteFile(agreeP, []byte(w.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("agreement -> %s\n", agreeP)
	fmt.Printf("  compared %d   agreed %d   raw %.3f   kappa %.3f\n",
		res.N, res.Agreed, res.RawAgreement, res.Kappa)
	fmt.Printf("  disagreements %d   unlabelable %d\n",
		len(res.Disagreements), len(res.Unlabelable))
	if len(res.Disagreements) > 0 {
		fmt.Println()
		fmt.Println("Resolve each one in adjudication/adjudicated-armC.json before reporting.")
	}
	return nil
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
