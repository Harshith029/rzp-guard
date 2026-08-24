package opauth

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// DirSyncSupported reports whether this platform can fsync a directory, which
// is what makes a newly created file's DIRECTORY ENTRY durable.
//
// Syncing the file alone is not enough: after a power loss the file's contents
// can be on disk while the entry naming it is not. If the verifier is committed
// in that window, the credential exists and the token does not -- the same
// permanent lockout, moved to the filesystem-durability boundary.
func DirSyncSupported() bool { return runtime.GOOS != "windows" }

// syncParentDir fsyncs the directory containing path.
//
// Windows does not support opening a directory as a file for syncing, so this
// durability step CANNOT be guaranteed there. Callers must surface that rather
// than pretend the write is crash-safe.
func syncParentDir(path string) error {
	if !DirSyncSupported() {
		return errUnsupportedDirSync
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open parent directory: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("sync parent directory: %w", err)
	}
	return nil
}
