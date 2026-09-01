package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A supplementary label set must not be able to become primary, and the
// safeguard must not rest on its filename. The minimal three-column form is
// accepted for supplementary sets and REJECTED by the primary loader, which
// requires the full emitted header -- evidence the file came from the
// worksheet that was actually delivered.
func TestSupplementarySetCannotPassAsPrimary(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sneaky.csv")
	minimal := `row_id,label,reason
A-001,in-intent,looks fine
`
	if err := os.WriteFile(p, []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}

	// The supplementary loader accepts it.
	if _, err := loadSupplementarySet("assistant", p); err != nil {
		t.Fatalf("supplementary loader rejected a valid minimal set: %v", err)
	}

	// The primary loader must not, whatever the file is called. It now also
	// needs a canonical delivered file to verify against, which a supplementary
	// set has no counterpart for -- a second, independent reason it cannot
	// become ground truth.
	canonical := filepath.Join(dir, "canonical.csv")
	canonicalBody := `row_id,intent_text,intent_payment,tool,call_payment,amount_paise,target_status,amount_status,label,reason
A-001,some intent,PAY-aaaa,create_refund,PAY-aaaa,1,present,present,,
`
	if err := os.WriteFile(canonical, []byte(canonicalBody), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readLabelsCSVVerified(p, canonical)
	if err == nil {
		t.Fatal("the PRIMARY loader accepted a three-column supplementary file; renaming one to labels-armC-e1.csv would make it ground truth")
	}
	// Rejected either by the fixed field count or by the header check --
	// two independent barriers, and the test accepts either.
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "field") && !strings.Contains(msg, "column") {
		t.Errorf("rejected for the wrong reason: %v", err)
	}
}

// Only e1 and e2 are primary. Anything else must be detected, not ignored.
func TestPrimaryRatersAreExactlyE1AndE2(t *testing.T) {
	if len(primaryAuditRaters) != 2 {
		t.Fatalf("primary raters = %v, want exactly e1 and e2", primaryAuditRaters)
	}
	for _, r := range []string{"e1", "e2"} {
		if !primaryAuditRaters[r] {
			t.Errorf("%s should be primary", r)
		}
	}
	for _, r := range []string{"assistant", "author", "reviewer", "e3"} {
		if primaryAuditRaters[r] {
			t.Errorf("%s must NOT be primary", r)
		}
	}
}
