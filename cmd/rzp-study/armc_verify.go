package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Verifying a returned rater file against the one that was delivered.
//
// The previous check validated the HEADER and then read only row_id, label and
// reason. That is not evidence the file came from the delivered worksheet, and
// I claimed it was. Two things could pass it:
//
//   a rater or spreadsheet alters intent_text or amount_paise, and the label is
//   still joined to the original row id as though nothing changed;
//
//   row ids arrive duplicated, missing or unknown, and are collapsed or ignored
//   after parsing rather than refused.
//
// So the returned file is now compared field by field against the canonical
// delivered CSV. Only label and reason may differ. Anything else fails closed,
// before agreement and before anything is rendered into a report.

// controlChars rejects anything below space plus DEL. A returned label or
// reason is rendered into a Markdown report, so control characters are both a
// corruption signal and a rendering hazard.
func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func readCSVFile(path string) ([][]string, error) {
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
	return recs, nil
}

// readLabelsCSVVerified fails closed unless the returned file is the delivered
// file with only label and reason filled in.
func readLabelsCSVVerified(returned, canonical string) (map[string]labelledRow, error) {
	can, err := readCSVFile(canonical)
	if err != nil {
		return nil, fmt.Errorf("reading the canonical delivered file: %w", err)
	}
	got, err := readCSVFile(returned)
	if err != nil {
		return nil, err
	}

	for i, want := range armCCSVHeader {
		if got[0][i] != want {
			return nil, fmt.Errorf("%s: column %d is %q, expected %q; columns must not be reordered, renamed or removed", returned, i+1, got[0][i], want)
		}
	}
	if len(got)-1 != len(can)-1 {
		return nil, fmt.Errorf("%s has %d data rows; the delivered file has %d. Rows must not be added or removed", returned, len(got)-1, len(can)-1)
	}

	idx := map[string]int{}
	for i, h := range armCCSVHeader {
		idx[h] = i
	}
	editable := map[int]bool{idx["label"]: true, idx["reason"]: true}

	canon := map[string][]string{}
	for _, r := range can[1:] {
		canon[r[idx["row_id"]]] = r
	}

	out := map[string]labelledRow{}
	var unknown, dup, blank, reordered []string
	for i, r := range got[1:] {
		id := r[idx["row_id"]]
		cr, ok := canon[id]
		if !ok {
			unknown = append(unknown, id)
			continue
		}
		if _, seen := out[id]; seen {
			dup = append(dup, id)
			continue
		}
		// Every non-editable field byte-for-byte.
		for c := range armCCSVHeader {
			if editable[c] {
				continue
			}
			if r[c] != cr[c] {
				return nil, fmt.Errorf("%s row %s: field %q was modified. delivered %q, returned %q. Only label and reason may be edited",
					returned, id, armCCSVHeader[c], cr[c], r[c])
			}
		}
		if i < len(can)-1 && can[i+1][idx["row_id"]] != id {
			reordered = append(reordered, id)
		}

		lb := strings.TrimSpace(r[idx["label"]])
		rs := strings.TrimSpace(r[idx["reason"]])
		if lb == "" {
			blank = append(blank, id)
			continue
		}
		if lb != labelIn && lb != labelOut && lb != "unlabelable" {
			return nil, fmt.Errorf("%s row %s: label %q; expected %s, %s or unlabelable", returned, id, lb, labelIn, labelOut)
		}
		for _, fv := range []struct{ name, val string }{{"label", lb}, {"reason", rs}} {
			if hasControlChars(fv.val) {
				return nil, fmt.Errorf("%s row %s: %s contains a control character", returned, id, fv.name)
			}
			if err := csvSafe(fv.val); err != nil {
				return nil, fmt.Errorf("%s row %s: %s %w", returned, id, fv.name, err)
			}
		}
		out[id] = labelledRow{Key: id, Label: lb, Reason: rs}
	}

	var problems []string
	if len(unknown) > 0 {
		problems = append(problems, fmt.Sprintf("%d unknown row id(s): %s", len(unknown), strings.Join(trim3(unknown), ", ")))
	}
	if len(dup) > 0 {
		problems = append(problems, fmt.Sprintf("%d duplicated row id(s): %s", len(dup), strings.Join(trim3(dup), ", ")))
	}
	if len(blank) > 0 {
		problems = append(problems, fmt.Sprintf("%d unlabelled row(s): %s", len(blank), strings.Join(trim3(blank), ", ")))
	}
	var missing []string
	for id := range canon {
		if _, ok := out[id]; !ok {
			found := false
			for _, b := range blank {
				if b == id {
					found = true
				}
			}
			if !found {
				missing = append(missing, id)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("%d missing row id(s): %s", len(missing), strings.Join(trim3(missing), ", ")))
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s does not match the delivered worksheet: %s", returned, strings.Join(problems, "; "))
	}
	if len(reordered) > 0 {
		fmt.Fprintf(os.Stderr, "note: %s rows are in a different order than delivered; matched by row_id, contents verified\n", returned)
	}
	return out, nil
}

func trim3(s []string) []string {
	sort.Strings(s)
	if len(s) > 3 {
		return s[:3]
	}
	return s
}
