package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const template = `{
  "intent_id": "int_cli_001",
  "issued_by": "support@merchant.example",
  "valid_for": "2h",
  "max_calls_per_minute": 6,
  "items": [
    {
      "item_id": "cracked_jar",
      "payment_id": "pay_SYN00000000001",
      "captured_paise": 120000,
      "refund": {"exact_paise": 50000},
      "because": "jar arrived cracked"
    }
  ],
  "total_paise": 50000
}`

// withArgs drives the same entry point main() does. run takes its argv and
// builds its own FlagSet, so two invocations in one test binary cannot leak
// flag state into each other -- which is what makes the second test in a
// package pass for the wrong reason.
func withArgs(t *testing.T, args ...string) error {
	t.Helper()
	return run(args)
}

func writeTemplate(t *testing.T, dir, body string) string {
	t.Helper()
	p := filepath.Join(dir, "intent.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCompileThenVerifyRoundTrips(t *testing.T) {
	dir := t.TempDir()
	in := writeTemplate(t, dir, template)
	out := filepath.Join(dir, "mandate.json")

	if err := withArgs(t, "compile", "-intent", in, "-out", out); err != nil {
		t.Fatalf("compile: %v", err)
	}
	for _, f := range []string{out, filepath.Join(dir, "mandate.intent.json"),
		filepath.Join(dir, "mandate.source.json")} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("compile did not write %s: %v", filepath.Base(f), err)
		}
	}
	if err := withArgs(t, "verify", "-mandate", out); err != nil {
		t.Fatalf("verify of a freshly compiled mandate failed: %v", err)
	}
}

// The realistic way an over-broad grant appears on a machine that has this
// compiler: someone edits the mandate afterwards. verify has to catch it, or
// the coverage record is decoration.
func TestVerifyRefusesAWidenedMandate(t *testing.T) {
	dir := t.TempDir()
	in := writeTemplate(t, dir, template)
	out := filepath.Join(dir, "mandate.json")
	if err := withArgs(t, "compile", "-intent", in, "-out", out); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	widened := strings.Replace(string(raw), `"max_cumulative_paise": 50000`,
		`"max_cumulative_paise": 500000`, 1)
	if widened == string(raw) {
		t.Fatal("the mutation did not apply; the test would pass for the wrong reason")
	}
	if err := os.WriteFile(out, []byte(widened), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := withArgs(t, "verify", "-mandate", out); err == nil {
		t.Fatal("verify accepted a mandate whose cap was raised by hand after compilation")
	}
}

// A mandate on disk may be the one a running guard is enforcing. Overwriting it
// in place changes the file without changing the enforced authority until a
// restart, so the two disagree silently -- worse than either refusing or
// replacing outright.
func TestCompileWillNotOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	in := writeTemplate(t, dir, template)
	out := filepath.Join(dir, "mandate.json")
	if err := withArgs(t, "compile", "-intent", in, "-out", out); err != nil {
		t.Fatal(err)
	}
	if err := withArgs(t, "compile", "-intent", in, "-out", out); err == nil {
		t.Fatal("compile overwrote an existing mandate without -force")
	}
	if err := withArgs(t, "compile", "-intent", in, "-out", out, "-force"); err != nil {
		t.Fatalf("-force did not permit the overwrite: %v", err)
	}
}

// A refusal must name its rule on the way out, because a merchant-side tool
// routes on that string rather than on prose.
func TestAnAmbiguousIntentIsRefusedByRule(t *testing.T) {
	dir := t.TempDir()
	body := strings.Replace(template, `"refund": {"exact_paise": 50000}`, `"refund": {}`, 1)
	in := writeTemplate(t, dir, body)
	err := withArgs(t, "compile", "-intent", in, "-out", filepath.Join(dir, "m.json"))
	if err == nil {
		t.Fatal("an item with no figure compiled")
	}
	if !strings.HasPrefix(err.Error(), "AMBIGUOUS_AMOUNT") {
		t.Errorf("refusal does not lead with its rule: %v", err)
	}
}

// A hand-written mandate has no coverage record, and verify must say so
// plainly rather than reporting a missing file. This is the state every mandate
// in the repository was in before this command existed.
func TestVerifyOnAHandWrittenMandateExplainsWhyItCannot(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hand.json")
	if err := os.WriteFile(p, []byte(`{"mandate_id":"mnd_x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := withArgs(t, "verify", "-mandate", p)
	if err == nil {
		t.Fatal("verify passed a mandate with no coverage record")
	}
	if !strings.Contains(err.Error(), "hand-written") {
		t.Errorf("the message does not explain the situation: %v", err)
	}
}
