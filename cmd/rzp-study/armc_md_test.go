package main

import (
	"strings"
	"testing"
)

// A rater's `reason` is free text written by someone outside the project and
// rendered into a published document. Rejecting formula prefixes and control
// characters stops a spreadsheet executing it; neither stops it changing what
// the audit appears to say.
func TestMdCodeNeutralisesRaterText(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"heading injection", "x\n## Conclusion: the guard is broken"},
		{"bold and emphasis", "**the audit found no issues**"},
		{"table pipe", "a | b | c"},
		{"list injection", "- item one"},
		{"link", "[click](http://example.invalid)"},
		{"html", "<script>x</script>"},
		{"backtick escape attempt", "` + BT + `code` + BT + ` then **bold**"},
		{"triple backtick fence", "` + BT*3 + `"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := mdCode(tc.in)
			// The rendered form must open and close with a backtick fence, so
			// everything between is literal.
			if !strings.HasPrefix(got, "`") || !strings.HasSuffix(got, "`") {
				t.Fatalf("not wrapped in a code span: %s", got)
			}
			// The fence must be longer than any backtick run in the content,
			// otherwise the text closes the span and escapes.
			fence := got[:strings.IndexFunc(got, func(r rune) bool { return r != '`' })]
			if strings.Contains(tc.in, fence) {
				t.Fatalf("content contains the fence %q and would escape: %s", fence, got)
			}
			// A pipe would still split a table cell even inside a code span.
			if strings.Contains(tc.in, "|") && strings.Contains(got, " |") {
				if !strings.Contains(got, "\\|") {
					t.Fatalf("unescaped pipe survives: %s", got)
				}
			}
		})
	}
}

func TestMdCodeHandlesEmpty(t *testing.T) {
	if got := mdCode(""); !strings.Contains(got, "no reason given") {
		t.Errorf("empty reason rendered as %q", got)
	}
}
