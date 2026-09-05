package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// BACKUP, which scored 1 out of 10 in the engineering audit: "None. No
// documented procedure, no RPO or RTO."
//
// WHAT IS ACTUALLY AT RISK. Not much money and quite a lot of accountability.
// The state file holds the action ledger, the operator's Argon2id verifier, the
// audit trail, the denial queue and every issued grant. Losing it does NOT lose
// a refund -- Razorpay is the system of record for whether money moved. It
// loses the ability to say which authorizations were consumed, which is worse
// than it sounds: every action returns to AVAILABLE, so a mandate replayed
// against a fresh file can spend its whole authority a second time, and every
// IN_DOUBT refund awaiting a human vanishes along with the question.
//
// So the recovery objective is about REPLAY and ACCOUNTABILITY, not about
// money in flight, and that is what makes a simple file-level backup adequate
// here rather than a streaming replica.
//
// WHY VACUUM INTO AND NOT A FILE COPY. cp on a live SQLite database in WAL mode
// copies the main file without the -wal, so the copy is missing every committed
// transaction that has not been checkpointed -- which, at synchronous=FULL with
// short transactions, is most of the recent ones. It produces a file that opens
// cleanly and is silently out of date, which is the worst failure shape a backup
// can have. VACUUM INTO takes a read transaction and writes a complete,
// consistent, already-compacted database, and it works while the guard is
// running.

// ErrBackupExists means the destination is already there. Refused rather than
// overwritten: a backup file is the thing you reach for when something has gone
// wrong, and silently replacing one with a copy of a database that may itself
// be damaged is how the good copy is lost.
var ErrBackupExists = errors.New("backup destination already exists")

// BackupResult is what a backup produced, for the operator's screen and for a
// test to assert against.
type BackupResult struct {
	Path      string
	Bytes     int64
	TakenAt   time.Time
	Mandates  []string
	Actions   int
	InDoubt   int
	OpenQueue int
}

// Backup writes a consistent copy of the state file to path.
//
// SAFE WHILE THE GUARD IS RUNNING. VACUUM INTO holds a read transaction for the
// duration, so a concurrent writer is briefly serialized behind it rather than
// producing a torn copy. That is the whole reason the lease replaced the
// exclusive lock: a backup you have to stop the payment proxy to take is a
// backup nobody takes on a schedule.
func (s *Store) Backup(path string) (BackupResult, error) {
	if path == "" {
		return BackupResult{}, errors.New("storage: backup: no destination given")
	}
	if _, err := os.Stat(path); err == nil {
		return BackupResult{}, fmt.Errorf("%w: %s", ErrBackupExists, path)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return BackupResult{}, fmt.Errorf("storage: backup: %w", err)
		}
	}

	// The destination inherits the source's sensitivity: it holds the Argon2id
	// verifier and every payment id the guard has seen. Create it 0600 BEFORE
	// SQLite writes into it, because SQLite creates with the process umask and a
	// world-readable copy of this on a shared host is the kind of file that
	// becomes the incident.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return BackupResult{}, fmt.Errorf("storage: backup: %w", err)
	}
	f.Close()
	if err := os.Remove(path); err != nil {
		return BackupResult{}, fmt.Errorf("storage: backup: %w", err)
	}

	// VACUUM INTO refuses to write to an existing file, which is a second guard
	// on top of the check above -- and the reason the placeholder is removed
	// rather than truncated.
	if _, err := s.db.Exec(`VACUUM INTO ?`, path); err != nil {
		return BackupResult{}, fmt.Errorf("storage: backup: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return BackupResult{}, fmt.Errorf("storage: backup: securing %s: %w", path, err)
	}

	// STRIP THE LEASES. Found by the restore test, and it is a real defect
	// rather than tidiness.
	//
	// VACUUM INTO copies every table, owner_lease included, so a backup taken
	// while the guard was running carries that guard's lease -- holder token,
	// pid, hostname and a heartbeat frozen at the moment of the copy. Restore it
	// and the first guard to start is refused with "held by pid 12064 on
	// HarshaLdev, last heartbeat 3 weeks ago", naming a process that has not
	// existed since. It clears itself after leaseTTL, so the outage is short;
	// the confusion, during a restore, is not.
	//
	// A lease describes a RUNNING PROCESS. Nothing about it survives the file
	// meaningfully, so carrying it forward can only mislead.
	if err := clearLeases(path); err != nil {
		return BackupResult{}, err
	}

	fi, err := os.Stat(path)
	if err != nil {
		return BackupResult{}, fmt.Errorf("storage: backup: %w", err)
	}
	res := BackupResult{Path: path, Bytes: fi.Size(), TakenAt: time.Now().UTC()}

	// Verify the copy by OPENING IT, not by trusting that the write returned
	// nil. A backup nobody has read is a hypothesis, and the moment you find out
	// is the moment you cannot afford to.
	if err := verifyBackup(path, &res); err != nil {
		return BackupResult{}, err
	}
	return res, nil
}

