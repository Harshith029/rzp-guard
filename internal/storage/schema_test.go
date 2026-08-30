package storage

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// A state file outlives the binary that made it. Without a version stamp, the
// first schema change has no way to tell an old file from a new one, and the
// failure mode is silent misreading of a file that records money.
//
// These tests are cheap now and impossible later: they only work while every
// state file is disposable.

// setVersion reopens a CLOSED state file and forces its recorded version, which
// is how a file written by another build is simulated.
func setVersion(t *testing.T, path string, v int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE schema_meta SET version = ? WHERE id = 1`, v); err != nil {
		t.Fatal(err)
	}
}

func TestNewStateFileIsStampedWithTheCurrentSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := s.db.QueryRow(`SELECT version FROM schema_meta WHERE id = 1`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Fatalf("new file stamped version %d, want %d", v, schemaVersion)
	}
	_ = s.Close()

	// Reopening must not re-stamp or complain.
	s2, err := Open(path, "mnd_test")
	if err != nil {
		t.Fatalf("reopening a file this build wrote must succeed: %v", err)
	}
	_ = s2.Close()
}

// The dangerous direction: a file from a NEWER build may track state in columns
// this build cannot see. Opening it would act on a partial view of money.
func TestAFileFromANewerBuildIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	setVersion(t, path, schemaVersion+1)

	_, err = Open(path, "mnd_test")
	if err == nil {
		t.Fatal("opened a state file written by a newer build; this build may be " +
			"blind to columns that file uses to track in-flight money")
	}
	if !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("got %v, want ErrSchemaVersion so a caller can tell a version "+
			"mismatch from a corrupt file", err)
	}
	// The message has to tell an operator what to do, because the wrong move
	// here (deleting the file) destroys IN_DOUBT actions.
	if !strings.Contains(err.Error(), "Upgrade the binary") {
		t.Fatalf("error does not name the remedy: %v", err)
	}
}

// The other direction means a schema change shipped with no migration. That is
// this repository's bug, and the message must say so rather than implying the
// operator's file is at fault.
func TestAFileFromAnOlderSchemaIsRefusedWithOurOwnBlame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	setVersion(t, path, schemaVersion-1)

	_, err = Open(path, "mnd_test")
	if err == nil {
		t.Fatal("opened a file at an older schema with no migration")
	}
	if !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("got %v, want ErrSchemaVersion", err)
	}
	if !strings.Contains(err.Error(), "defect in rzp-guard") {
		t.Fatalf("error blames the operator's file instead of this build: %v", err)
	}
	if !strings.Contains(err.Error(), "Do NOT delete") {
		t.Fatalf("error does not warn against deleting a file that may hold "+
			"IN_DOUBT actions: %v", err)
	}
}

// A file created before schema_meta existed carries the v1 schema, so it must
// be adopted rather than rejected. Rejecting it would strand every state file
// written before this change.
func TestAFileWithNoStampIsAdoptedAsCurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reserve("rfa_001", "rzpg_aaaaaaaaaaaa", 1000); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// Simulate the pre-versioning world.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE schema_meta`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s2, err := Open(path, "mnd_test")
	if err != nil {
		t.Fatalf("a file predating the version stamp must be adopted, not "+
			"rejected; rejecting strands every state file written before this "+
			"change: %v", err)
	}
	defer s2.Close()

	// And its contents must survive adoption.
	rows, err := s2.ActionsInState("RESERVED")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ActionID != "rfa_001" {
		t.Fatalf("adoption lost state: %+v", rows)
	}
}
