package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const modulePath = "github.com/harshith/rzp-guard/"

// repoRoot moves to the repository root, because every path the manifest names
// is written relative to it. Restored on cleanup.
func repoRoot(t *testing.T) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(filepath.Join("..", "..")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
}

// importClosure walks non-test sources from dir and returns every package of
// this module that is reachable, dir included.
func importClosure(t *testing.T, dir string) map[string]bool {
	t.Helper()
	seen := map[string]bool{}
	var walk func(string)
	walk = func(d string) {
		if seen[d] {
			return
		}
		seen[d] = true
		ents, err := os.ReadDir(d)
		if err != nil {
			t.Fatalf("reading %s: %v", d, err)
		}
		for _, e := range ents {
			n := e.Name()
			if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(d, n), nil,
				parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parsing %s: %v", n, err)
			}
			for _, imp := range f.Imports {
				p, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				if strings.HasPrefix(p, modulePath) {
					walk(strings.TrimPrefix(p, modulePath))
				}
			}
		}
	}
	walk(dir)
	return seen
}

// THE CONTROL THIS TEST EXISTS FOR. The first version of `rzp-armd verify`
// hashed internal/policy and nothing else, while the score also ran
// internal/mandate, internal/lifecycle and internal/opauth. A verifier narrower
// than the thing it verifies gives a green light it has not earned.
//
// Hard-coding the package list would let it drift again the moment an import is
// added, so this recomputes the closure from the source and compares.
func TestTheManifestCoversTheWholeDecisionPath(t *testing.T) {
	repoRoot(t)

	got := importClosure(t, "cmd/rzp-armd")
	want := map[string]bool{"cmd/rzp-armd": true}
	for _, d := range decisionPathDirs {
		want[d] = true
	}

	var missing, extra []string
	for p := range got {
		if !want[p] {
			missing = append(missing, p)
		}
	}
	for p := range want {
		if !got[p] {
			extra = append(extra, p)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("the scorer imports %v, which the manifest does not hash. A "+
			"change there could move a published decision while `rzp-armd "+
			"verify` still reported everything unchanged. Add them to "+
			"decisionPathDirs and re-record the manifest.", missing)
	}
	if len(extra) > 0 {
		t.Errorf("the manifest hashes %v, which the scorer no longer imports. "+
			"Hashing dead packages is not harmful, but it makes the recorded "+
			"decision path a claim about something other than the evaluation.", extra)
	}
}

// internal/storage is deliberately absent from the decision path, on the ground
// that the scorer's persister is nil so no storage code is reachable. That is a
// convenient claim -- a bounded-retry fix landed in internal/storage the same
// day -- so it is checked rather than asserted.
func TestStorageIsGenuinelyOutsideTheDecisionPath(t *testing.T) {
	repoRoot(t)
	if importClosure(t, "cmd/rzp-armd")["internal/storage"] {
		t.Fatal("internal/storage is now reachable from the scorer, so a change " +
			"there can move an arm D decision. It must be hashed, and the " +
			"exclusion note in verify.go and in the manifest is now false.")
	}
}

// The control PROTOCOL-armD.md claimed and never built, now run by `go test`.
// Without this, `rzp-armd verify` is a command someone has to remember.
func TestArmDStillVerifies(t *testing.T) {
	repoRoot(t)
	if err := verifyArmD(); err != nil {
		t.Fatalf("arm D verification failed: %v", err)
	}
}
