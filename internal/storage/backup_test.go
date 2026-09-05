package storage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// backupFixture builds a state file with something worth losing in it: a
// committed action, one awaiting an operator, an open refusal, and a credential.
func backupFixture(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	s, err := Open(path, "mnd_backup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.Reserve("rfa_done", "rzpg_aaaaaaaaaaaa", 1000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetState("rfa_done", "RESERVED", "COMMITTED"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve("rfa_stuck", "rzpg_bbbbbbbbbbbb", 2000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetState("rfa_stuck", "RESERVED", "IN_DOUBT"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDenial("create_refund", "NO_AUTHORIZED_ACTION",
		"pay_SYN0001", 7500, "no authorized refund action exists"); err != nil {
		t.Fatal(err)
	}
	// The credential, without which a restored file is one the guard refuses to
	// start against.
	_ = operatorGrant(t, "ops@merchant.example")
	if err := s.InitOperatorVerifier(
		"$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHQ$c29tZWhhc2h2YWx1ZQ"); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

// A backup must be takeable WHILE THE GUARD IS RUNNING. One you have to stop
// the payment proxy for is one nobody takes on a schedule, and an untaken
// backup is the same as none.
func TestABackupIsTakenWithoutStoppingTheGuard(t *testing.T) {
	s, dir := backupFixture(t)
	dest := filepath.Join(dir, "backups", "state-2026-09-05.db")

	res, err := s.Backup(dest)
	if err != nil {
		t.Fatalf("backup while the lease was held: %v", err)
	}
	if res.Bytes == 0 {
		t.Error("backup is empty")
	}
	// The counts are the operator's evidence. "1.2MB written" says nothing.
	if res.Actions != 2 || res.InDoubt != 1 || res.OpenQueue != 1 {
		t.Errorf("backup reports %d actions, %d IN_DOUBT, %d open refusals; "+
			"want 2/1/1", res.Actions, res.InDoubt, res.OpenQueue)
	}
	if len(res.Mandates) != 1 || res.Mandates[0] != "mnd_backup" {
		t.Errorf("mandates = %v", res.Mandates)
	}

	// And the guard keeps working through it.
	if err := s.Renew(); err != nil {
		t.Fatalf("the guard lost its lease to a backup: %v", err)
	}
	if err := s.Reserve("rfa_after", "rzpg_cccccccccccc", 500); err != nil {
		t.Fatalf("the guard could not write after a backup: %v", err)
	}
}

// A backup file holds the Argon2id verifier and every payment id the guard has
// seen. World-readable on a shared host is the kind of file that becomes the
// incident. Same reasoning as the evidence tee, which shipped 0666 until it
// was found.
func TestABackupIsNotWorldReadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		// Go's Chmod on Windows only toggles the read-only bit, so Perm() reads
		// 0666 whatever was asked for. The check is meaningful on the platform
		// the guard actually deploys to, and CI runs it there.
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	s, dir := backupFixture(t)
	dest := filepath.Join(dir, "b.db")
	if _, err := s.Backup(dest); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("backup mode is %o; it carries the operator verifier and every "+
			"payment id this guard has seen", mode)
	}
}

// Refusing to overwrite is the point. A backup is what you reach for when
// something has gone wrong, and replacing a good copy with a copy of a database
// that may itself be damaged is how the good one is lost.
func TestABackupWillNotOverwriteAnExistingFile(t *testing.T) {
	s, dir := backupFixture(t)
	dest := filepath.Join(dir, "b.db")
	if _, err := s.Backup(dest); err != nil {
		t.Fatal(err)
	}
	_, err := s.Backup(dest)
	if !errors.Is(err, ErrBackupExists) {
		t.Fatalf("second backup returned %v, want ErrBackupExists", err)
	}
}

