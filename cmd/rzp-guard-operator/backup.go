package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/harshith/rzp-guard/internal/opauth"
	"github.com/harshith/rzp-guard/internal/storage"
)

// BACKUP AND RESTORE, which the engineering audit scored 1/10: "None. No
// documented procedure, no RPO or RTO."
//
// The RPO and the RTO are stated in OPERATIONS.md rather than here, because
// both are decided by how often somebody runs this, not by what it does. What
// this file provides is the two things a stated objective needs to be more than
// a number: a backup that can be taken from a LIVE system, and verification
// that can be run on the copy without the original.

// cmdBackup writes a consistent copy of the state file.
//
// It requires the operator credential like every other command here, and for
// the same reason list and audit do: the copy carries payment ids, receipts,
// amounts, the audit trail and the Argon2id verifier itself. A backup command
// anyone local could run is a credential-exfiltration command with a reassuring
// name.
func cmdBackup(store *storage.Store, g opauth.Grant, out string, asJSON bool) error {
	if !g.Valid() {
		return errors.New("backup requires a verified operator credential: the copy " +
			"carries the operator verifier and every payment id this guard has seen")
	}
	if out == "" {
		// A timestamped default, because the one thing a backup must not do is
		// overwrite yesterday's.
		out = filepath.Join("backups",
			fmt.Sprintf("rzp-guard-%s.db", time.Now().UTC().Format("20060102T150405Z")))
	}
	res, err := store.Backup(out)
	if err != nil {
		if errors.Is(err, storage.ErrBackupExists) {
			return fmt.Errorf("%w.\n  Refusing to overwrite: this is the file you "+
				"reach for when something has gone wrong, and replacing it with a "+
				"copy of a database that may itself be damaged is how the good copy "+
				"is lost", err)
		}
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	printBackup("Wrote", res)
	fmt.Println("\n  Taken from a LIVE state file: the guard, if one is running, was")
	fmt.Println("  not interrupted and did not lose its lease. The copy carries no")
	fmt.Println("  lease of its own, so restoring it starts cleanly.")
	fmt.Println("\n  To restore: stop the guard, copy this file over the state file,")
	fmt.Println("  start the guard. Verify first:")
	fmt.Printf("    rzp-guard-operator -mandate M -state S verify-backup -out %s\n", res.Path)
	return nil
}

// cmdVerifyBackup opens a backup and reports what is in it.
//
// It does NOT require the state file to be healthy, or present. That is the
// whole point: the moment this is needed is the moment the original is gone.
func cmdVerifyBackup(path string, asJSON bool) error {
	if path == "" {
		return errors.New("-out is the backup file to verify")
	}
	res, err := storage.VerifyBackup(path)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	printBackup("Verified", res)
	fmt.Println("\n  integrity_check passed, the schema version is one this build")
	fmt.Println("  understands, and an operator credential is present -- without which")
	fmt.Println("  a restored file is one the guard refuses to start against.")
	return nil
}

// printBackup shows the counts rather than only the size. "1.2 MB written" is
// not something a person can sanity-check; "3 mandates, 412 actions, 2 awaiting
// an operator" is.
func printBackup(verb string, r storage.BackupResult) {
	fmt.Printf("%s %s (%d bytes)\n", verb, r.Path, r.Bytes)
	fmt.Printf("  mandates:            %v\n", r.Mandates)
	fmt.Printf("  actions recorded:    %d\n", r.Actions)
	fmt.Printf("  awaiting an operator: %d\n", r.InDoubt)
	fmt.Printf("  refusals unreviewed:  %d\n", r.OpenQueue)
}
