package storage

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Single-instance ownership is a money claim, not a tidiness claim. Each guard
// process restores its own in-memory ledger and checks the cumulative cap
// against it, so two guards sharing one state file would each authorize up to
// the cap and between them spend twice it. SetMaxOpenConns(1) does not help:
// it serializes writers within one process only.
//
// Nothing tested this. ErrNotOwner was returned by Open and asserted nowhere.

// The claim is about two PROCESSES. An in-process second Open shares the
// runtime and could in principle be refused by driver bookkeeping rather than
// by a real file lock, which would not protect anything. This re-execs the test
// binary so the lock is contended across an OS process boundary.
func TestSecondGuardProcessIsRefused(t *testing.T) {
	if os.Getenv("RZP_OWNER_CHILD") != "" {
		return // the child runs the helper below, not this
	}
	path := filepath.Join(t.TempDir(), "state.db")

	owner, err := Open(path, "mnd_incumbent")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer owner.Close()

	cmd := exec.Command(os.Args[0], "-test.run", "TestChildOpenAttempt", "-test.v")
	cmd.Env = append(os.Environ(), "RZP_OWNER_CHILD=1", "RZP_OWNER_PATH="+path)
	out, err := cmd.CombinedOutput()
	got := string(out)
	if err == nil {
		t.Fatalf("a separate process opened the owned state file:\n%s", got)
	}
	if !strings.Contains(got, "CHILD_REFUSED") {
		t.Fatalf("child failed for some reason other than ownership:\n%s", got)
	}
	if !strings.Contains(got, "owned by another guard process") {
		t.Fatalf("child was refused, but not as an ownership conflict:\n%s", got)
	}

	// The owner is unaffected by the contention.
	if err := owner.Reserve("rfa_001", "rzpg_bbbbbbbbbbbb", 5000); err != nil {
		t.Fatalf("owner could not write after a cross-process takeover attempt: %v", err)
	}
}

// TestChildOpenAttempt runs only in the re-exec'd child. It opens the state
// file its parent owns and fails the test if that succeeds.
func TestChildOpenAttempt(t *testing.T) {
	path := os.Getenv("RZP_OWNER_PATH")
	if os.Getenv("RZP_OWNER_CHILD") == "" || path == "" {
		t.Skip("helper: runs only in the re-exec'd child")
	}
	s, err := Open(path, "mnd_child")
	if err == nil {
		s.Close()
		t.Fatal("CHILD_OPENED: a second process took a state file that was owned")
	}
	t.Fatalf("CHILD_REFUSED: %v", err)
}
