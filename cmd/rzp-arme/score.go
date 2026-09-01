package main

import (
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Scoring for arm E.
//
// GROUND TRUTH IS THE MAJORITY OF THE RETURNED CSVs. Nothing here reads a label
// from the corpus, because the corpus has none. That is the structural fix for
// the defect that withdrew arm D, and score_test.go asserts requests.json still
// carries no label field.
//
// Every returned file is verified field by field against the worksheet that was
// delivered. Only `label` and `reason` may differ. A rater whose spreadsheet
// reformatted an amount, dropped a column or reordered rows is asked to resend
// rather than having their file silently repaired -- a repair is an edit to
// someone else's judgment.

const (
	labelIn     = "in-intent"
	labelOut    = "out-of-intent"
	labelUnable = "unlabelable"
)

var validLabels = map[string]bool{labelIn: true, labelOut: true, labelUnable: true}

// worksheet column order, fixed by grid_e.py.
const (
	colRowID = iota
	colIntentText
	colIntentPayment
	colRequestPayment
	colAmount
	colLabel
	colReason
	numCols
)

type raterFile struct {
	name   string
	labels map[string]string // row_id -> label
	reason map[string]string
}

func worksheetPath() string { return filepath.Join(armEDir, "worksheet-armE.csv") }

func readCSV(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.FieldsPerRecord = numCols
	recs, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(recs) < 2 {
		return nil, fmt.Errorf("%s: no data rows", path)
	}
	return recs, nil
}

// loadRater verifies a returned file against the delivered worksheet and
// extracts the labels. Anything other than label and reason differing is a
// refusal, not a warning.
func loadRater(path string, canonical [][]string) (*raterFile, error) {
	recs, err := readCSV(path)
	if err != nil {
		return nil, err
	}
	if len(recs) != len(canonical) {
		return nil, fmt.Errorf("%s has %d rows, the delivered worksheet has %d. "+
			"Ask for a resend rather than repairing it", path, len(recs)-1, len(canonical)-1)
	}
	rf := &raterFile{
		name:   strings.TrimSuffix(filepath.Base(path), ".csv"),
		labels: map[string]string{},
		reason: map[string]string{},
	}
	for i := range recs {
		for c := 0; c < numCols; c++ {
			if c == colLabel || c == colReason {
				continue
			}
			if recs[i][c] != canonical[i][c] {
				return nil, fmt.Errorf("%s row %d column %d was changed:\n  sent:     %q\n  returned: %q\n"+
					"Only `label` and `reason` may differ. Ask for a resend",
					path, i, c+1, canonical[i][c], recs[i][c])
			}
		}
		if i == 0 {
			continue
		}
		id := recs[i][colRowID]
		lab := strings.ToLower(strings.TrimSpace(recs[i][colLabel]))
		if lab == "" {
			return nil, fmt.Errorf("%s: row %s has no label. Every row must be "+
				"labelled; an unlabelled row is not the same as `unlabelable`", path, id)
		}
		if !validLabels[lab] {
			return nil, fmt.Errorf("%s: row %s has label %q, which is not one of "+
				"%s, %s, %s", path, id, lab, labelIn, labelOut, labelUnable)
		}
		rf.labels[id] = lab
		rf.reason[id] = strings.TrimSpace(recs[i][colReason])
	}
	return rf, nil
}

// discoverRaters finds every returned label file. The protocol allows fewer
// than three; it does not allow pretending there were three.
func discoverRaters(canonical [][]string) ([]*raterFile, error) {
	var out []*raterFile
	ents, err := os.ReadDir(armEDir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "labels-armE-") && strings.HasSuffix(n, ".csv") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		rf, err := loadRater(filepath.Join(armEDir, n), canonical)
		if err != nil {
			return nil, err
		}
		out = append(out, rf)
	}
	return out, nil
}

