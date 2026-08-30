package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// Every query in this package is scoped by mandate_id, recovery included. That
// makes a state file reopened under a different mandate silently empty rather
// than obviously wrong: a refund left RESERVED by the previous process is never
// promoted to IN_DOUBT, never listed, never resolvable -- while the money may
// already have moved. -state defaults to rzp-guard.db for both binaries and the
// mandate is compiled per session, so this is the DEFAULT second run.

func TestReopeningUnderAnotherMandateIsRefusedWhileWorkIsUnresolved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	first, err := Open(path, "mnd_AAA")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Reserve("rfa_inflight", "rzpg_aaaaaaaaaaaa", 50000); err != nil {
		t.Fatal(err)
	}
	first.Close() // the process dies mid-flight

	second, err := Open(path, "mnd_BBB")
	if err == nil {
		second.Close()
		t.Fatal("a state file holding another mandate's in-flight refund was opened " +
			"under a new mandate; the RESERVED row would never be recovered and " +
			"never shown to an operator")
	}
	if !errors.Is(err, ErrMandateMismatch) {
		t.Fatalf("refused with %v, want ErrMandateMismatch", err)
	}
	// The operator has to be told WHICH mandate and WHICH action, or the advice
	// to reopen with it is unactionable.
	for _, want := range []string{"mnd_AAA", "rfa_inflight"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
}

// The refusal is only defensible if the escape route it recommends works.
func TestTheStrandedActionIsRecoverableUnderItsOwnMandate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	first, err := Open(path, "mnd_AAA")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Reserve("rfa_inflight", "rzpg_aaaaaaaaaaaa", 50000); err != nil {
		t.Fatal(err)
	}
	first.Close()

	again, err := Open(path, "mnd_AAA")
	if err != nil {
		t.Fatalf("reopening with the SAME mandate must work -- that is an ordinary "+
			"restart, and it is the recovery path the refusal points at: %v", err)
	}
	defer again.Close()

	promoted, err := again.RecoverStartup()
	if err != nil {
		t.Fatal(err)
	}
	if len(promoted) != 1 || promoted[0] != "rfa_inflight" {
		t.Fatalf("recovery promoted %v, want the stranded action", promoted)
	}
	rows, _ := again.ActionsInState("IN_DOUBT")
	if len(rows) != 1 {
		t.Fatalf("the operator console shows %d IN_DOUBT rows, want 1", len(rows))
	}
}

// Nothing unresolved means nothing to lose, so a new mandate may reuse the file.
// Refusing here would make the guard unstartable after every clean session.
func TestAnotherMandateMayReuseAFinishedStateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	first, err := Open(path, "mnd_AAA")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Reserve("rfa_done", "rzpg_aaaaaaaaaaaa", 1000); err != nil {
		t.Fatal(err)
	}
	if err := first.SetState("rfa_done", "RESERVED", "COMMITTED"); err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := Open(path, "mnd_BBB")
	if err != nil {
		t.Fatalf("a finished state file was refused to a new mandate: %v", err)
	}
	defer second.Close()

	// And ownership actually transfers, or the next open would still see mnd_AAA.
	var owner string
	if err := second.db.QueryRow("SELECT mandate_id FROM owner WHERE id = 1").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "mnd_BBB" {
		t.Fatalf("owner row still says %q after a successful takeover", owner)
	}
}

// The mutation sweep found this gap: counting only RESERVED rows as unresolved
// passed every test above. Work can be stranded in the other direction too --
// already recovered to IN_DOUBT and waiting on a human. That is the stronger
// case, not the weaker one: the action is explicitly locked pending an
// operator's finding, so hiding it behind a mandate swap strands a decision
// somebody was told to make.
func TestReopeningIsRefusedWhenTheOtherMandateAwaitsAnOperator(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	first, err := Open(path, "mnd_AAA")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Reserve("rfa_waiting", "rzpg_aaaaaaaaaaaa", 50000); err != nil {
		t.Fatal(err)
	}
	// Recovered already: nothing is RESERVED any more, only IN_DOUBT.
	if err := first.SetState("rfa_waiting", "RESERVED", "IN_DOUBT"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := first.ActionsInState("RESERVED"); len(rows) != 0 {
		t.Fatalf("precondition: %d RESERVED rows, want 0 so this test can only "+
			"pass by counting IN_DOUBT", len(rows))
	}
	first.Close()

	second, err := Open(path, "mnd_BBB")
	if err == nil {
		second.Close()
		t.Fatal("a state file holding another mandate's IN_DOUBT action was opened " +
			"under a new mandate; an operator was waiting to resolve it and it " +
			"would never appear again")
	}
	if !errors.Is(err, ErrMandateMismatch) {
		t.Fatalf("refused with %v, want ErrMandateMismatch", err)
	}
	if !strings.Contains(err.Error(), "rfa_waiting") {
		t.Errorf("refusal does not name the waiting action: %v", err)
	}
}
