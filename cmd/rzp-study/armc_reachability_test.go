package main

import "testing"

// subsetSize is the whole claim. If it is bounded, or wrong about the smallest
// subset, the README's "nine" becomes a number nothing supports.
func TestSubsetSizeFindsTheSmallestReachingSubset(t *testing.T) {
	for _, tc := range []struct {
		name   string
		acts   []int64
		target int64
		want   int
	}{
		{"exact single action", []int64{24000, 18500}, 24000, 1},
		{"pair", []int64{24000, 18500}, 42500, 2},
		{"prefers the smaller subset", []int64{10, 10, 20}, 20, 1},
		{"unreachable", []int64{24000, 18500}, 30000, 0},
		{"empty action list", nil, 1000, 0},
		{"zero target is not a subset", []int64{5}, 0, 0},
		// The property that matters: this search must NOT stop at eight, or a
		// refusal caused only by the guard's bound would be invisible here --
		// the check would agree with the guard and report nothing.
		{"ten actions, past the guard's bound", []int64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1}, 10, 10},
	} {
		if got := subsetSize(tc.acts, tc.target); got != tc.want {
			t.Errorf("%s: subsetSize = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A refusal reason that stops parsing is a call silently dropped from the
// count, so the command errors rather than under-reporting. These are the
// shapes the guard actually emits.
func TestTheRefusalReasonPatternMatchesWhatTheGuardEmits(t *testing.T) {
	ok := []string{
		"24000 paise is not authorized for pay_SYN9004; actions: G004_01 (exactly 12000 paise on pay_SYN9004)",
		"61500 paise is not authorized for pay_SYN1; actions: a (exactly 1 paise on pay_SYN1), b (exactly 2 paise on pay_SYN1)",
	}
	for _, r := range ok {
		if reRefusal.FindStringSubmatch(r) == nil {
			t.Errorf("a real refusal reason did not parse:\n  %q", r)
		}
	}
	if m := reRefusal.FindStringSubmatch(ok[1]); m != nil {
		if got := len(reExact.FindAllStringSubmatch(m[3], -1)); got != 2 {
			t.Errorf("extracted %d exact amounts from a two-action list, want 2", got)
		}
	}
	bounded := "500 paise is not authorized for pay_X; actions: a (up to 900 paise on pay_X)"
	if !reBounded.MatchString(bounded) {
		t.Error("a bounded action was not detected, so it would be counted as an " +
			"exact-subset case the combining bound did not cause")
	}
}
