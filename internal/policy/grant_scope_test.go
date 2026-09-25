package policy

import (
	"sort"
	"strings"
	"testing"

	"github.com/harshith/rzp-guard/internal/storage"
)

// The storage layer decides which refusals an operator may approve; this
// package decides which ones a grant can actually override. They are two lists
// in two packages, and they must be the same list.
//
// If storage allows one this package does not override, an operator's approval
// does nothing except leave a live grant that a LATER refusal of an overridable
// kind can spend -- FAILURES.md F53. If this package overrides one storage will
// not grant, a refusal a human should be able to correct cannot be.
func TestGrantableRulesMatchWhatTheGuardOverrides(t *testing.T) {
	var overridable []string
	for r := range overridableRules {
		overridable = append(overridable, r)
	}
	sort.Strings(overridable)
	grantable := storage.GrantableRules()

	if strings.Join(overridable, ",") != strings.Join(grantable, ",") {
		t.Fatalf("the guard overrides %v but storage will grant %v", overridable,
			grantable)
	}
}
