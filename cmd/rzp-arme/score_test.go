package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// THE DEFECT THIS GUARDS. Arm D's scorer branched on a `label` field its
// generator wrote into the corpus, so a one-field edit moved the reported
// precision and recall. Arm E's corpus has no such field and this asserts it on
// the real file, because a comment saying so would not survive the next edit.
func TestTheCorpusCarriesNoAuthoredLabel(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", armEDir, "requests.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("empty corpus")
	}
	for _, r := range rows {
		if _, ok := r["label"]; ok {
			t.Fatalf("row %v carries a label field. Ground truth must come only "+
				"from the returned rater files; an authored label in the corpus "+
				"is the defect that withdrew arm D", r["request_id"])
		}
	}
}

// Two votes out of three decide. Three different labels decide nothing, and the
// row is excluded rather than resolved by whoever is running the scorer.
func TestMajorityNeedsTwoOfThreeAndAdmitsDeadlock(t *testing.T) {
	for _, tc := range []struct {
		name  string
		votes []string
		want  string
		ok    bool
	}{
		{"unanimous", []string{labelOut, labelOut, labelOut}, labelOut, true},
		{"two of three", []string{labelIn, labelOut, labelOut}, labelOut, true},
		{"unlabelable can win", []string{labelUnable, labelUnable, labelIn}, labelUnable, true},
		{"three-way split", []string{labelIn, labelOut, labelUnable}, "", false},
		{"single rater", []string{labelOut}, labelOut, true},
		{"two raters split", []string{labelIn, labelOut}, "", false},
	} {
		got, ok := majority(tc.votes)
		if ok != tc.ok {
			t.Errorf("%s: majority exists = %v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A three-way split must not silently become a label. Two raters disagreeing
// must not either -- with an even number of raters there is no majority, and
// inventing one would put the author's thumb on the ground truth.
func TestATieIsNeverResolved(t *testing.T) {
	if _, ok := majority([]string{labelIn, labelOut}); ok {
		t.Fatal("two raters disagreeing produced a majority")
	}
}

func TestWilsonBracketsThePointEstimateAndStaysInRange(t *testing.T) {
	for _, tc := range []struct{ k, n int }{{0, 10}, {10, 10}, {1, 3}, {54, 60}, {17, 36}} {
		lo, hi := wilson(tc.k, tc.n)
		p := float64(tc.k) / float64(tc.n)
		if lo < 0 || hi > 1 {
			t.Errorf("wilson(%d,%d) = [%.3f,%.3f] leaves [0,1]", tc.k, tc.n, lo, hi)
		}
		if p < lo || p > hi {
			t.Errorf("wilson(%d,%d) = [%.3f,%.3f] excludes p=%.3f", tc.k, tc.n, lo, hi, p)
		}
	}
	// The reason this is not a normal approximation: at the boundary that would
	// give a degenerate interval and imply certainty from ten observations.
	if lo, hi := wilson(10, 10); lo >= 1 || hi < 1 {
		t.Errorf("wilson(10,10) = [%.3f,%.3f]; a boundary proportion must still "+
			"carry uncertainty below it", lo, hi)
	}
}

func TestFleissIsOneOnPerfectAgreementAndLowOnNoise(t *testing.T) {
	ids := []string{"a", "b", "c", "d"}
	mk := func(ls ...map[string]string) []*raterFile {
		var out []*raterFile
		for i, l := range ls {
			out = append(out, &raterFile{name: string(rune('r' + i)), labels: l})
		}
		return out
	}
	perfect := map[string]string{"a": labelIn, "b": labelOut, "c": labelIn, "d": labelOut}
	k, n := fleiss(ids, mk(perfect, perfect, perfect))
	if n != 4 || math.Abs(k-1) > 1e-9 {
		t.Errorf("unanimous raters gave kappa %.3f over %d rows, want 1.000 over 4", k, n)
	}

	// Fewer than two raters has no agreement to measure, and must say so rather
	// than returning a number.
	if k, _ := fleiss(ids, mk(perfect)); !math.IsNaN(k) {
		t.Errorf("a single rater produced kappa %.3f; there is no agreement to "+
			"compute and reporting one would imply independence that does not exist", k)
	}
}

// A rater whose file lost a column, gained a row, or had an amount reformatted
// by a spreadsheet must be asked to resend. Repairing it silently would be the
// author editing someone else's judgment.
func TestAReturnedFileThatChangedAnythingElseIsRefused(t *testing.T) {
	dir := t.TempDir()
	canonical := [][]string{
		{"row_id", "intent_text", "intent_payment", "request_payment", "request_amount_paise", "label", "reason"},
		{"E001", "refund the atta, 24000 paise", "PAY-E001", "PAY-E001", "24000", "", ""},
	}
	write := func(name string, rows [][]string) string {
		p := filepath.Join(dir, name)
		var b []byte
		for _, r := range rows {
			for i, f := range r {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, '"')
				b = append(b, []byte(f)...)
				b = append(b, '"')
			}
			b = append(b, '\n')
		}
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	good := [][]string{canonical[0], {"E001", "refund the atta, 24000 paise", "PAY-E001", "PAY-E001", "24000", "in-intent", "exact"}}
	if _, err := loadRater(write("ok.csv", good), canonical); err != nil {
		t.Fatalf("a correctly returned file was refused: %v", err)
	}

	tampered := [][]string{canonical[0], {"E001", "refund the atta, 24000 paise", "PAY-E001", "PAY-E001", "24,000", "in-intent", "exact"}}
	if _, err := loadRater(write("reformatted.csv", tampered), canonical); err == nil {
		t.Error("an amount reformatted by a spreadsheet was accepted")
	}

	missing := [][]string{canonical[0], {"E001", "refund the atta, 24000 paise", "PAY-E001", "PAY-E001", "24000", "", "no idea"}}
	if _, err := loadRater(write("blank.csv", missing), canonical); err == nil {
		t.Error("a row with an empty label was accepted; blank is not `unlabelable`")
	}

	bogus := [][]string{canonical[0], {"E001", "refund the atta, 24000 paise", "PAY-E001", "PAY-E001", "24000", "maybe", "hmm"}}
	if _, err := loadRater(write("bogus.csv", bogus), canonical); err == nil {
		t.Error("an unrecognised label was accepted")
	}
}
