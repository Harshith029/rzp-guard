package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/harshith/rzp-guard/internal/mandate"
	"github.com/harshith/rzp-guard/internal/policy"
)

// `rzp-armd verify` re-decides the arm D corpus IN MEMORY and checks everything
// the published numbers depend on. It never writes.
//
// WHY IT EXISTS. PROTOCOL-armD.md claimed "the policy is not modified after
// scoring" was "enforced by the harness". Nothing enforced it. That was a
// control described but never built.
//
// WHY IT IS WIDER THAN ITS FIRST VERSION. The first version hashed only
// internal/policy. The score also runs internal/mandate through mandate.Load,
// and mandate is not the only other package: `go list -deps ./cmd/rzp-armd`
// gives the whole closure, and internal/lifecycle and internal/opauth are in it
// too. A change in any of them can move a decision, so the manifest covers all
// of them plus the scorer itself. manifest_test.go recomputes that closure from
// the source and fails if the manifest stops covering it, so this cannot
// silently narrow again.
//
// internal/storage is NOT in the closure and is not hashed. The scorer calls
// policy.New, whose persister is nil, so no storage code is reachable. That is
// checkable with the go list command above rather than taken on trust, which
// matters here: a bounded-retry fix landed in internal/storage on the same day
// as this file, and leaving the reason for its exclusion unstated would be
// convenient rather than honest.
//
// WHAT IT CHECKS
//
//	the decision-path source still hashes to what the manifest recorded;
//	the corpus, the 90 mandates and the corpus generators still hash likewise;
//	re-deciding all 90 requests reproduces the published matrix;
//	that matrix actually appears in the generated conformance report AND in the
//	  preserved body of the retracted RESULTS-armD.md;
//	the preserved bodies of the three retracted documents are unedited.
//
// WHAT IT DOES NOT FIX. The numbers are scored against author-declared labels
// in requests.json. This proves reproducibility and stability. It does not make
// the labels independent, and no amount of verification will.

// preservedMarker separates a retraction notice from the artifact it retracts.
// Everything AFTER this line is the original document and must not change.
const preservedMarker = "<!-- PRESERVED-ORIGINAL-BELOW -->"

const manifestPath = "study/armD/manifest.json"

type preservedDoc struct {
	Path       string `json:"path"`
	FromCommit string `json:"preserved_from_commit"`
	GitBlob    string `json:"preserved_git_blob"`
	BodySHA256 string `json:"preserved_body_sha256"`
}

// supersededPath is one decision-path hash that a re-record replaced, with the
// reason and the matrix that was verified at the time. A re-record that cannot
// reproduce the published matrix is refused outright.
type supersededPath struct {
	TreeSHA256   string `json:"tree_sha256"`
	RecordedAt   string `json:"recorded_at_utc"`
	SupersededAt string `json:"superseded_at_utc"`
	Reason       string `json:"reason"`
	MatrixHeld   bool   `json:"published_matrix_still_reproduced"`
}

