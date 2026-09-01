// Command rzp-armd scores the guard as a verifier on the arm D corpus and
// writes a CONFORMANCE report.
//
// WHAT THIS IS. A same-author synthetic conformance corpus. It scores the
// guard's decisions against AUTHOR-DECLARED labels: the switch below branches
// on r.Label, a field written into requests.json by study/grid_d.py. It does
// NOT read intent_text, it derives no label of its own, and a one-field edit to
// `label` changes every number it prints.
//
// THE OUTPUT PATH MOVED, DELIBERATELY. This program used to write
// study/RESULTS-armD.md, and that report claimed an evaluation on data the
// author had not seen, with ground truth taken from the intent. Both claims
// were withdrawn on 2026-09-01. The original report is preserved unedited as
// the historical record, and this program can no longer overwrite it or
// regenerate its prose: it writes study/armD/CONFORMANCE-armD.md instead. See
// study/ASSESSMENT-armD.md.
//
// WHAT THE POLICY FREEZE BUYS. The policy was written and frozen before this
// corpus existed, so the implementation cannot have been fitted to data that
// did not exist. That is all it buys. It does not blind the author, who knew
// the decision rule while building the corpus, and it does not make the labels
// independent.
//
// WHAT THIS IS NOT. Not agent traffic. The requests are constructed, so nothing
// here says how often an agent would make one, and no figure may be presented
// as a rate of agent misbehaviour. Arm C asked that question, failed, and its
// failure stands.
//
// Nothing here is random and no model is called, so the report reproduces byte
// for byte. `rzp-armd verify` checks that, and checks the decision path and the
// corpus against study/armD/manifest.json.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/policy"
)

type request struct {
	RequestID    string            `json:"request_id"`
	Cell         map[string]string `json:"cell"`
	IntentText   string            `json:"intent_text"`
	IntentTotal  int64             `json:"intent_total_paise"`
	ReqPayment   string            `json:"request_payment"`
	ReqAmount    int64             `json:"request_amount_paise"`
	Label        string            `json:"label"`
	CoverageNote string            `json:"coverage_note"`
}

type scored struct {
	request
	Allowed bool
	Rule    string
	Outcome string // TP FP TN FN
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "rzp-armd:", err)
		os.Exit(1)
	}
}

