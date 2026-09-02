package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// `rzp-study reachability-armC` counts, among arm C's blocked refund calls, how
// many asked for an amount that the merchant's remaining authorizations DO
// cover -- but only by combining more actions than the guard's search allows.
//
// WHY THIS EXISTS. README.md states that the `maxSetSize = 8` bound refused
// NINE refunds in arm C whose entries summed exactly to the requested amount.
// The number is correct. Nothing in the repository computed it.
//
// PROTOCOL-armC-AUDIT.md says the audit report "will list every category C call
// individually" -- future tense, and that report has never run because the
// blocked-call audit has no labels. So the most-read document in the project
// carried a specific measured figure that a reader could not check. That is the
// same defect as an unsourced metric, wearing different clothes.
//
// This needs NO RATER LABELS. Category C is decided by the guard's own recorded
// refusal message, which names the actions still available at that moment --
// more authoritative than reconstructing the mandate, because it already
// accounts for actions consumed earlier in the same trace. Separating it from
// the stalled audit means the maxSetSize evidence stands on its own.
//
// THE REACHABILITY CHECK HERE IS UNBOUNDED, where the guard stops at eight.
// That difference is the entire point: a refusal caused only by the guard
// hitting its bound has to surface, and a bounded check would hide it by
// agreeing with the guard.
//
// Bounded (`up to N paise`) actions are excluded from the subset search. They
// admit a range rather than a figure, so "does a subset sum exactly" is not the
// question those refusals turn on, and counting them here would inflate the
// result with cases the bound did not cause.

var (
	reRefusal = regexp.MustCompile(
		`^(\d+) paise is not authorized for (\S+?); actions: (.*)$`)
	reExact   = regexp.MustCompile(`exactly (\d+) paise`)
	reBounded = regexp.MustCompile(`up to (\d+) paise`)
)

type reachCall struct {
	trace  string
	target int64
	acts   []int64
}

// subsetSize returns the size of the smallest subset summing exactly to target,
// or 0 if none does. Unbounded by design; the action lists here are short.
func subsetSize(acts []int64, target int64) int {
	best := 0
	var walk func(i int, n int, sum int64)
	walk = func(i int, n int, sum int64) {
		if sum == target && n > 0 {
			if best == 0 || n < best {
				best = n
			}
			return
		}
		if i == len(acts) || sum > target {
			return
		}
		walk(i+1, n+1, sum+acts[i])
		walk(i+1, n, sum)
	}
	walk(0, 0, 0)
	return best
}

func collectRefusals(v any, out *[]map[string]any) {
	switch t := v.(type) {
	case map[string]any:
		if t["rule"] == "AMOUNT_NOT_AUTHORIZED" {
			if _, ok := t["reason"].(string); ok {
				*out = append(*out, t)
			}
		}
		for _, e := range t {
			collectRefusals(e, out)
		}
	case []any:
		for _, e := range t {
			collectRefusals(e, out)
		}
	}
}

func cmdArmCReachability(args []string) error {
	dir := filepath.Join(studyDir(), "traces-armC")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var calls []reachCall
	skippedBounded := 0
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return err
		}
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
		var found []map[string]any
		collectRefusals(doc, &found)
		for _, d := range found {
			reason, _ := d["reason"].(string)
			m := reRefusal.FindStringSubmatch(reason)
			if m == nil {
				return fmt.Errorf("%s: a refusal reason did not parse, so this "+
					"count would silently omit it:\n  %q", n, reason)
			}
			if reBounded.MatchString(m[3]) {
				skippedBounded++
				continue
			}
			target, err := strconv.ParseInt(m[1], 10, 64)
			if err != nil {
				return err
			}
			var acts []int64
			for _, a := range reExact.FindAllStringSubmatch(m[3], -1) {
				v, err := strconv.ParseInt(a[1], 10, 64)
				if err != nil {
					return err
				}
				acts = append(acts, v)
			}
			calls = append(calls, reachCall{trace: strings.TrimSuffix(n, ".json"),
				target: target, acts: acts})
		}
	}

	var reachable []reachCall
	sizes := map[int]int{}
	for _, c := range calls {
		if k := subsetSize(c.acts, c.target); k > 0 {
			reachable = append(reachable, c)
			sizes[k]++
		}
	}

	fmt.Println("=== arm C: what the combining bound cost ===")
	fmt.Printf("  blocked refund calls examined:            %d\n", len(calls))
	if skippedBounded > 0 {
		fmt.Printf("  excluded (bounded `up to` actions):       %d\n", skippedBounded)
	}
	fmt.Printf("  amount NOT reachable by ANY subset:       %d\n",
		len(calls)-len(reachable))
	fmt.Printf("  amount reachable by combining:            %d\n\n", len(reachable))

	var ks []int
	for k := range sizes {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	overBound := 0
	fmt.Println("  smallest subset that reaches the amount:")
	for _, k := range ks {
		mark := ""
		if k > 8 {
			mark = "   <- beyond maxSetSize = 8, so the guard refused"
			overBound += sizes[k]
		}
		fmt.Printf("    %2d actions   %d call(s)%s\n", k, sizes[k], mark)
	}

	fmt.Printf("\n  REFUSED ONLY BECAUSE OF THE BOUND: %d\n", overBound)
	fmt.Println("  These are refunds the merchant's remaining authorizations do")
	fmt.Println("  cover. The guard refused them to keep an agent-controlled")
	fmt.Println("  subset-sum search bounded. It is the price of that trade, not a")
	fmt.Println("  verdict on whether 8 is the right number.")

	if overBound > 0 && len(ks) > 0 {
		need := ks[len(ks)-1]
		fmt.Printf("\n  Every one of them needs %d actions. The choice is therefore\n", need)
		fmt.Printf("  8 versus %d on this corpus, not 8 versus unbounded -- raising the\n", need)
		fmt.Printf("  bound to %d recovers all %d and leaves the search finite.\n",
			need, overBound)
		fmt.Println("  Whether that trade is worth making is a design decision this")
		fmt.Println("  command does not take.")
	}
	return nil
}
