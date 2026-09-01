// Command rzp-arme scores the guard as a verifier on the arm E corpus against
// labels produced by three independent human raters.
//
// GROUND TRUTH COMES FROM THE RETURNED CSVs AND NOWHERE ELSE. study/armE/
// requests.json carries no label field -- see grid_e.py and the test in
// score_test.go that asserts its absence. This program physically cannot branch
// on an authored label, which is the defect that withdrew arm D.
//
// Two modes:
//
//	precheck  runs the guard over the corpus and reports its decisions by cell.
//	          Needs no labels. It exists to answer one question BEFORE anyone is
//	          asked to spend time labelling: are false negatives actually
//	          reachable? Arm D's positives were all constructed as unmatched
//	          amounts, so a default-deny verifier had to refuse every one and
//	          recall 1.000 restated the construction. If precheck reports no
//	          forwarded request that exceeds its intent, the coverage=over cells
//	          did not work and the corpus is not worth a rater's time.
//
//	score     majority-of-three over the returned CSVs, confusion matrix,
//	          precision/recall/FPR with Wilson intervals, Fleiss' kappa.
//
// PRECHECK IS NOT A RESULT AND CANNOT BECOME ONE. It compares the guard's
// decision to `intent_total_paise`, an arithmetic property of the corpus, and
// prints "potential" counts. That is a design check on reachability. The
// arithmetic comparison is exactly the predicate the policy implements, so
// treating it as ground truth would be arm A's defect. Only human labels score.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/policy"
)

type cell struct {
	IntentKind string `json:"intent_kind"`
	Coverage   string `json:"coverage"`
	Request    string `json:"request"`
	Size       string `json:"size"`
}

// request mirrors study/armE/requests.json. There is no Label field, and adding
// one would reintroduce the defect this arm exists to avoid.
type request struct {
	RequestID     string `json:"request_id"`
	Cell          cell   `json:"cell"`
	IntentText    string `json:"intent_text"`
	IntentPayment string `json:"intent_payment"`
	IntentTotal   int64  `json:"intent_total_paise"`
	Ceiling       int64  `json:"mandate_ceiling_paise"`
	CoverageNote  string `json:"coverage_note"`
	ReqPayment    string `json:"request_payment"`
	ReqAmount     int64  `json:"request_amount_paise"`
}

const armEDir = "study/armE"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "rzp-arme:", err)
		os.Exit(1)
	}
}

func run() error {
	mode := "precheck"
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	switch mode {
	case "precheck":
		return precheck()
	case "score":
		return score()
	case "verify":
		return verifyArmE()
	default:
		return fmt.Errorf("unknown mode %q (precheck | score | verify)", mode)
	}
}

func loadCorpus() ([]request, error) {
	b, err := os.ReadFile(filepath.Join(armEDir, "requests.json"))
	if err != nil {
		return nil, err
	}
	var reqs []request
	if err := json.Unmarshal(b, &reqs); err != nil {
		return nil, err
	}
	if len(reqs) == 0 {
		return nil, fmt.Errorf("empty corpus")
	}
	return reqs, nil
}

// decide runs the frozen policy over one request. A fixed clock, and policy.go
// contains no time.Now or rand, so the whole pass is reproducible.
func decide(r request) (bool, string, error) {
	mb, err := os.ReadFile(filepath.Join(armEDir, "mandates", r.RequestID+".json"))
	if err != nil {
		return false, "", err
	}
	m, err := mandate.Load(mb)
	if err != nil {
		return false, "", fmt.Errorf("%s: %w", r.RequestID, err)
	}
	d := policy.New(m).Decide("create_refund", map[string]any{
		"payment_id": r.ReqPayment,
		"amount":     json.Number(fmt.Sprintf("%d", r.ReqAmount)),
	}, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	return d.Allowed, d.Rule, nil
}

func precheck() error {
	reqs, err := loadCorpus()
	if err != nil {
		return err
	}

	var allowed, refused int
	// "Potential" only: the guard forwarded a request larger than the intent
	// names. Whether that is out-of-intent is a human judgment, and three of
	// them will make it. This counts REACHABILITY, not errors.
	var potentialFN, potentialFP []request
	byCell := map[string][2]int{}

	for _, r := range reqs {
		ok, _, err := decide(r)
		if err != nil {
			return err
		}
		if ok {
			allowed++
		} else {
			refused++
		}
		key := r.Cell.Coverage + "/" + r.Cell.Request
		c := byCell[key]
		if ok {
			c[0]++
		} else {
			c[1]++
		}
		byCell[key] = c

		if ok && r.ReqAmount > r.IntentTotal {
			potentialFN = append(potentialFN, r)
		}
		if !ok && r.ReqAmount <= r.IntentTotal {
			potentialFP = append(potentialFP, r)
		}
	}

	fmt.Println("=== arm E precheck (no labels; not a result) ===")
	fmt.Printf("  corpus: %d requests\n", len(reqs))
	fmt.Printf("  guard allowed %d, refused %d\n\n", allowed, refused)

	fmt.Println("  decisions by coverage/request  (allowed / refused)")
	keys := make([]string, 0, len(byCell))
	for k := range byCell {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("    %-28s %2d / %2d\n", k, byCell[k][0], byCell[k][1])
	}

	fmt.Printf("\n  FORWARDED ABOVE THE STATED INTENT: %d\n", len(potentialFN))
	fmt.Println("  These are the rows where a false negative is reachable. If this")
	fmt.Println("  is zero the corpus repeats arm D's tautology and is not worth")
	fmt.Println("  anyone's time.")
	for i, r := range potentialFN {
		if i == 6 {
			fmt.Printf("    ... and %d more\n", len(potentialFN)-6)
			break
		}
		fmt.Printf("    %s  intent %-7d asked %-7d  ceiling %-7d  [%s/%s]\n",
			r.RequestID, r.IntentTotal, r.ReqAmount, r.Ceiling,
			r.Cell.IntentKind, r.Cell.Coverage)
	}

	fmt.Printf("\n  REFUSED AT OR BELOW THE STATED INTENT: %d\n", len(potentialFP))
	fmt.Println("  Rows where a false positive is reachable.")

	if len(potentialFN) == 0 {
		return fmt.Errorf("no request above its intent was forwarded: recall " +
			"would be 1.000 by construction, exactly as in arm D. Do not " +
			"distribute this corpus")
	}
	fmt.Println("\n  Both error directions are reachable. The corpus can measure")
	fmt.Println("  recall and precision rather than restate its own construction.")
	return nil
}
