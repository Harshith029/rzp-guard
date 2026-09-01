package main

import "testing"

// exactlyReachable decides category B from category C, so it decides whether a
// refusal is reported as the corpus's fault or the guard's. It is deliberately
// UNBOUNDED where the guard's own search stops at eight actions -- if it shared
// that bound it would agree with the guard by construction and category C would
// always be empty.
func TestExactlyReachable(t *testing.T) {
	cases := []struct {
		name   string
		target int64
		amts   []int64
		want   bool
	}{
		{"single exact action", 24000, []int64{24000, 18500}, true},
		{"two actions sum exactly", 42500, []int64{24000, 18500}, true},
		{"no subset sums to it", 30000, []int64{24000, 18500}, false},
		{"empty pool", 24000, nil, false},
		{"zero target", 0, []int64{24000}, false},
		// The real finding from arm C: ten actions summing to exactly 108000,
		// which the guard refused because its search stops at eight.
		{"ten actions, beyond the guard's bound", 108000,
			[]int64{12000, 12000, 9250, 9250, 9500, 9500, 16000, 16000, 7250, 7250}, true},
		// A duplicate-amount pool must not be collapsed: two 12000 actions can
		// legitimately fund a 24000 refund.
		{"duplicates are distinct actions", 24000, []int64{12000, 12000}, true},
		{"oversized action ignored", 5000, []int64{99999, 5000}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exactlyReachable(tc.target, tc.amts); got != tc.want {
				t.Errorf("exactlyReachable(%d, %v) = %v, want %v",
					tc.target, tc.amts, got, tc.want)
			}
		})
	}
}

// If this check ever adopted the guard's bound, category C would be empty by
// construction and the audit would report every refusal as the corpus's fault.
func TestReachabilityIsNotBoundedLikeTheGuard(t *testing.T) {
	ten := []int64{12000, 12000, 9250, 9250, 9500, 9500, 16000, 16000, 7250, 7250}
	if !exactlyReachable(108000, ten) {
		t.Fatal("a 10-action exact sum was reported unreachable; the audit would then blame the corpus for a refusal the guard's own bound caused")
	}
}
