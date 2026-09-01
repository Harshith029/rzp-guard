package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Arm E's rater instrument goes through the same gate as arm C's. The scan is
// per-arm only in which file it reads; the word list and the refusal are shared,
// so a new arm cannot quietly ship an instrument that names the component under
// test.
func TestArmEInstrumentIsRaterSafe(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "study", "RATER-INSTRUCTIONS-armE.md"))
	if err != nil {
		t.Fatal(err)
	}
	if hits := scanForbiddenContext(string(b)); len(hits) > 0 {
		t.Fatalf("the arm E instrument contains forbidden context: %v. A rater "+
			"who reads any of these is no longer blind to what the rows have in "+
			"common", hits)
	}
}