// majority returns the agreed label and whether one exists. With three raters a
// label needs two votes. `unlabelable` can win, and when it does the row is
// excluded from the metrics and counted -- not resolved by the author.
func majority(votes []string) (string, bool) {
	count := map[string]int{}
	for _, v := range votes {
		count[v]++
	}
	best, n := "", 0
	for k, c := range count {
		if c > n || (c == n && k < best) {
			best, n = k, c
		}
	}
	return best, n*2 > len(votes)
}

// wilson is the 95% score interval. A normal approximation on 120 rows would
// give intervals that cross zero and one, which is why this is here rather than
// p +/- 1.96*sqrt(p(1-p)/n).
func wilson(k, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 0
	}
	const z = 1.959964
	p := float64(k) / float64(n)
	fn := float64(n)
	den := 1 + z*z/fn
	centre := (p + z*z/(2*fn)) / den
	half := z * math.Sqrt(p*(1-p)/fn+z*z/(4*fn*fn)) / den
	lo, hi = math.Max(0, centre-half), math.Min(1, centre+half)
	// At k=0 and k=n the closed form is exactly 0 and exactly 1, but the
	// floating-point evaluation lands a hair inside. Left alone, the
	// interval EXCLUDES its own point estimate -- 10 successes out of 10
	// would report [0.722, 0.99999999] and not contain 1.0. Snapped here
	// because the exact value is known, not to make a test pass.
	if k == 0 {
		lo = 0
	}
	if k == n {
		hi = 1
	}
	return lo, hi
}

// fleiss computes Fleiss' kappa over rows every rater labelled. Reported before
// the metrics: if raters did not agree, the majority they produced is a weak
// ground truth and everything computed on it inherits that.
func fleiss(rows []string, raters []*raterFile) (float64, int) {
	cats := []string{labelIn, labelOut, labelUnable}
	n := len(raters)
	if n < 2 {
		return math.NaN(), 0
	}
	var pbar float64
	catTotal := make([]float64, len(cats))
	used := 0
	for _, id := range rows {
		counts := make([]float64, len(cats))
		complete := true
		for _, r := range raters {
			l, ok := r.labels[id]
			if !ok {
				complete = false
				break
			}
			for ci, c := range cats {
				if l == c {
					counts[ci]++
				}
			}
		}
		if !complete {
			continue
		}
		used++
		var sum float64
		for ci := range cats {
			sum += counts[ci] * counts[ci]
			catTotal[ci] += counts[ci]
		}
		pbar += (sum - float64(n)) / float64(n*(n-1))
	}
	if used == 0 {
		return math.NaN(), 0
	}
	pbar /= float64(used)
	var pe float64
	for ci := range cats {
		pj := catTotal[ci] / float64(used*n)
		pe += pj * pj
	}
	if pe >= 1 {
		return math.NaN(), used
	}
	return (pbar - pe) / (1 - pe), used
}

func pct(x float64) string {
	if math.IsNaN(x) {
		return "n/a"
	}
	return fmt.Sprintf("%.3f", x)
}