// THE RESTORE HALF. A backup nobody has opened is a hypothesis. This is the
// procedure end to end: take one, lose the original, restore, and prove the
// state that mattered came back -- the IN_DOUBT refund awaiting a human, the
// consumed action that must not become spendable again, and the credential
// without which neither could be resolved.
func TestARestoredBackupCarriesTheStateThatMattered(t *testing.T) {
	s, dir := backupFixture(t)
	dest := filepath.Join(dir, "b.db")
	if _, err := s.Backup(dest); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// The original is gone. This is the disaster.
	restored := filepath.Join(dir, "restored.db")
	if err := copyFile(dest, restored); err != nil {
		t.Fatal(err)
	}

	back, err := Open(restored, "mnd_backup")
	if err != nil {
		t.Fatalf("a restored backup does not open: %v", err)
	}
	defer back.Close()

	rows, err := back.ActionsInState("IN_DOUBT")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ActionID != "rfa_stuck" {
		t.Fatalf("the refund awaiting an operator did not survive: %+v", rows)
	}
	// A consumed action must NOT come back as spendable. This is the whole risk
	// of losing the file: every action returns to AVAILABLE and a replayed
	// mandate spends its authority twice.
	snap, err := back.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.States["rfa_done"] != "COMMITTED" {
		t.Errorf("a consumed action restored as %q; replaying the mandate against "+
			"this file would spend it again", snap.States["rfa_done"])
	}
	_, configured, ephemeral, err := back.OperatorVerifier()
	if err != nil || !configured || ephemeral {
		t.Errorf("the operator credential did not survive (configured=%v ephemeral=%v "+
			"err=%v); the guard would refuse to start against this file",
			configured, ephemeral, err)
	}
	queue, err := back.Denials(DenialOpen, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 {
		t.Errorf("the refusal queue restored with %d entries, want 1", len(queue))
	}
}

// Verification must work on a file found on a disk months later, with no
// original present and no guard running.
func TestABackupVerifiesStandalone(t *testing.T) {
	s, dir := backupFixture(t)
	dest := filepath.Join(dir, "b.db")
	if _, err := s.Backup(dest); err != nil {
		t.Fatal(err)
	}
	s.Close()

	res, err := VerifyBackup(dest)
	if err != nil {
		t.Fatalf("standalone verification failed: %v", err)
	}
	if res.Actions != 2 || res.InDoubt != 1 {
		t.Errorf("standalone verify reports %d actions / %d IN_DOUBT", res.Actions, res.InDoubt)
	}
}

// A truncated or corrupt file must be REFUSED, not repaired. Applying the
// schema to it would produce something that opens cleanly and is wrong, which
// is the worst shape a backup failure can take.
func TestACorruptBackupIsRefusedRatherThanRepaired(t *testing.T) {
	s, dir := backupFixture(t)
	dest := filepath.Join(dir, "b.db")
	if _, err := s.Backup(dest); err != nil {
		t.Fatal(err)
	}
	s.Close()

	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(dest, fi.Size()/2); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBackup(dest); err == nil {
		t.Fatal("a half-truncated backup verified clean")
	}
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

// A restored backup must not carry the dead guard's lease.
//
// Found by the restore test above rather than by inspection: VACUUM INTO copies
// every table, so a backup taken while the guard ran carried its lease with a
// heartbeat frozen at the moment of the copy. The first guard to start against
// a restored file was refused, naming a pid on a host that may not exist any
// more. It clears itself after leaseTTL, so the outage was short; the confusion,
// during a restore, would not have been.
func TestARestoredBackupCarriesNoLease(t *testing.T) {
	s, dir := backupFixture(t)
	dest := filepath.Join(dir, "b.db")
	if _, err := s.Backup(dest); err != nil {
		t.Fatal(err)
	}

	// The original guard is STILL RUNNING and still holds its lease, which is
	// the realistic case: backups are taken from live systems.
	if err := s.Renew(); err != nil {
		t.Fatalf("taking a backup released the running guard's own lease: %v", err)
	}

	restored := filepath.Join(dir, "restored.db")
	if err := copyFile(dest, restored); err != nil {
		t.Fatal(err)
	}
	back, err := Open(restored, "mnd_backup")
	if err != nil {
		t.Fatalf("a restored backup refused the first guard to start against it, "+
			"blaming a process that no longer exists: %v", err)
	}
	defer back.Close()
}
