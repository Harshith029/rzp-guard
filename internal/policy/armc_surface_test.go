package policy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/harshith/rzp-guard/internal/mandate"
)

// What the argument surface costs on the only real agent traffic this project
// has, computed by the real rule rather than restated in prose.
//
// Arm C's committed traces keep every argument the model sent, including the
// three its pre-registered projection hides from raters. Run over them, the
// default-deny surface refuses exactly one create_refund in 340 -- and that one
// is the behaviour the rule exists for: an unprompted speed="optimum", which
// the guard of the time forwarded. PROTOCOL-armE-AMENDMENT-4.md quotes these
// numbers; this is where they come from.
//
// The traces are frozen study evidence, so the counts are exact. If this fails
// after a trace changed, the amendment is describing data that no longer
// exists and must be revisited, not the number edited to match.
func TestArmCTrafficUnderTheArgumentSurface(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "study", "traces-armC", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no arm C traces found (%v); the amendment's numbers have no source", err)
	}

	var total, withNotes, withReceipt int
	var refused []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var trace struct {
			ToolCalls []struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(b, &trace); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, c := range trace.ToolCalls {
			if c.Name != RefundTool {
				continue
			}
			total++
			// Decoded exactly as the relay decodes a tools/call off the wire.
			dec := json.NewDecoder(strings.NewReader(c.Arguments))
			dec.UseNumber()
			var args map[string]any
			if err := dec.Decode(&args); err != nil {
				t.Fatalf("%s: arguments do not parse: %v", f, err)
			}
			if _, ok := args["notes"]; ok {
				withNotes++
			}
			if _, ok := args["receipt"]; ok {
				withReceipt++
			}
			if _, _, err := vettedRefundArgs(args, mandate.Limits{}); err != nil {
				refused = append(refused, filepath.Base(f)+": "+err.Error())
			}
		}
	}
	sort.Strings(refused)

	if total != 340 {
		t.Fatalf("%d create_refund calls in arm C, the amendment says 340", total)
	}
	// Why notes pass and receipt is discarded rather than refused: every real
	// call carried both. Refusing either would refuse all 340.
	if withNotes != total || withReceipt != total {
		t.Fatalf("notes on %d and receipt on %d of %d calls; the amendment says all",
			withNotes, withReceipt, total)
	}
	if len(refused) != 1 || !strings.HasPrefix(refused[0], "G020_run1.json") ||
		!strings.Contains(refused[0], `"optimum"`) {
		t.Fatalf("refused %d of %d, want exactly G020_run1's speed=optimum:\n  %s",
			len(refused), total, strings.Join(refused, "\n  "))
	}
}
