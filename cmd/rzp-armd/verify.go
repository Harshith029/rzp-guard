package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/policy"
)

// verify-armd re-decides the arm D corpus IN MEMORY and checks the result
// against what was published, without writing anything.
//
// WHY THIS EXISTS. PROTOCOL-armD.md claimed "the policy is not modified after
// scoring" was "enforced by the harness". It was not. rzp-armd reads whatever
// internal/policy currently contains and merely refuses to overwrite
// RESULTS-armD.md, so the policy could change underneath a published result and
// nothing would notice. That was a control described but never built.
//
// WHAT IT ENFORCES NOW:
//
//	the policy source tree still hashes to the value recorded in
//	study/armD/policy-freeze.json;
//	re-deciding every request reproduces the published confusion matrix exactly.
//
// It never writes, never rescores into a file, and never touches
// RESULTS-armD.md. A mismatch is reported and exits non-zero.
//
// WHAT IT DOES NOT FIX. The published numbers are still scored against
// author-declared labels in requests.json. This command verifies reproducibility
// and policy stability. It does not make the labels independent, and no amount
// of verification will.

type policyFreeze struct {
	Note      string            `json:"note"`
	SHA256    string            `json:"policy_tree_sha256"`
	Files     map[string]string `json:"files"`
	Recorded  string            `json:"recorded_at_utc"`
	Published struct {
		TP int `json:"tp"`
		FP int `json:"fp"`
		TN int `json:"tn"`
		FN int `json:"fn"`
	} `json:"published_matrix"`
}

// policyTreeHash hashes every non-test Go file under internal/policy, sorted, so
// a change anywhere in the decision path moves the value.
func policyTreeHash() (string, map[string]string, error) {
	files := map[string]string{}
	var names []string
	err := filepath.Walk("internal/policy", func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(p)
		files[rel] = fmt.Sprintf("%x", sha256.Sum256(b))
		names = append(names, rel)
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s:%s\n", n, files[n])
	}
	return fmt.Sprintf("%x", h.Sum(nil)), files, nil
}

func verifyArmD() error {
	freezePath := filepath.Join("study", "armD", "policy-freeze.json")
	b, err := os.ReadFile(freezePath)
	if err != nil {
		return fmt.Errorf("reading the recorded policy freeze: %w", err)
	}
	var rec policyFreeze
	if err := json.Unmarshal(b, &rec); err != nil {
		return err
	}

	got, files, err := policyTreeHash()
	if err != nil {
		return err
	}
	fmt.Println("=== arm D verification (read-only) ===")
	fmt.Printf("  recorded policy tree: %s\n", rec.SHA256)
	fmt.Printf("  current  policy tree: %s\n", got)
	if got != rec.SHA256 {
		fmt.Fprintln(os.Stderr, "\nPOLICY CHANGED SINCE SCORING. The published arm D result")
		fmt.Fprintln(os.Stderr, "describes a policy that is no longer in the tree, so it is VOID")
		fmt.Fprintln(os.Stderr, "and must be re-scored from a new corpus. Differing files:")
		for n, h := range files {
			if rec.Files[n] != h {
				fmt.Fprintf(os.Stderr, "  %s\n    recorded %s\n    current  %s\n",
					n, rec.Files[n], h)
			}
		}
		return fmt.Errorf("policy tree hash mismatch")
	}
	fmt.Println("  policy tree unchanged since the result was published")

	// Re-decide in memory. Nothing is written.
	rb, err := os.ReadFile(filepath.Join("study", "armD", "requests.json"))
	if err != nil {
		return err
	}
	var reqs []request
	if err := json.Unmarshal(rb, &reqs); err != nil {
		return err
	}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var tp, fp, tn, fn int
	for _, r := range reqs {
		mb, err := os.ReadFile(filepath.Join("study", "armD", "mandates", r.RequestID+".json"))
		if err != nil {
			return err
		}
		m, err := mandate.Load(mb)
		if err != nil {
			return err
		}
		d := policy.New(m).Decide("create_refund", map[string]any{
			"payment_id": r.ReqPayment,
			"amount":     json.Number(fmt.Sprintf("%d", r.ReqAmount)),
		}, now)
		switch {
		case r.Label == "out-of-intent" && !d.Allowed:
			tp++
		case r.Label == "in-intent" && !d.Allowed:
			fp++
		case r.Label == "out-of-intent" && d.Allowed:
			fn++
		default:
			tn++
		}
	}
	fmt.Printf("  recomputed: TP %d FP %d TN %d FN %d\n", tp, fp, tn, fn)
	fmt.Printf("  published:  TP %d FP %d TN %d FN %d\n",
		rec.Published.TP, rec.Published.FP, rec.Published.TN, rec.Published.FN)
	if tp != rec.Published.TP || fp != rec.Published.FP ||
		tn != rec.Published.TN || fn != rec.Published.FN {
		return fmt.Errorf("the corpus no longer reproduces the published matrix")
	}
	fmt.Println("  reproduces the published matrix exactly")
	fmt.Println()
	fmt.Println("  NOTE: this verifies reproducibility and policy stability only.")
	fmt.Println("  The labels are author-declared; verification does not make them")
	fmt.Println("  independent. See the banner in study/RESULTS-armD.md.")
	return nil
}
