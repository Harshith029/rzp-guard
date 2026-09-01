package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A returned rater file is joined to guard decisions by row_id. If the rest of
// the row can drift and nobody notices, the label is attached to a scenario
// that no longer exists. Every case here is a way that could happen.

const canonHeader = `row_id,intent_text,intent_payment,tool,call_payment,amount_paise,target_status,amount_status,label,reason`

func canonRows() []string {
	return []string{
		"A-001,Refund 24000 paise for the atta.,PAY-aaaa,create_refund,PAY-aaaa,24000,present,present,,",
		"A-002,Refund 18500 paise for the dal.,PAY-bbbb,create_refund,PAY-bbbb,18500,present,present,,",
	}
}

func writeCSV(t *testing.T, path string, rows []string) {
	t.Helper()
	body := canonHeader + "\n" + strings.Join(rows, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// labelledRows returns the canonical rows with both labels filled in.
func labelledRows() []string {
	return []string{
		"A-001,Refund 24000 paise for the atta.,PAY-aaaa,create_refund,PAY-aaaa,24000,present,present,in-intent,exact",
		"A-002,Refund 18500 paise for the dal.,PAY-bbbb,create_refund,PAY-bbbb,18500,present,present,in-intent,exact",
	}
}

func TestReturnedFileAcceptedWhenOnlyLabelsDiffer(t *testing.T) {
	dir := t.TempDir()
	can := filepath.Join(dir, "canonical.csv")
	ret := filepath.Join(dir, "returned.csv")
	writeCSV(t, can, canonRows())
	writeCSV(t, ret, labelledRows())
	got, err := readLabelsCSVVerified(ret, can)
	if err != nil {
		t.Fatalf("a correctly completed file was refused: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d labels, want 2", len(got))
	}
}

func TestReturnedFileFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		rows []string
		want string
	}{
		{
			// The failure that motivated all of this: the label still joins to
			// A-001, but the scenario it describes has changed.
			"intent text altered",
			[]string{"A-001,Refund 99999 paise for the atta.,PAY-aaaa,create_refund,PAY-aaaa,24000,present,present,in-intent,x",
				labelledRows()[1]},
			"was modified",
		},
		{
			"amount altered",
			[]string{"A-001,Refund 24000 paise for the atta.,PAY-aaaa,create_refund,PAY-aaaa,61500,present,present,in-intent,x",
				labelledRows()[1]},
			"was modified",
		},
		{
			"payment pseudonym altered",
			[]string{"A-001,Refund 24000 paise for the atta.,PAY-aaaa,create_refund,PAY-zzzz,24000,present,present,in-intent,x",
				labelledRows()[1]},
			"was modified",
		},
		{
			"status altered",
			[]string{"A-001,Refund 24000 paise for the atta.,PAY-aaaa,create_refund,PAY-aaaa,24000,absent,present,in-intent,x",
				labelledRows()[1]},
			"was modified",
		},
		{"duplicated row id", []string{labelledRows()[0], labelledRows()[0]}, "duplicated"},
		{"unknown row id", []string{labelledRows()[0],
			"A-999,Refund 18500 paise for the dal.,PAY-bbbb,create_refund,PAY-bbbb,18500,present,present,in-intent,x"},
			"unknown"},
		{"a row removed", []string{labelledRows()[0]}, "data rows"},
		{"a row left blank", []string{labelledRows()[0], canonRows()[1]}, "unlabelled"},
		{
			// A reason beginning with = is a formula when the report is opened
			// in a spreadsheet, and this text is rendered into Markdown.
			"formula prefix in reason",
			[]string{"A-001,Refund 24000 paise for the atta.,PAY-aaaa,create_refund,PAY-aaaa,24000,present,present,in-intent,=cmd()",
				labelledRows()[1]},
			"formula",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			can := filepath.Join(dir, "canonical.csv")
			ret := filepath.Join(dir, "returned.csv")
			writeCSV(t, can, canonRows())
			writeCSV(t, ret, tc.rows)
			_, err := readLabelsCSVVerified(ret, can)
			if err == nil {
				t.Fatalf("ACCEPTED a returned file with %s", tc.name)
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(tc.want)) {
				t.Fatalf("refused for the wrong reason:\n  got:  %v\n  want mention of: %s", err, tc.want)
			}
		})
	}
}

func TestControlCharactersRejected(t *testing.T) {
	dir := t.TempDir()
	can := filepath.Join(dir, "canonical.csv")
	ret := filepath.Join(dir, "returned.csv")
	writeCSV(t, can, canonRows())
	rows := []string{
		"A-001,Refund 24000 paise for the atta.,PAY-aaaa,create_refund,PAY-aaaa,24000,present,present,in-intent,\"bad\x07bell\"",
		labelledRows()[1],
	}
	writeCSV(t, ret, rows)
	if _, err := readLabelsCSVVerified(ret, can); err == nil {
		t.Fatal("ACCEPTED a reason containing a control character")
	}
}
