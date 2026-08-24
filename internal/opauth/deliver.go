package opauth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/term"
)

// ErrDestinationExists is returned when a token destination already exists.
var ErrDestinationExists = errors.New("destination already exists")

// StdoutIsTerminal reports whether stdout is a real interactive terminal.
//
// os.ModeCharDevice was not good enough: it is satisfied by devices that are not
// consoles, and Windows handle semantics differ. A false positive is not
// cosmetic here -- it means the verifier gets committed while the only copy of
// the token goes to a sink nobody reads, which is the lockout this package now
// exists to prevent.
func StdoutIsTerminal() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// WriteTokenExclusive creates path and writes the token to it.
//
// O_EXCL means an existing file is NEVER touched: a typo cannot destroy another
// secret, and the open fails rather than following a symlink to somewhere the
// token would be disclosed. Verified before the fix: -out over a file holding
// other content replaced it outright.
//
// The file is fsynced before return, so a caller that commits a credential
// afterwards knows the token is actually on disk.
func WriteTokenExclusive(path, token string, allowUnprotected bool) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%w: %s (refusing to overwrite; choose a new path)",
				ErrDestinationExists, path)
		}
		return fmt.Errorf("create token file: %w", err)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("write token file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("sync token file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("close token file: %w", err)
	}

	// Verify the permission bits actually took, rather than assuming they did.
	// Windows does not honour Unix mode bits: measured, the file lands 0644.
	// "Directory ACLs are the boundary" is not a control the program enforces,
	// so refuse rather than imply protection that is not there.
	fi, err := os.Stat(path)
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("stat token file: %w", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 && !allowUnprotected {
		os.Remove(path)
		return fmt.Errorf("token file landed with mode %04o, not 0600, so this "+
			"platform did not apply the restriction (Windows does not honour Unix "+
			"mode bits). Write it interactively instead, or re-run with "+
			"-allow-unprotected-out once %s is on a directory you have restricted "+
			"yourself", perm, filepath.Dir(path))
	}
	return nil
}
