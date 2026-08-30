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

// An older schema with NO migration path. v1 has one (see below), so this uses
// version 0. The message must blame this repository rather than implying the
// operator's file is at fault, because the instinct on meeting it is to delete
// a file that may hold IN_DOUBT actions.
func TestAFileFromAnOlderSchemaIsRefusedWithOurOwnBlame(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path, "mnd_test")
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	setVersion(t, path, 0)

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

// buildV1File writes a state file with the ORIGINAL v1 layout: action_state
// with a UNIQUE receipt, no call_receipt table, and no schema_meta row.
// This is what every state file written before schema v2 actually looks like.
func buildV1File(t *testing.T, path string, rows [][3]any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE owner (id INTEGER PRIMARY KEY CHECK (id = 1),
		   mandate_id TEXT NOT NULL, acquired_at TEXT NOT NULL)`,
		`CREATE TABLE action_state (
		   mandate_id   TEXT    NOT NULL,
		   action_id    TEXT    NOT NULL,
		   receipt      TEXT    NOT NULL UNIQUE,
		   state        TEXT    NOT NULL,
		   amount_paise INTEGER NOT NULL,
		   updated_at   TEXT    NOT NULL,
		   PRIMARY KEY (mandate_id, action_id))`,
		`INSERT INTO owner (id, mandate_id, acquired_at) VALUES (1, 'mnd_test', '2026-01-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("building v1 file (%s): %v", q[:30], err)
		}
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO action_state (mandate_id, action_id, receipt, state, amount_paise, updated_at)
			 VALUES ('mnd_test', ?, ?, ?, ?, '2026-01-01T00:00:00Z')`,
			r[0], r[1], r[2], 5000); err != nil {
			t.Fatal(err)
		}
	}
}

// The migration that schema versioning existed for.
//
// A v1 file must be adopted and upgraded, not refused: refusing would strand
// every state file written before v2, including ones holding IN_DOUBT actions
// whose refunds already moved money.
func TestAV1FileIsMigratedToV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	buildV1File(t, path, [][3]any{
		{"rfa_001", "rzpg_aaaaaaaaaaaa", "IN_DOUBT"},
		{"rfa_002", "rzpg_bbbbbbbbbbbb", "COMMITTED"},
	})

	s, err := Open(path, "mnd_test")
	if err != nil {
		t.Fatalf("a v1 file must migrate, not fail: %v", err)
	}
	defer s.Close()

	var v int
	if err := s.db.QueryRow(`SELECT version FROM schema_meta WHERE id = 1`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != 2 {
		t.Fatalf("version is %d after migration, want 2", v)
	}

	// Nothing may be lost. These rows are money in a known state.
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.States["rfa_001"] != "IN_DOUBT" || snap.States["rfa_002"] != "COMMITTED" {
		t.Fatalf("migration changed state: %+v", snap.States)
	}
	if snap.Amounts["rfa_001"] != 5000 {
		t.Fatalf("migration lost amounts: %+v", snap.Amounts)
	}

	// The uniqueness guarantee must be CONTINUOUS across the migration: a
	// receipt already issued in v1 must still be refused in v2, or the upgrade
	// would quietly permit re-issuing a provider correlation key.
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM call_receipt WHERE receipt = 'rzpg_aaaaaaaaaaaa'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("migration did not backfill call_receipt; a receipt issued before " +
			"the upgrade could be issued again after it")
	}

	// And the old UNIQUE constraint must be gone, which is what the migration
	// was for: one call may now consume several actions under one receipt.
	var ddl string
	if err := s.db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='action_state'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ddl, "UNIQUE") {
		t.Fatalf("action_state still declares UNIQUE after migration:\n%s", ddl)
	}
}

// Migrating twice must be a no-op, because a guard restarts.
func TestMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	buildV1File(t, path, [][3]any{{"rfa_001", "rzpg_aaaaaaaaaaaa", "COMMITTED"}})

	for i := 0; i < 3; i++ {
		s, err := Open(path, "mnd_test")
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		snap, err := s.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if snap.States["rfa_001"] != "COMMITTED" {
			t.Fatalf("open %d: state became %s", i, snap.States["rfa_001"])
		}
		_ = s.Close()
	}
}
