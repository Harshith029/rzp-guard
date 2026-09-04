package storage

import (
	"path/filepath"
	"testing"
)

// Every query in this package is scoped by mandate_id, recovery included. That
// used to make a state file reopened under a different mandate silently empty
// rather than obviously wrong: a refund left RESERVED by the previous process
// was never promoted to IN_DOUBT, never listed, never resolvable -- while the
// money may already have moved.
//
// THE GUARANTEE MOVED FROM REFUSE TO SURFACE, and these tests moved with it.
//
// Open used to refuse a file whose previous mandate had unresolved work. That
// was the right answer while a file held exactly one mandate, because the
// exclusive lock made anything else impossible anyway. It is the wrong answer
// now that a file can hold several: it would refuse the normal multi-tenant
// case, and the whole reason for multi-tenancy is that ten merchants on a host
// should share ONE queue and ONE operator credential rather than needing an
// operator to guess which of ten databases holds the refund they are hunting.
//
// So the file is shared, and the stranded work is reported instead --
// continuously, through StrandedElsewhere at every start and through the
// operator's cross-mandate view, rather than once, as a reason to fail. The
// tests below assert that nothing is hidden, which is the property the refusal
// was protecting.

func TestAnotherMandateMayShareTheFileAndTheStrandedWorkIsStillVisible(t *testing.T) {
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
	if err != nil {
		t.Fatalf("a second mandate could not share the state file: %v", err)
	}
	defer second.Close()

	// mnd_BBB's own view is scoped to mnd_BBB, exactly as before. That scoping is
	// the money property -- one mandate's actions must never enter another's
	// ledger or cap arithmetic -- and sharing the file does not weaken it.
	if rows, _ := second.ActionsInState("RESERVED"); len(rows) != 0 {
		t.Fatalf("mnd_BBB sees %d of mnd_AAA's rows in its own ledger", len(rows))
	}

	// But the stranded work is not hidden. This is what replaces the refusal.
	stranded, err := second.StrandedElsewhere()
	if err != nil {
		t.Fatal(err)
	}
	ids, ok := stranded["mnd_AAA"]
	if !ok || len(ids) != 1 || ids[0] != "rfa_inflight" {
		t.Fatalf("StrandedElsewhere = %v; mnd_AAA's in-flight refund must be "+
			"surfaced to whoever opens this file", stranded)
	}
}

// The stronger case: already recovered to IN_DOUBT and waiting on a human.
// Nothing is RESERVED any more, so this can only pass by counting IN_DOUBT too.
func TestWorkAwaitingAnOperatorIsSurfacedAcrossMandates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	first, err := Open(path, "mnd_AAA")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Reserve("rfa_waiting", "rzpg_aaaaaaaaaaaa", 50000); err != nil {
		t.Fatal(err)
	}
	if err := first.SetState("rfa_waiting", "RESERVED", "IN_DOUBT"); err != nil {
		t.Fatal(err)
	}
	if rows, _ := first.ActionsInState("RESERVED"); len(rows) != 0 {
		t.Fatalf("precondition: %d RESERVED rows, want 0", len(rows))
	}
	first.Close()

	second, err := Open(path, "mnd_BBB")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	stranded, err := second.StrandedElsewhere()
	if err != nil {
		t.Fatal(err)
	}
	if ids := stranded["mnd_AAA"]; len(ids) != 1 || ids[0] != "rfa_waiting" {
		t.Fatalf("an action explicitly locked pending an operator's finding was "+
			"not surfaced: %v", stranded)
	}
}

// The escape route the old refusal recommended must still work: reopening under
// the action's own mandate recovers it.
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
			"restart: %v", err)
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

// Two mandates in one file must keep their ledgers and their leases apart. This
// is the property that makes a shared file safe, so it is asserted rather than
// assumed.
func TestTwoMandatesInOneFileAreIndependent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")

	a, err := Open(path, "mnd_A")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path, "mnd_B")
	if err != nil {
		t.Fatalf("a second mandate could not take its own lease on a shared file: %v", err)
	}
	defer b.Close()

	// Same action id under both mandates. The primary key is (mandate, action),
	// so these are two different rows and neither may see the other's state.
	if err := a.Reserve("rfa_1", "rzpg_aaaaaaaaaaaa", 1000); err != nil {
		t.Fatal(err)
	}
	if err := b.Reserve("rfa_1", "rzpg_bbbbbbbbbbbb", 2000); err != nil {
		t.Fatalf("mandate B could not reserve its own action of the same name: %v", err)
	}
	if err := a.SetState("rfa_1", "RESERVED", "COMMITTED"); err != nil {
		t.Fatal(err)
	}
	rows, err := b.ActionsInState("RESERVED")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AmountPaise != 2000 {
		t.Fatalf("mandate B's ledger = %+v after mandate A committed its own row", rows)
	}

	names, err := a.Mandates()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 {
		t.Fatalf("the file reports %v, want both mandates: a cross-mandate operator "+
			"view is the reason sharing is allowed at all", names)
	}
}