// VerifyBackup opens a backup file and reports what it contains.
//
// Exported because verification has to be runnable on its own, against a file
// somebody found on a disk months later, without the original state file
// present. A restore procedure whose first step is "hope" is not a procedure.
func VerifyBackup(path string) (BackupResult, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return BackupResult{}, fmt.Errorf("storage: verify backup: %w", err)
	}
	res := BackupResult{Path: path, Bytes: fi.Size(), TakenAt: fi.ModTime().UTC()}
	if err := verifyBackup(path, &res); err != nil {
		return BackupResult{}, err
	}
	return res, nil
}

func verifyBackup(path string, res *BackupResult) error {
	// Read-only, and NO SCHEMA APPLIED. Running the schema statement would
	// silently upgrade a backup taken by an older build, so a corrupt or
	// truncated file could be "repaired" into one that opens and is wrong.
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return fmt.Errorf("storage: verify backup: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	var check string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil {
		return fmt.Errorf("storage: verify backup: %w", err)
	}
	if check != "ok" {
		return fmt.Errorf("storage: backup %s failed integrity_check: %s", path, check)
	}

	var version int
	if err := db.QueryRow(`SELECT version FROM schema_meta WHERE id = 1`).
		Scan(&version); err != nil {
		return fmt.Errorf("storage: verify backup: no schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("%w: backup %s is version %d, this build understands %d",
			ErrSchemaVersion, path, version, schemaVersion)
	}

	// The counts are the operator's evidence that the backup holds what they
	// think it does. "1.2 MB written" says nothing; "3 mandates, 412 actions, 2
	// awaiting an operator" is something a person can sanity-check.
	if err := db.QueryRow(`SELECT COUNT(*) FROM action_state`).Scan(&res.Actions); err != nil {
		return fmt.Errorf("storage: verify backup: %w", err)
	}
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM action_state WHERE state = 'IN_DOUBT'`).
		Scan(&res.InDoubt); err != nil {
		return fmt.Errorf("storage: verify backup: %w", err)
	}
	// The denial queue only exists from v3. A v2 backup is still a valid backup.
	if version >= 3 {
		if err := db.QueryRow(
			`SELECT COUNT(*) FROM denial WHERE resolution = 'OPEN'`).
			Scan(&res.OpenQueue); err != nil {
			return fmt.Errorf("storage: verify backup: %w", err)
		}
	}
	rows, err := db.Query(`SELECT DISTINCT mandate_id FROM action_state ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("storage: verify backup: %w", err)
	}
	defer rows.Close()
	res.Mandates = nil
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		res.Mandates = append(res.Mandates, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// The operator credential must have survived, or the backup restores a state
	// file in which no human can resolve an IN_DOUBT refund -- exactly the
	// condition the guard refuses to start against.
	var verifier string
	switch err := db.QueryRow(
		`SELECT verifier FROM operator_verifier WHERE id = 1`).Scan(&verifier); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("storage: backup %s carries no operator credential. "+
			"Restoring it would produce a state file the guard refuses to start "+
			"against, and in which no IN_DOUBT refund could ever be resolved", path)
	case err != nil:
		return fmt.Errorf("storage: verify backup: %w", err)
	}
	return nil
}

// clearLeases removes process-liveness state from a backup copy.
//
// It opens the COPY, never the source: the running guard's own lease must stay
// exactly where it is, or taking a backup would release the lease of the
// process taking it.
func clearLeases(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("storage: backup: clear leases: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`DELETE FROM owner_lease`); err != nil {
		return fmt.Errorf("storage: backup: clear leases: %w", err)
	}
	return nil
}