type armDManifest struct {
	Note         string `json:"note"`
	RecordedAt   string `json:"recorded_at_utc"`
	DecisionPath struct {
		Note       string            `json:"note"`
		Packages   []string          `json:"packages"`
		TreeSHA256 string            `json:"tree_sha256"`
		Files      map[string]string `json:"files"`
	} `json:"decision_path"`
	Corpus struct {
		RequestsSHA256     string            `json:"requests_sha256"`
		MandateCount       int               `json:"mandate_count"`
		MandatesTreeSHA256 string            `json:"mandates_tree_sha256"`
		Generators         map[string]string `json:"generators"`
	} `json:"corpus"`
	Report struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"generated_report"`
	Preserved []preservedDoc `json:"preserved_documents"`
	// PriorPaths records every decision-path hash this manifest has superseded.
	// Re-recording after a source change would otherwise erase the fact that
	// the freeze was ever broken, leaving "unchanged since scoring" true of
	// the newest stamp and silent about the ones before it.
	PriorPaths      []supersededPath `json:"superseded_decision_paths,omitempty"`
	PublishedMatrix struct {
		TP int `json:"tp"`
		FP int `json:"fp"`
		TN int `json:"tn"`
		FN int `json:"fn"`
	} `json:"published_matrix"`
}

// decisionPathDirs is the import closure of the scorer, from
// `go list -deps ./cmd/rzp-armd`. manifest_test.go proves it is still complete.
var decisionPathDirs = []string{
	"internal/lifecycle",
	"internal/mandate",
	"internal/opauth",
	"internal/policy",
}

// scorerFiles are the scorer's own sources. main.go decides every outcome -- it
// is the file that branches on r.Label -- so it belongs in the hash. verify.go
// and the tests do not: the checker is not part of the evaluation, and hashing
// it would leave the manifest verifying itself.
var scorerFiles = []string{"cmd/rzp-armd/main.go"}

// corpusGenerators built the requests and compiled the mandates. A change here
// changes the data even when every hash of the data is recomputed alongside it.
var corpusGenerators = []string{
	"study/grid.py",
	"study/grid_d.py",
	"study/compile_mandate.py",
}

func sha256File(p string) (string, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}

// hashSet folds a name-to-hash map into one value, sorted, so a rename or a
// deletion moves it as surely as an edit does.
func hashSet(files map[string]string) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s:%s\n", n, files[n])
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func decisionPathHashes() (string, map[string]string, error) {
	files := map[string]string{}
	for _, dir := range decisionPathDirs {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return "", nil, err
		}
		for _, e := range ents {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			p := filepath.ToSlash(filepath.Join(dir, n))
			h, err := sha256File(p)
			if err != nil {
				return "", nil, err
			}
			files[p] = h
		}
	}
	for _, p := range scorerFiles {
		h, err := sha256File(p)
		if err != nil {
			return "", nil, err
		}
		files[p] = h
	}
	return hashSet(files), files, nil
}

func mandateHashes() (string, int, error) {
	dir := filepath.Join("study", "armD", "mandates")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", 0, err
	}
	files := map[string]string{}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.ToSlash(filepath.Join(dir, e.Name()))
		h, err := sha256File(p)
		if err != nil {
			return "", 0, err
		}
		files[p] = h
	}
	return hashSet(files), len(files), nil
}

func generatorHashes() (map[string]string, error) {
	out := map[string]string{}
	for _, p := range corpusGenerators {
		h, err := sha256File(p)
		if err != nil {
			return nil, err
		}
		out[p] = h
	}
	return out, nil
}

// preservedBody returns the retracted document's original text: everything
// after the marker line. A file without the marker is an error, not a pass --
// deleting the notice must not become a way to skip the check.
func preservedBody(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	i := strings.Index(string(b), preservedMarker)
	if i < 0 {
		return nil, fmt.Errorf("%s: the %s marker is gone, so the retraction "+
			"notice has been removed or rewritten", path, preservedMarker)
	}
	rest := string(b[i+len(preservedMarker):])
	rest = strings.TrimPrefix(rest, "\r")
	rest = strings.TrimPrefix(rest, "\n")
	return []byte(rest), nil
}

var (
	reOutRow = regexp.MustCompile(`\|\s*\*\*out-of-intent\*\*\s*\|\s*TP\s+(\d+)\s*\|\s*FN\s+(\d+)\s*\|`)
	reInRow  = regexp.MustCompile(`\|\s*\*\*in-intent\*\*\s*\|\s*FP\s+(\d+)\s*\|\s*TN\s+(\d+)\s*\|`)
)

type matrix struct{ TP, FP, TN, FN int }

// matrixIn reads the confusion matrix a document actually PRINTS. Hashing a
// document proves it has not changed; this proves it says what the manifest
// says it says. The first version of this command checked neither.
func matrixIn(what string, text []byte) (matrix, error) {
	var m matrix
	o := reOutRow.FindSubmatch(text)
	i := reInRow.FindSubmatch(text)
	if o == nil || i == nil {
		return m, fmt.Errorf("%s: no confusion-matrix table found", what)
	}
	m.TP, _ = strconv.Atoi(string(o[1]))
	m.FN, _ = strconv.Atoi(string(o[2]))
	m.FP, _ = strconv.Atoi(string(i[1]))
	m.TN, _ = strconv.Atoi(string(i[2]))
	return m, nil
}

// scoreCorpus re-decides every request in memory. Nothing is written.
func scoreCorpus() (matrix, error) {
	var m matrix
	rb, err := os.ReadFile(filepath.Join("study", "armD", "requests.json"))
	if err != nil {
		return m, err
	}
	var reqs []request
	if err := json.Unmarshal(rb, &reqs); err != nil {
		return m, err
	}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	for _, r := range reqs {
		mb, err := os.ReadFile(filepath.Join("study", "armD", "mandates", r.RequestID+".json"))
		if err != nil {
			return m, err
		}
		md, err := mandate.Load(mb)
		if err != nil {
			return m, err
		}
		d := policy.New(md).Decide("create_refund", map[string]any{
			"payment_id": r.ReqPayment,
			"amount":     json.Number(fmt.Sprintf("%d", r.ReqAmount)),
		}, now)
		// Branches on r.Label, exactly as main.go does. Deriving a label here
		// would check something other than what was published.
		switch {
		case r.Label == "out-of-intent" && !d.Allowed:
			m.TP++
		case r.Label == "in-intent" && !d.Allowed:
			m.FP++
		case r.Label == "out-of-intent" && d.Allowed:
			m.FN++
		default:
			m.TN++
		}
	}
	return m, nil
}

// recordManifest writes study/armD/manifest.json and REFUSES to overwrite one.
// Re-recording after a change therefore requires deleting the file first, which
// shows up in the diff. That is the only thing between this and a manifest that
// can be relaundered silently, and it is worth saying plainly: git history, not
// this program, is what makes the record trustworthy.
// recordManifest writes study/armD/manifest.json.
//
// Without -supersede it refuses to overwrite an existing manifest. With it, the
// old decision-path hash is carried forward into superseded_decision_paths with
// the reason and the date, and the re-record is REFUSED unless the corpus still
// reproduces the published matrix. That is the difference between "the code
// changed and the result is unaffected, here is the trail" and "the stamp was
// quietly reapplied".
func recordManifest() error {
	supersede := ""
	if len(os.Args) > 2 && os.Args[2] == "-supersede" {
		if len(os.Args) < 4 || strings.TrimSpace(os.Args[3]) == "" {
			return fmt.Errorf("-supersede needs a reason: what changed in the " +
				"decision path, and why the published result survives it")
		}
		supersede = os.Args[3]
	}
	var prior []supersededPath
	if b, err := os.ReadFile(manifestPath); err == nil {
		if supersede == "" {
			return fmt.Errorf("refusing to overwrite %s. If the decision path changed "+
				"and the result still holds, re-record with:\n  rzp-armd manifest "+
				"-supersede \"what changed and why the result survives\"", manifestPath)
		}
		var old armDManifest
		if err := json.Unmarshal(b, &old); err != nil {
			return err
		}
		m, err := scoreCorpus()
		if err != nil {
			return err
		}
		held := m.TP == old.PublishedMatrix.TP && m.FP == old.PublishedMatrix.FP &&
			m.TN == old.PublishedMatrix.TN && m.FN == old.PublishedMatrix.FN
		if !held {
			return fmt.Errorf("REFUSING to supersede: the corpus no longer reproduces "+
				"the published matrix (was TP %d FP %d TN %d FN %d, now TP %d FP %d "+
				"TN %d FN %d). This is not a re-record, it is a different result, and "+
				"it needs a new corpus rather than a new stamp",
				old.PublishedMatrix.TP, old.PublishedMatrix.FP, old.PublishedMatrix.TN,
				old.PublishedMatrix.FN, m.TP, m.FP, m.TN, m.FN)
		}
		prior = append(old.PriorPaths, supersededPath{
			TreeSHA256:   old.DecisionPath.TreeSHA256,
			RecordedAt:   old.RecordedAt,
			SupersededAt: time.Now().UTC().Format(time.RFC3339),
			Reason:       supersede,
			MatrixHeld:   true,
		})
	}
	var mf armDManifest
	mf.PriorPaths = prior
	mf.Note = "Everything the published arm D numbers depend on. `rzp-armd verify` " +
		"checks all of it and writes nothing. None of this makes the labels " +
		"independent; see study/ASSESSMENT-armD.md."
	mf.RecordedAt = time.Now().UTC().Format(time.RFC3339)
	mf.DecisionPath.Note = "Non-test sources of the scorer's import closure " +
		"(`go list -deps ./cmd/rzp-armd`), plus cmd/rzp-armd/main.go. " +
		"internal/storage is absent because it is not in that closure: the " +
		"scorer's persister is nil."
	mf.DecisionPath.Packages = append(append([]string{}, decisionPathDirs...), "cmd/rzp-armd")

	tree, files, err := decisionPathHashes()
	if err != nil {
		return err
	}
	mf.DecisionPath.TreeSHA256, mf.DecisionPath.Files = tree, files

	if mf.Corpus.RequestsSHA256, err = sha256File("study/armD/requests.json"); err != nil {
		return err
	}
	if mf.Corpus.MandatesTreeSHA256, mf.Corpus.MandateCount, err = mandateHashes(); err != nil {
		return err
	}
	if mf.Corpus.Generators, err = generatorHashes(); err != nil {
		return err
	}

	mf.Report.Path = "study/armD/CONFORMANCE-armD.md"
	if mf.Report.SHA256, err = sha256File(mf.Report.Path); err != nil {
		return err
	}

	for _, d := range []preservedDoc{
		{Path: "study/RESULTS-armD.md", FromCommit: "f87c86b",
			GitBlob: "5fa0bc39be271ddc9fb2dee1ec90fdcac478cb75"},
		{Path: "study/PROTOCOL-armD.md", FromCommit: "ca1e4c1",
			GitBlob: "db57fd5902f085d95c9ea1f7681b766b9c885af6"},
		{Path: "study/FINDINGS-armD.md", FromCommit: "f87c86b",
			GitBlob: "49ec6ae34ac78f84ac445b6fafc3a7be9780af4e"},
	} {
		body, err := preservedBody(d.Path)
		if err != nil {
			return err
		}
		d.BodySHA256 = fmt.Sprintf("%x", sha256.Sum256(body))
		mf.Preserved = append(mf.Preserved, d)
	}

	m, err := scoreCorpus()
	if err != nil {
		return err
	}
	mf.PublishedMatrix.TP, mf.PublishedMatrix.FP = m.TP, m.FP
	mf.PublishedMatrix.TN, mf.PublishedMatrix.FN = m.TN, m.FN

	out, err := json.MarshalIndent(mf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(manifestPath, append(out, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("recorded %s\n", manifestPath)
	fmt.Printf("  decision path: %d files, tree %s\n", len(files), tree[:16])
	fmt.Printf("  matrix: TP %d FP %d TN %d FN %d\n", m.TP, m.FP, m.TN, m.FN)
	return nil
}

func verifyArmD() error {
	b, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("reading %s: %w", manifestPath, err)
	}
	var mf armDManifest
	if err := json.Unmarshal(b, &mf); err != nil {
		return err
	}
	fmt.Println("=== arm D verification (read-only) ===")

	var bad []string
	fail := func(f string, v ...any) { bad = append(bad, fmt.Sprintf(f, v...)) }
	pass := func(f string, v ...any) { fmt.Printf("  OK   "+f+"\n", v...) }

	// 1. The decision path: every non-test source the score executes.
	tree, files, err := decisionPathHashes()
	if err != nil {
		return err
	}
	if tree != mf.DecisionPath.TreeSHA256 {
		fail("DECISION PATH CHANGED. The published result describes code that is "+
			"no longer in the tree, so it is VOID and must be re-scored.\n"+
			"       recorded %s\n       current  %s", mf.DecisionPath.TreeSHA256, tree)
		for n, h := range files {
			if mf.DecisionPath.Files[n] != h {
				fail("       differs: %s", n)
			}
		}
		for n := range mf.DecisionPath.Files {
			if _, ok := files[n]; !ok {
				fail("       missing: %s", n)
			}
		}
	} else {
		pass("decision path unchanged (%d files across %s)",
			len(files), strings.Join(mf.DecisionPath.Packages, ", "))
	}

	// 2. The corpus, the compiled mandates and the generators that made them.
	if h, err := sha256File("study/armD/requests.json"); err != nil {
		return err
	} else if h != mf.Corpus.RequestsSHA256 {
		fail("CORPUS CHANGED: study/armD/requests.json no longer matches the manifest")
	} else {
		pass("corpus unchanged (requests.json)")
	}
	mtree, mcount, err := mandateHashes()
	if err != nil {
		return err
	}
	switch {
	case mcount != mf.Corpus.MandateCount:
		fail("MANDATE COUNT CHANGED: %d on disk, %d recorded", mcount, mf.Corpus.MandateCount)
	case mtree != mf.Corpus.MandatesTreeSHA256:
		fail("MANDATES CHANGED: the compiled mandates no longer match the manifest")
	default:
		pass("all %d compiled mandates unchanged", mcount)
	}
	gens, err := generatorHashes()
	if err != nil {
		return err
	}
	genBad := false
	for n, h := range gens {
		if mf.Corpus.Generators[n] != h {
			fail("CORPUS GENERATOR CHANGED: %s", n)
			genBad = true
		}
	}
	if !genBad {
		pass("corpus generators unchanged (%d files)", len(gens))
	}

	// 3. Re-decide the whole corpus in memory.
	got, err := scoreCorpus()
	if err != nil {
		return err
	}
	want := matrix{mf.PublishedMatrix.TP, mf.PublishedMatrix.FP,
		mf.PublishedMatrix.TN, mf.PublishedMatrix.FN}
	if got != want {
		fail("RE-SCORING DISAGREES: recomputed TP %d FP %d TN %d FN %d, manifest "+
			"records TP %d FP %d TN %d FN %d",
			got.TP, got.FP, got.TN, got.FN, want.TP, want.FP, want.TN, want.FN)
	} else {
		pass("re-deciding all %d requests reproduces TP %d FP %d TN %d FN %d",
			got.TP+got.FP+got.TN+got.FN, got.TP, got.FP, got.TN, got.FN)
	}

	// 4. The artifacts must SAY that matrix, not merely hash to something. The
	//    first version of this command verified a matrix stored in a JSON file
	//    it had written itself, which is not the same claim at all.
	rb, err := os.ReadFile(mf.Report.Path)
	if err != nil {
		return err
	}
	if h := fmt.Sprintf("%x", sha256.Sum256(rb)); h != mf.Report.SHA256 {
		fail("GENERATED REPORT CHANGED: %s no longer matches the manifest", mf.Report.Path)
	} else if rm, err := matrixIn(mf.Report.Path, rb); err != nil {
		fail("%v", err)
	} else if rm != want {
		fail("%s PRINTS TP %d FP %d TN %d FN %d, not the manifest's matrix",
			mf.Report.Path, rm.TP, rm.FP, rm.TN, rm.FN)
	} else {
		pass("%s unchanged, and it prints that same matrix", mf.Report.Path)
	}

	// 5. The retracted documents are preserved unedited.
	for _, d := range mf.Preserved {
		body, err := preservedBody(d.Path)
		if err != nil {
			fail("%v", err)
			continue
		}
		if h := fmt.Sprintf("%x", sha256.Sum256(body)); h != d.BodySHA256 {
			fail("PRESERVED DOCUMENT EDITED: %s no longer matches the artifact "+
				"committed in %s (git blob %s). A retraction may not rewrite what "+
				"it retracts.", d.Path, d.FromCommit, d.GitBlob)
			continue
		}
		if d.Path == "study/RESULTS-armD.md" {
			pm, err := matrixIn(d.Path, body)
			if err != nil {
				fail("%v", err)
				continue
			}
			if pm != want {
				fail("%s PRINTS TP %d FP %d TN %d FN %d, not the manifest's matrix",
					d.Path, pm.TP, pm.FP, pm.TN, pm.FN)
				continue
			}
		}
		pass("preserved unedited: %s (from %s)", d.Path, d.FromCommit)
	}

	if len(bad) > 0 {
		fmt.Fprintln(os.Stderr)
		for _, m := range bad {
			fmt.Fprintln(os.Stderr, "  FAIL "+m)
		}
		return fmt.Errorf("arm D verification failed: %d problem(s)", len(bad))
	}

	fmt.Println()
	fmt.Println("  NOTE: this verifies reproducibility, source stability, and that the")
	fmt.Println("  published documents still say what was recorded. The labels are")
	fmt.Println("  author-declared; verification does not make them independent.")
	fmt.Println("  See study/ASSESSMENT-armD.md.")
	return nil
}
