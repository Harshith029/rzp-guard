package main

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Supplementary label sets.
//
// Only e1 and e2 are ground truth for the blocked-call audit. Anything else --
// an assistant reviewer's sanity check, a second pass by the author, a
// spot-check by someone who has read the repository -- is supplementary, and the
// distinction has to be ENFORCED rather than left to a filename not matching.
//
// The failure mode this prevents is quiet: a well-meant extra label file dropped
// into the adjudication directory, picked up because it happened to be named
// conveniently, and silently becoming a kappa input. Agreement between an
// informed reviewer and an external rater is not inter-rater agreement, and
// publishing it as such would be a false claim of independence.
//
// So: any audit label file whose rater is not e1 or e2 is detected, reported by
// name, and excluded from ground truth and from every agreement statistic.

var primaryAuditRaters = map[string]bool{"e1": true, "e2": true}

type supplementarySet struct {
	Rater  string
	Path   string
	Counts map[string]int
	Rows   map[string]string // row_id -> label
}

// findSupplementaryAuditSets returns every audit label file that is NOT a
// primary rater's.
func findSupplementaryAuditSets() ([]supplementarySet, error) {
	dir := filepath.Join(studyDir(), "adjudication")
	paths, err := filepath.Glob(filepath.Join(dir, "audit-labels-armC-*"))
	if err != nil {
		return nil, err
	}
	var out []supplementarySet
	for _, p := range paths {
		base := filepath.Base(p)
		name := strings.TrimSuffix(strings.TrimSuffix(base, ".csv"), ".json")
		rater := strings.TrimPrefix(name, "audit-labels-armC-")
		if primaryAuditRaters[rater] {
			continue
		}
		s, err := loadSupplementarySet(rater, p)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rater < out[j].Rater })
	return out, nil
}

// loadSupplementarySet accepts a MINIMAL form -- row_id, label, reason -- which
// the primary loader deliberately rejects.
//
// The asymmetry is the safeguard. A primary label file must carry the full
// emitted header, which is evidence it came from the worksheet that was actually
// delivered. A supplementary set has no such requirement precisely because it is
// never ground truth, and accepting a looser format here makes it impossible for
// one to satisfy the primary path by accident.
func loadSupplementarySet(rater, path string) (supplementarySet, error) {
	s := supplementarySet{Rater: rater, Path: path,
		Counts: map[string]int{}, Rows: map[string]string{}}
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	if len(recs) < 2 {
		return s, fmt.Errorf("%s: no data rows", path)
	}
	idx := map[string]int{}
	for i, h := range recs[0] {
		idx[strings.TrimSpace(h)] = i
	}
	for _, k := range []string{"row_id", "label"} {
		if _, ok := idx[k]; !ok {
			return s, fmt.Errorf("%s: supplementary sets need at least row_id and "+
				"label columns; got %v", path, recs[0])
		}
	}
	for _, r := range recs[1:] {
		if len(r) <= idx["label"] {
			continue
		}
		id := strings.TrimSpace(r[idx["row_id"]])
		lb := strings.TrimSpace(r[idx["label"]])
		s.Rows[id] = lb
		s.Counts[lb]++
	}
	return s, nil
}

// reportSupplementary renders supplementary sets into the audit document, kept
// away from every ground-truth quantity.
func reportSupplementary(p func(string, ...any), sets []supplementarySet,
	agreedIn, agreedOut map[string]bool) {
	if len(sets) == 0 {
		return
	}
	p("---\n\n## Supplementary label sets — NOT ground truth, NOT a kappa input\n\n")
	p("These were provided outside the two-external-rater design. They are\n")
	p("recorded because discarding them would be worse, and excluded from every\n")
	p("quantity above because including them would be a false claim of\n")
	p("independence: none of these labellers is an external rater blind to the\n")
	p("implementation.\n\n")
	p("They contribute to **no** ground truth, **no** agreement statistic, and\n")
	p("**no** bound. Nothing in this section may be quoted as inter-rater\n")
	p("agreement.\n\n")
	for _, s := range sets {
		p("### `%s`\n\n", filepath.Base(s.Path))
		p("| label | n |\n|---|---|\n")
		var ks []string
		for k := range s.Counts {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			p("| %s | %d |\n", k, s.Counts[k])
		}
		p("\n")
		// A concordance, offered only as a sanity signal.
		if len(agreedIn)+len(agreedOut) > 0 {
			match, compared := 0, 0
			for id, lb := range s.Rows {
				want := ""
				if agreedIn[id] {
					want = labelIn
				} else if agreedOut[id] {
					want = labelOut
				} else {
					continue
				}
				compared++
				if lb == want {
					match++
				}
			}
			if compared > 0 {
				p("Concordance with the rows both external raters agreed on: "+
					"**%d / %d**. This is a sanity signal only — an informed "+
					"reviewer matching uninformed raters says nothing about "+
					"whether the ground truth is independent.\n\n", match, compared)
			}
		}
	}
}