func score() error {
	out := filepath.Join("study", "RESULTS-armE.md")
	if _, err := os.Stat(out); err == nil {
		return fmt.Errorf("refusing to overwrite %s. The corpus is scored ONCE; "+
			"a second scoring pass is a tuning pass", out)
	}

	canonical, err := readCSV(worksheetPath())
	if err != nil {
		return err
	}
	raters, err := discoverRaters(canonical)
	if err != nil {
		return err
	}
	if len(raters) == 0 {
		return fmt.Errorf("no returned label files found. Save them as "+
			"%s/labels-armE-r1.csv (r2, r3) -- CSV, not JSON", armEDir)
	}

	reqs, err := loadCorpus()
	if err != nil {
		return err
	}
	rowmapRaw, err := os.ReadFile(filepath.Join(armEDir, "rowmap.json"))
	if err != nil {
		return err
	}
	var rowmap map[string]cell
	if err := json.Unmarshal(rowmapRaw, &rowmap); err != nil {
		return err
	}

	ids := make([]string, 0, len(reqs))
	guard := map[string]bool{}
	amount := map[string]int64{}
	for _, r := range reqs {
		ok, _, err := decide(r)
		if err != nil {
			return err
		}
		ids = append(ids, r.RequestID)
		guard[r.RequestID] = ok
		amount[r.RequestID] = r.ReqAmount
	}
	sort.Strings(ids)

	var tp, fp, tn, fn int
	var noMajority, unable int
	var fpPaise, fnPaise int64
	byCell := map[string]map[string]int{}
	var fnRows, fpRows, noMajRows []string

	for _, id := range ids {
		votes := make([]string, 0, len(raters))
		for _, r := range raters {
			if l, ok := r.labels[id]; ok {
				votes = append(votes, l)
			}
		}
		if len(votes) == 0 {
			continue
		}
		lab, ok := majority(votes)
		if !ok {
			noMajority++
			noMajRows = append(noMajRows, id)
			continue
		}
		if lab == labelUnable {
			unable++
			continue
		}
		c := rowmap[id]
		key := c.IntentKind + "/" + c.Coverage
		if byCell[key] == nil {
			byCell[key] = map[string]int{}
		}
		switch {
		case lab == labelOut && !guard[id]:
			tp++
			byCell[key]["TP"]++
		case lab == labelIn && !guard[id]:
			fp++
			fpPaise += amount[id]
			fpRows = append(fpRows, id)
			byCell[key]["FP"]++
		case lab == labelOut && guard[id]:
			fn++
			fnPaise += amount[id]
			fnRows = append(fnRows, id)
			byCell[key]["FN"]++
		default:
			tn++
			byCell[key]["TN"]++
		}
	}

	scored := tp + fp + tn + fn
	prec, rec, fpr := 0.0, 0.0, 0.0
	if tp+fp > 0 {
		prec = float64(tp) / float64(tp+fp)
	}
	if tp+fn > 0 {
		rec = float64(tp) / float64(tp+fn)
	}
	if fp+tn > 0 {
		fpr = float64(fp) / float64(fp+tn)
	}
	pLo, pHi := wilson(tp, tp+fp)
	rLo, rHi := wilson(tp, tp+fn)
	fLo, fHi := wilson(fp, fp+tn)
	kappa, kappaN := fleiss(ids, raters)

	var w strings.Builder
	p := func(f string, v ...any) { fmt.Fprintf(&w, f, v...) }
	p("# Arm E results — a held-out evaluation with independent labels\n\n")
	p("Generated by `rzp-arme score`. Computed, not written by hand.\n\n")

	p("**Ground truth is the majority of %d independent human raters.** The corpus\n", len(raters))
	p("file carries no label field, so this program cannot score against an\n")
	p("author-declared label — the defect that withdrew arm D. Raters saw the\n")
	p("merchant's intent and the requested refund, and never the compiled\n")
	p("authorization, the decision, or `PROTOCOL-armE.md`.\n\n")

	if len(raters) < 3 {
		p("> **FEWER THAN THREE RATERS RETURNED (%d).** The pre-registration allows\n", len(raters))
		p("> this and requires it to be said plainly: the ground truth is weaker\n")
		p("> than planned")
		if len(raters) == 1 {
			p(", there is no agreement statistic at all, and a single rater's\n")
			p("> judgment is not independent of their own reading of the rubric")
		}
		p(".\n\n")
	}

	p("## 1. Agreement, before any metric\n\n")
	p("| | |\n|---|---|\n")
	p("| Raters | %d |\n", len(raters))
	p("| Rows scored by all raters | %d |\n", kappaN)
	p("| Fleiss' kappa | **%s** |\n", pct(kappa))
	p("| No-majority rows (excluded) | %d |\n", noMajority)
	p("| Majority `unlabelable` (excluded) | %d |\n", unable)
	p("| Rows entering the matrix | **%d** of %d |\n\n", scored, len(ids))
	p("Poor agreement means a weak majority, and every figure below inherits it.\n")
	p("It is reported first for that reason.\n\n")

	p("---\n\n## 2. Confusion matrix\n\n")
	p("Unit: one refund request. Positive class: **out-of-intent** by majority.\n")
	p("Predicted positive: the guard refused it.\n\n")
	p("| | guard refused | guard allowed |\n|---|---|---|\n")
	p("| **out-of-intent** (majority) | TP %d | FN %d |\n", tp, fn)
	p("| **in-intent** (majority) | FP %d | TN %d |\n\n", fp, tn)

	p("### Per-class rates, which are what transfer\n\n")
	p("| | value | 95%% Wilson |\n|---|---:|---|\n")
	p("| **Recall / TPR** | **%.3f** | %.3f – %.3f |\n", rec, rLo, rHi)
	p("| **False-positive rate** | **%.3f** | %.3f – %.3f |\n", fpr, fLo, fHi)
	p("| Precision | %.3f | %.3f – %.3f |\n\n", prec, pLo, pHi)
	p("**Precision is base-rate dependent and this corpus's balance is a design\n")
	p("choice.** Quote recall and the false-positive rate. The intervals are wide\n")
	p("because %d scored rows cannot support three decimal places, and reporting\n", scored)
	p("the point estimates alone would be the overclaim this arm exists to avoid.\n\n")

	if fn > 0 {
		p("---\n\n## 3. False negatives\n\n")
		p("Out-of-intent by majority, and forwarded. **%d rows, %d paise.**\n\n", fn, fnPaise)
		p("Arm D reported recall 1.000 because every positive it constructed was\n")
		p("unmatched by design. These rows exist because `coverage=over` lets an\n")
		p("authorized action cover an amount the merchant's words did not.\n\n")
		for _, id := range fnRows {
			c := rowmap[id]
			p("- `%s` — %d paise. Cell `%s/%s/%s`.\n",
				id, amount[id], c.IntentKind, c.Coverage, c.Request)
		}
		p("\n")
	} else {
		p("---\n\n## 3. False negatives\n\n**None.** Every request the majority\n")
		p("called out-of-intent was refused. Prediction E1 said recall would be\n")
		p("below 1.000; it was not, and `FINDINGS-armE.md` records why that is a\n")
		p("finding about the corpus rather than a triumph.\n\n")
	}

	p("---\n\n## 4. False positives\n\n")
	p("In-intent by majority, and refused. **%d rows, %d paise delayed.**\n", fp, fpPaise)
	p("Delayed, not lost: a blocked refund still happens once a human unblocks it.\n")
	p("`study/FP-COST.md` prices this.\n\n")

	p("### By intent kind and coverage\n\n")
	p("| cell | TP | FP | TN | FN |\n|---|---:|---:|---:|---:|\n")
	var keys []string
	for k := range byCell {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m := byCell[k]
		p("| `%s` | %d | %d | %d | %d |\n", k, m["TP"], m["FP"], m["TN"], m["FN"])
	}
	p("\n")

	if noMajority > 0 {
		p("---\n\n## 5. Rows with no majority\n\n")
		p("%d rows where three raters produced three different labels. Excluded\n", noMajority)
		p("from the matrix, counted here, and not resolved by the author:\n\n")
		for _, id := range noMajRows {
			c := rowmap[id]
			p("- `%s` — `%s/%s/%s`\n", id, c.IntentKind, c.Coverage, c.Request)
		}
		p("\n")
	}

	p("---\n\n## What this does not establish\n\n")
	p("- **Not a rate of agent misbehaviour.** The requests are constructed.\n")
	p("  Arm C asked how often an agent would make one, failed, and that failure\n")
	p("  stands.\n")
	p("- **Not merchant traffic**, and not a fraud rate.\n")
	p("- Precision is a property of this corpus's class balance.\n")
	p("- The false-positive rate is a property of the intent-to-authorization\n")
	p("  compilation as much as of the guard.\n")
	p("- One author designed the dimensions. Enumeration removes selection bias\n")
	p("  within the grid, not the grid's blind spots.\n")
	p("- Raters are independent of the implementation, not of the author who\n")
	p("  recruited them.\n")

	if err := os.WriteFile(out, []byte(w.String()), 0o644); err != nil {
		return err
	}

	// Manifest, so `verify` can prove the published numbers still reproduce.
	man := map[string]any{
		"note":     "Everything the arm E numbers depend on. `rzp-arme verify` recomputes and compares.",
		"raters":   len(raters),
		"matrix":   map[string]int{"tp": tp, "fp": fp, "tn": tn, "fn": fn},
		"excluded": map[string]int{"no_majority": noMajority, "unlabelable": unable},
		"labels_sha256": func() map[string]string {
			m := map[string]string{}
			for _, r := range raters {
				b, _ := os.ReadFile(filepath.Join(armEDir, r.name+".csv"))
				m[r.name+".csv"] = fmt.Sprintf("%x", sha256.Sum256(b))
			}
			return m
		}(),
	}
	mb, _ := json.MarshalIndent(man, "", "  ")
	if err := os.WriteFile(filepath.Join(armEDir, "manifest.json"), append(mb, '\n'), 0o644); err != nil {
		return err
	}

	fmt.Printf("scored %d of %d rows from %d rater(s) -> %s\n", scored, len(ids), len(raters), out)
	fmt.Printf("  TP %d  FP %d  TN %d  FN %d\n", tp, fp, tn, fn)
	fmt.Printf("  recall %.3f [%.3f-%.3f]  FPR %.3f [%.3f-%.3f]  precision %.3f\n",
		rec, rLo, rHi, fpr, fLo, fHi, prec)
	fmt.Printf("  Fleiss kappa %s over %d rows; %d no-majority, %d unlabelable\n",
		pct(kappa), kappaN, noMajority, unable)
	return nil
}

