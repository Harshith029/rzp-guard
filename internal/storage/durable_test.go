package storage

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The durable layer is what makes an IN_DOUBT refund survive a crash and what
// makes an operator's decision auditable. Coverage here was 39.8%, and the
// paths below -- the atomic resolve, the UNIQUE receipt backstop, and
// credential rotation -- were the untested ones that matter.

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"), "mnd_test")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// The receipt is a 48-bit TRUNCATED hash. Collision resistance is not
// uniqueness, so the schema carries a UNIQUE constraint as the backstop: a
// collision must be REFUSED, never allowed to attach a second action to a
// receipt the provider already knows.
func TestReceiptUniquenessIsEnforced(t *testing.T) {
	s := openTemp(t)

	if err := s.Reserve("rfa_001", "rzpg_deadbeef1234", 10000); err != nil {
		t.Fatalf("first reserve: %v", err)
	}
	err := s.Reserve("rfa_002", "rzpg_deadbeef1234", 20000)
	if err == nil {
		t.Fatal("a second action reused an existing receipt; the truncated hash " +
			"has no uniqueness guarantee, so the database must be the one that refuses")
	}
	if !errors.Is(err, ErrReceiptExists) {
		t.Fatalf("got %v, want ErrReceiptExists so the caller can tell a collision "+
			"from an ordinary write failure", err)
	}

	// The refusal must be total: no half-written row for the second action.
	rows, err := s.ActionsInState("RESERVED")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ActionID != "rfa_001" {
		t.Fatalf("after a refused collision the table holds %d reserved rows, want only rfa_001", len(rows))
	}
}

// Resolution moves the state AND writes the audit row, in one transaction.
// A decision that moved money without leaving a record, or a record with no
// decision, would each be worse than failing outright.
func TestResolveInDoubtIsAtomicWithItsAudit(t *testing.T) {
	s := openTemp(t)
	if err := s.Reserve("rfa_001", "rzpg_aaaaaaaaaaaa", 30000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetState("rfa_001", "RESERVED", "IN_DOUBT"); err != nil {
		t.Fatal(err)
	}

	before, _ := s.AuditCount()
	if before != 0 {
		t.Fatalf("audit already had %d rows", before)
	}

	if err := s.ResolveInDoubt("rfa_001", "COMMITTED", "alice", "checked the dashboard", true); err != nil {
		t.Fatalf("ResolveInDoubt: %v", err)
	}

	after, _ := s.AuditCount()
	if after != 1 {
		t.Fatalf("audit has %d rows after one resolution, want 1", after)
	}
	trail, err := s.AuditTrail()
	if err != nil {
		t.Fatal(err)
	}
	a := trail[0]
	if a.Actor != "alice" || a.ActionID != "rfa_001" {
		t.Fatalf("audit row does not identify who decided what: %+v", a)
	}
	if a.From != "IN_DOUBT" || a.To != "COMMITTED" {
		t.Fatalf("audit row records %s -> %s", a.From, a.To)
	}
	if !a.RefundLanded {
		t.Fatal("refund_landed was not recorded; it is the operator's finding about " +
			"whether the money actually moved")
	}
	if !strings.Contains(a.Reason, "dashboard") {
		t.Fatalf("the operator's reason was not stored: %q", a.Reason)
	}

	rows, _ := s.ActionsInState("COMMITTED")
	if len(rows) != 1 {
		t.Fatalf("state did not move: %d COMMITTED rows", len(rows))
	}
}

// Only an IN_DOUBT action may be resolved, and a refusal must leave BOTH the
// state and the audit untouched -- a rejected decision that still wrote a
// record would make the trail lie.
func TestResolveRefusesAnythingNotInDoubt(t *testing.T) {
	s := openTemp(t)
	if err := s.Reserve("rfa_001", "rzpg_bbbbbbbbbbbb", 10000); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, action string }{
		{"a RESERVED action", "rfa_001"},
		{"an action that does not exist", "rfa_nope"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.ResolveInDoubt(tc.action, "COMMITTED", "alice", "r", false); err == nil {
				t.Fatal("resolution must be refused")
			}
			if n, _ := s.AuditCount(); n != 0 {
				t.Fatalf("a refused resolution wrote %d audit rows; the trail must not "+
					"record decisions that did not happen", n)
			}
		})
	}

	rows, _ := s.ActionsInState("RESERVED")
	if len(rows) != 1 {
		t.Fatalf("state changed despite refusal: %d RESERVED rows", len(rows))
	}
}

