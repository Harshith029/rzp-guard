package main

import (
	"crypto/sha256"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// CSV export for raters.
//
// JSON is the canonical artifact and stays that way -- it is what the tooling
// reads back. But handing a 340-row JSON file to two non-technical volunteers
// and asking them to edit two string fields per row invites exactly the
// mistakes that would waste their afternoon: a lost comma, a mangled quote, a
// file that no longer parses after two hours of work.
//
// The CSV carries EXACTLY the approved rater fields, in the approved order. No
// formulas, no second sheet, no metadata, no hidden columns.

var armCCSVHeader = []string{
	"row_id", "intent_text", "intent_payment", "tool", "call_payment",
	"amount_paise", "target_status", "amount_status", "label", "reason",
}

// formulaLead are the characters a spreadsheet may treat as the start of a
// formula. A CSV that a rater opens in Excel is an execution context, so a
// field starting with one of these is refused rather than quietly rewritten:
// silently altering a rater's input would be worse than stopping.
// 9 is tab and 13 is carriage return; written as runes so no escape can be
// mangled by the tooling that generates this file.
var formulaLead = string([]rune{'=', '+', '-', '@', 9, 13})

func csvSafe(field string) error {
	if field == "" {
		return nil
	}
	if strings.ContainsRune(formulaLead, rune(field[0])) {
		return fmt.Errorf("field %q begins with %q, which a spreadsheet may treat as a formula", field, string(field[0]))
	}
	return nil
}

// writeArmCCSV writes one rater's CSV and returns its SHA-256.
func writeArmCCSV(path string, rows []armCRow) (string, error) {
	var b strings.Builder
	cw := csv.NewWriter(&b)
	if err := cw.Write(armCCSVHeader); err != nil {
		return "", err
	}
	for _, r := range rows {
		rec := []string{
			r.RowID, r.IntentText, r.IntentPayment, r.Tool, r.CallPayment,
			strconv.FormatInt(r.AmountPaise, 10), r.TargetStatus, r.AmountStatus,
			"", "",
		}
		for _, f := range rec {
			if err := csvSafe(f); err != nil {
				return "", fmt.Errorf("row %s: %w", r.RowID, err)
			}
		}
		if err := cw.Write(rec); err != nil {
			return "", err
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return "", err
	}
	data := []byte(b.String())
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

// writeDeliverySums records a hash for every file a rater receives, so a
// returned file can be checked against what was sent.
func writeDeliverySums(dir string, sums map[string]string) error {
	var names []string
	for n := range sums {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	fmt.Fprintf(&b, "# SHA-256 of every arm C file delivered to a rater.\n")
	fmt.Fprintf(&b, "# The CSV is the working copy; the JSON is canonical.\n")
	fmt.Fprintf(&b, "# The three CSVs are byte-identical BY DESIGN -- every rater\n")
	fmt.Fprintf(&b, "# must see exactly the same rows. Attribution is by filename,\n")
	fmt.Fprintf(&b, "# not by hash. A returned file that still matches its hash\n")
	fmt.Fprintf(&b, "# here has not been labelled at all.\n\n")
	for _, n := range names {
		fmt.Fprintf(&b, "%s  %s\n", sums[n], n)
	}
	return os.WriteFile(filepath.Join(dir, "SHA256SUMS-armC.txt"), []byte(b.String()), 0o644)
}

// readLabelsCSV parses a rater's returned CSV.
//
// Raters work in the CSV, so this is the path their labels actually arrive by.
// It validates the header against the emitted one rather than trusting column
// order: a spreadsheet that reorders or drops a column would otherwise be read
// as though nothing had happened.
func readLabelsCSV(path string) (map[string]labelledRow, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rd := csv.NewReader(f)
	rd.FieldsPerRecord = len(armCCSVHeader)
	recs, err := rd.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(recs) < 2 {
		return nil, fmt.Errorf("%s: no data rows", path)
	}
	for i, want := range armCCSVHeader {
		if recs[0][i] != want {
			return nil, fmt.Errorf("%s: column %d is %q, expected %q; the sheet's "+
				"columns must not be reordered, renamed or removed",
				path, i+1, recs[0][i], want)
		}
	}

	idx := map[string]int{}
	for i, h := range armCCSVHeader {
		idx[h] = i
	}
	out := map[string]labelledRow{}
	var blank []string
	for _, r := range recs[1:] {
		id := strings.TrimSpace(r[idx["row_id"]])
		lb := strings.TrimSpace(r[idx["label"]])
		if lb == "" {
			blank = append(blank, id)
			continue
		}
		if lb != labelIn && lb != labelOut && lb != "unlabelable" {
			return nil, fmt.Errorf("%s: row %s has label %q; expected %s, %s or unlabelable",
				path, id, lb, labelIn, labelOut)
		}
		out[id] = labelledRow{Key: id, Label: lb,
			Reason: strings.TrimSpace(r[idx["reason"]])}
	}
	if len(blank) > 0 {
		n := len(blank)
		if n > 3 {
			blank = blank[:3]
		}
		return nil, fmt.Errorf("%s: %d rows are unlabelled (e.g. %s); agreement "+
			"cannot be computed on a partial sheet", path, n, strings.Join(blank, ", "))
	}
	return out, nil
}