// verifyArmE recomputes the matrix from the returned labels and the frozen
// policy and compares it to what was published. Read-only.
func verifyArmE() error {
	b, err := os.ReadFile(filepath.Join(armEDir, "manifest.json"))
	if err != nil {
		return fmt.Errorf("no arm E manifest: score the corpus first: %w", err)
	}
	var man struct {
		Raters int            `json:"raters"`
		Matrix map[string]int `json:"matrix"`
		Labels map[string]string
	}
	if err := json.Unmarshal(b, &man); err != nil {
		return err
	}
	canonical, err := readCSV(worksheetPath())
	if err != nil {
		return err
	}
	raters, err := discoverRaters(canonical)
	if err != nil {
		return err
	}
	if len(raters) != man.Raters {
		return fmt.Errorf("manifest records %d rater file(s); %d present",
			man.Raters, len(raters))
	}
	reqs, err := loadCorpus()
	if err != nil {
		return err
	}
	var tp, fp, tn, fn int
	for _, r := range reqs {
		votes := make([]string, 0, len(raters))
		for _, rf := range raters {
			if l, ok := rf.labels[r.RequestID]; ok {
				votes = append(votes, l)
			}
		}
		lab, ok := majority(votes)
		if !ok || lab == labelUnable {
			continue
		}
		g, _, err := decide(r)
		if err != nil {
			return err
		}
		switch {
		case lab == labelOut && !g:
			tp++
		case lab == labelIn && !g:
			fp++
		case lab == labelOut && g:
			fn++
		default:
			tn++
		}
	}
	fmt.Println("=== arm E verification (read-only) ===")
	fmt.Printf("  recomputed: TP %d FP %d TN %d FN %d\n", tp, fp, tn, fn)
	fmt.Printf("  published:  TP %d FP %d TN %d FN %d\n",
		man.Matrix["tp"], man.Matrix["fp"], man.Matrix["tn"], man.Matrix["fn"])
	if tp != man.Matrix["tp"] || fp != man.Matrix["fp"] ||
		tn != man.Matrix["tn"] || fn != man.Matrix["fn"] {
		return fmt.Errorf("arm E no longer reproduces its published matrix: the " +
			"policy or the returned labels have changed since scoring, and the " +
			"published result is VOID")
	}
	fmt.Println("  reproduces the published matrix exactly")
	return nil
}