// Recovery is the crash-safety claim: anything still RESERVED when the process
// died is promoted to IN_DOUBT, because the guard cannot know whether the
// refund landed. COMMITTED and IN_DOUBT rows must be left alone.
func TestRecoverStartupPromotesOnlyReserved(t *testing.T) {
	s := openTemp(t)
	for _, a := range []struct {
		id, receipt, to string
		amount          int64
	}{
		{"rfa_res", "rzpg_111111111111", "", 1000},
		{"rfa_com", "rzpg_222222222222", "COMMITTED", 2000},
		{"rfa_dbt", "rzpg_333333333333", "IN_DOUBT", 3000},
	} {
		if err := s.Reserve(a.id, a.receipt, a.amount); err != nil {
			t.Fatal(err)
		}
		if a.to != "" {
			if err := s.SetState(a.id, "RESERVED", a.to); err != nil {
				t.Fatal(err)
			}
		}
	}

	promoted, err := s.RecoverStartup()
	if err != nil {
		t.Fatalf("RecoverStartup: %v", err)
	}
	if len(promoted) != 1 || promoted[0] != "rfa_res" {
		t.Fatalf("promoted %v, want only the still-RESERVED action", promoted)
	}

	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"rfa_res": "IN_DOUBT", // was mid-flight
		"rfa_com": "COMMITTED",
		"rfa_dbt": "IN_DOUBT",
	}
	for id, st := range want {
		if snap.States[id] != st {
			t.Errorf("%s is %s after recovery, want %s", id, snap.States[id], st)
		}
	}
	// Amounts must survive, or the budget would be freed by a restart.
	if snap.Amounts["rfa_res"] != 1000 || snap.Amounts["rfa_com"] != 2000 {
		t.Fatalf("amounts lost across recovery: %+v", snap.Amounts)
	}

	// Idempotent: a second recovery has nothing left to promote.
	again, err := s.RecoverStartup()
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("second recovery promoted %v; recovery must be idempotent", again)
	}
}

// Rotation replaces the credential and leaves a record. Rotating when none has
// been provisioned must fail rather than quietly create one -- that would be a
// way to install a credential without going through init.
func TestRotateOperatorVerifier(t *testing.T) {
	s := openTemp(t)

	if err := s.RotateOperatorVerifier("argon2id$new", "alice", "quarterly"); err == nil {
		t.Fatal("rotating with no credential provisioned must fail, not create one")
	}

	if err := s.InitOperatorVerifier("argon2id$first"); err != nil {
		t.Fatal(err)
	}
	v, configured, ephemeral, err := s.OperatorVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if !configured || ephemeral || v != "argon2id$first" {
		t.Fatalf("after init: v=%q configured=%v ephemeral=%v", v, configured, ephemeral)
	}

	if err := s.RotateOperatorVerifier("argon2id$second", "alice", "quarterly"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	v, _, _, _ = s.OperatorVerifier()
	if v != "argon2id$second" {
		t.Fatalf("verifier is %q after rotation, want the new one", v)
	}

	trail, err := s.AuditTrail()
	if err != nil {
		t.Fatal(err)
	}
	if len(trail) != 1 || trail[0].To != "ROTATED" {
		t.Fatalf("rotation left no audit record: %+v", trail)
	}
	if trail[0].Actor != "alice" {
		t.Fatalf("rotation audit does not name who did it: %+v", trail[0])
	}
}

// A verifier may be installed once. A second init must be refused, or an
// attacker who reached the state file could replace the operator credential
// without knowing the current one.
func TestOperatorVerifierCannotBeSilentlyReplaced(t *testing.T) {
	s := openTemp(t)
	if err := s.InitOperatorVerifier("argon2id$first"); err != nil {
		t.Fatal(err)
	}
	if err := s.InitOperatorVerifier("argon2id$second"); err == nil {
		t.Fatal("a second init must be refused; replacing a credential is what " +
			"rotate is for, and rotate leaves an audit record")
	}
	v, _, _, _ := s.OperatorVerifier()
	if v != "argon2id$first" {
		t.Fatalf("the original credential was overwritten: %q", v)
	}
}
