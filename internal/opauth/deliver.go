package opauth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/term"
)

var (
	ErrDestinationExists = errors.New("destination already exists")

	errUnsupportedDirSync = errors.New("this platform cannot fsync a directory")
)

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
// O_EXCL means an EXISTING FINAL PATH is never touched: a typo cannot destroy
// another secret. Verified before the fix, when -out over a file holding other
// content replaced it outright.
//
// Scope of that guarantee, stated precisely: it covers the final path only. It
// does NOT establish that parent directories are free of symlinks or Windows
// reparse points, nor that the resolved directory is private. Canonical-path
// and platform-specific reparse checks would be needed for that, and are not
// implemented.
//
// The file is fsynced, and so is its parent directory where the platform allows
// it -- syncing the file alone leaves the directory entry itself vulnerable to
// a crash. durable reports whether that second step actually happened.
func WriteTokenExclusive(path, token string, allowUnprotected bool) (durable bool, err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return false, fmt.Errorf("%w: %s (refusing to overwrite; choose a new path)",
				ErrDestinationExists, path)
		}
		return false, fmt.Errorf("create token file: %w", err)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		f.Close()
		os.Remove(path)
		return false, fmt.Errorf("write token file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return false, fmt.Errorf("sync token file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return false, fmt.Errorf("close token file: %w", err)
	}

	// Verify the permission bits actually took, rather than assuming they did.
	// Windows does not honour Unix mode bits: measured, the file lands 0644.
	// "Directory ACLs are the boundary" is not a control the program enforces,
	// so refuse rather than imply protection that is not there.
	fi, err := os.Stat(path)
	if err != nil {
		os.Remove(path)
		return false, fmt.Errorf("stat token file: %w", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 && !allowUnprotected {
		os.Remove(path)
		return false, fmt.Errorf("token file landed with mode %04o, not 0600, so this "+
			"platform did not apply the restriction (Windows does not honour Unix "+
			"mode bits). %s is therefore not protected by this program. Provision "+
			"interactively, or use an OS secret store", perm, filepath.Dir(path))
	}

	// Make the directory entry durable. A failure here is reported, never
	// swallowed: the caller must not commit a credential believing the token is
	// safely on disk when it might not survive a crash.
	if err := syncParentDir(path); err != nil {
		if errors.Is(err, errUnsupportedDirSync) {
			return false, nil // caller warns; see DirSyncSupported
		}
		os.Remove(path)
		return false, err
	}
	return true, nil
}