func run() error {
	// -detail prints per-row outcomes and writes NOTHING. It exists so a
	// prediction that failed can be diagnosed without re-scoring: the published
	// result stays immutable and a second scoring pass is still refused below.
	if len(os.Args) > 1 && os.Args[1] == "verify" {
		return verifyArmD()
	}
	// Records study/armD/manifest.json, once. It refuses to overwrite one.
	if len(os.Args) > 1 && os.Args[1] == "manifest" {
		return recordManifest()
	}
	detail := len(os.Args) > 1 && os.Args[1] == "-detail"

	root := "study/armD"
	out := "study/armD/CONFORMANCE-armD.md"
	if detail {
		out = ""
	}
	if _, err := os.Stat(out); err == nil && !detail {
		return fmt.Errorf("refusing to overwrite published output: %s. The corpus "+
			"is scored ONCE; a second scoring pass is a tuning pass", out)
	}

	b, err := os.ReadFile(filepath.Join(root, "requests.json"))
	if err != nil {
		return err
	}
	var reqs []request
	if err := json.Unmarshal(b, &reqs); err != nil {
		return err
	}
	if len(reqs) == 0 {
		return fmt.Errorf("empty corpus")
	}

	// A fixed clock. Decide takes `now` as a parameter and policy.go contains no
	// time.Now() or rand, so the whole evaluation is reproducible.
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	var rows []scored
	for _, r := range reqs {
		mb, err := os.ReadFile(filepath.Join(root, "mandates", r.RequestID+".json"))
		if err != nil {
			return err
		}
		m, err := mandate.Load(mb)
		if err != nil {
			return fmt.Errorf("%s: %w", r.RequestID, err)
		}
		g := policy.New(m)
		d := g.Decide("create_refund", map[string]any{
			"payment_id": r.ReqPayment,
			"amount":     json.Number(fmt.Sprintf("%d", r.ReqAmount)),
		}, now)

		s := scored{request: r, Allowed: d.Allowed, Rule: d.Rule}
		switch {
		case r.Label == "out-of-intent" && !d.Allowed:
			s.Outcome = "TP"
		case r.Label == "in-intent" && !d.Allowed:
			s.Outcome = "FP"
		case r.Label == "out-of-intent" && d.Allowed:
			s.Outcome = "FN"
		default:
			s.Outcome = "TN"
		}
		rows = append(rows, s)
	}

	var tp, fp, tn, fn int
	var fpPaise int64
	byCoverage := map[string]map[string]int{}
	byRequest := map[string]map[string]int{}
	for _, s := range rows {
		switch s.Outcome {
		case "TP":
			tp++
		case "FP":
			fp++
			fpPaise += s.ReqAmount
		case "TN":
			tn++
		case "FN":
			fn++
		}
		cov := s.Cell["coverage"]
		if byCoverage[cov] == nil {
			byCoverage[cov] = map[string]int{}
		}
		byCoverage[cov][s.Outcome]++
		rq := s.Cell["request"]
		if byRequest[rq] == nil {
			byRequest[rq] = map[string]int{}
		}
		byRequest[rq][s.Outcome]++
	}

	prec, rec := 0.0, 0.0
	if tp+fp > 0 {
		prec = float64(tp) / float64(tp+fp)
	}
	if tp+fn > 0 {
		rec = float64(tp) / float64(tp+fn)
	}
	// Per-class rates, which are what transfer across base rates.
	tpr := rec
	fpr := 0.0
	if fp+tn > 0 {
		fpr = float64(fp) / float64(fp+tn)
	}

	var w strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&w, f, v...) }
	p("# Arm D conformance report — a synthetic grid\n\n")
	p("Generated by `rzp-armd`. Computed, not written by hand.\n\n")

	p("> **Arm D is a pre-registered, same-author synthetic conformance corpus\n")
	p("> scored against author-declared labels.**\n")
	p(">\n")
	p("> **Its %d-row confusion matrix is exact for that finite grid, but it is\n", len(rows))
	p("> not independently labelled or policy-blind and does not establish\n")
	p("> transferable recall, precision, or false-positive cost.**\n")
	p(">\n")
	p("> Read `study/ASSESSMENT-armD.md` before quoting any figure below.\n\n")

	p("## Scope\n\n")
	p("A conformance check of a **deterministic verifier** on a constructed grid of\n")
	p("%d refund requests.\n\n", len(rows))

	p("**Scored against author-declared labels.** Each decision is compared to the\n")
	p("`label` field in `requests.json`. `rzp-armd` reads that field and nothing\n")
	p("else: it does not read `intent_text` and derives no label of its own. The\n")
	p("labels, the corpus generator and the policy have one author.\n\n")

	p("**Not agent traffic.** The requests are constructed, so nothing here says\n")
	p("how often an agent would make one. Arm C asked that question, failed, and\n")
	p("that failure stands: see `PRELABEL-FINDING-armC.md`.\n\n")

	p("**Policy frozen before the corpus existed.** `fb87b12` (2026-08-30). The\n")
	p("implementation cannot have been fitted to data that did not exist, and the\n")
	p("commits are checkable. That is all the date establishes — it does not blind\n")
	p("the author, who knew the decision rule while building the corpus.\n\n")

	p("**No model is called; nothing is random; the corpus is scored once.**\n\n")

	p("---\n\n## 1. Confusion matrix\n\n")
	p("Unit: one refund request. Positive class: **out-of-intent**.\n")
	p("Predicted positive: the guard refused it.\n\n")
	p("| | guard refused | guard allowed |\n|---|---|---|\n")
	p("| **out-of-intent** | TP %d | FN %d |\n", tp, fn)
	p("| **in-intent** | FP %d | TN %d |\n\n", fp, tn)
	p("- **Precision %.3f**  (TP / TP+FP)\n", prec)
	p("- **Recall %.3f**  (TP / TP+FN)\n", rec)
	p("- True-positive rate %.3f, false-positive rate %.3f\n\n", tpr, fpr)

	p("### Class counts\n\n| | n |\n|---|---|\n")
	p("| out-of-intent | %d |\n", tp+fn)
	p("| in-intent | %d |\n", fp+tn)
	p("| total | %d |\n\n", len(rows))

	p("---\n\n## 2. Precision is base-rate dependent; recall is not\n\n")
	p("Prediction D5, recorded before scoring. The corpus is %.0f%% positive by\n",
		100*float64(tp+fn)/float64(len(rows)))
	p("construction, and that is a design choice, not an observed rate. Precision\n")
	p("moves with it. **Recall does not** — it is computed only over positives.\n\n")
	p("Holding the measured rates fixed (TPR %.3f, FPR %.3f), precision at other\n", tpr, fpr)
	p("base rates would be:\n\n")
	p("| out-of-intent share | precision |\n|---|---|\n")
	for _, base := range []float64{0.60, 0.25, 0.10, 0.05, 0.01} {
		num := tpr * base
		den := num + fpr*(1-base)
		pv := 0.0
		if den > 0 {
			pv = num / den
		}
		label := fmt.Sprintf("%.0f%%", base*100)
		if base > 0.59 && base < 0.61 {
			label += " (this corpus)"
		}
		p("| %s | %.3f |\n", label, pv)
	}
	p("\nNone of these figures transfers. The class balance is a design choice and\n")
	p("the labels are the author's, so the row that would apply to real traffic is\n")
	p("not in this table.\n\n")

	p("---\n\n## 3. Rows refused that the author labelled in-intent\n\n")
	p("The labels count these as false positives. **Whether every one of them is a\n")
	p("refund a merchant actually wanted is contested and unsettled**: the\n")
	p("`coverage=exact / request=under` rows may instead be a correct refusal to\n")
	p("honour half of an exactly-authorized amount. Settling that needs independent\n")
	p("labels, which this corpus does not have. See `study/ASSESSMENT-armD.md`.\n\n")
	p("| | |\n|---|---|\n")
	p("| False positives | %d |\n", fp)
	p("| Value refused | %d paise |\n", fpPaise)
	p("| False-positive rate | %.3f |\n\n", fpr)
	p("### By mandate coverage\n\n")
	p("| coverage | TP | FP | TN | FN |\n|---|---|---|---|---|\n")
	var covs []string
	for k := range byCoverage {
		covs = append(covs, k)
	}
	sort.Strings(covs)
	for _, c := range covs {
		m := byCoverage[c]
		p("| `%s` | %d | %d | %d | %d |\n", c, m["TP"], m["FP"], m["TN"], m["FN"])
	}
	p("\nPredictions D3 and D4 said every false positive would fall in\n")
	p("`coverage=under` and none in `exact` or `split`. The table above is the test.\n\n")

	p("### By request type\n\n")
	p("| request | TP | FP | TN | FN |\n|---|---|---|---|---|\n")
	var rqs []string
	for k := range byRequest {
		rqs = append(rqs, k)
	}
	sort.Strings(rqs)
	for _, r := range rqs {
		m := byRequest[r]
		p("| `%s` | %d | %d | %d | %d |\n", r, m["TP"], m["FP"], m["TN"], m["FN"])
	}
	p("\n")

	if fn > 0 {
		p("---\n\n## 4. False negatives — SECURITY DEFECTS\n\n")
		p("Prediction D1 said recall would be 1.000, because an out-of-intent\n")
		p("request asks for something no action authorizes. Each row below is an\n")
		p("out-of-intent request the guard ALLOWED, which is a defect and not a\n")
		p("metric.\n\n")
		for _, s := range rows {
			if s.Outcome == "FN" {
				p("- `%s` — %d paise on `%s`, intent authorized %d. Cell `coverage=%s request=%s`.\n",
					s.RequestID, s.ReqAmount, s.ReqPayment, s.IntentTotal,
					s.Cell["coverage"], s.Cell["request"])
			}
		}
		p("\n")
	}

	p("---\n\n## %d. What this does not establish\n\n", map[bool]int{true: 5, false: 4}[fn > 0])
	p("- Not a rate of agent misbehaviour. The requests are constructed.\n")
	p("- Not a metric result. The labels are author-declared, not independent.\n")
	p("- Not merchant traffic, and not a fraud rate.\n")
	p("- Precision is a property of this corpus's class balance.\n")
	p("- The false-positive cost is a property of the intent→mandate compiler as\n")
	p("  much as of the guard. `coverage=under` cells were built so the mandate\n")
	p("  under-expresses the intent; refusing there is correct enforcement of an\n")
	p("  incomplete authorization.\n")
	p("- One author designed the dimensions. Enumeration removes selection bias\n")
	p("  within the grid, not the grid's blind spots.\n")

	if detail {
		fmt.Println("DETAIL MODE -- nothing written. Per-row outcomes:")
		for _, s := range rows {
			if s.Outcome == "FP" || s.Outcome == "FN" {
				fmt.Printf("  %-3s %s  coverage=%-6s request=%-13s intent=%-7d asked=%-7d %s\n",
					s.Outcome, s.RequestID, s.Cell["coverage"], s.Cell["request"],
					s.IntentTotal, s.ReqAmount, s.Rule)
			}
		}
		return nil
	}
	if err := os.WriteFile(out, []byte(w.String()), 0o644); err != nil {
		return err
	}
	fmt.Printf("scored %d requests -> %s\n", len(rows), out)
	fmt.Printf("  TP %d  FP %d  TN %d  FN %d\n", tp, fp, tn, fn)
	fmt.Printf("  precision %.3f  recall %.3f  FPR %.3f\n", prec, rec, fpr)
	if fn > 0 {
		fmt.Printf("  WARNING: %d false negative(s) -- an out-of-intent request was ALLOWED\n", fn)
	}
	return nil
}
