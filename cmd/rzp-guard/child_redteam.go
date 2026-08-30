//go:build redteam

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// The child for external red-team work: ONE absolute path, no shell, no
// environment, no fallback.
//
// WHY THIS IS A SEPARATE FILE RATHER THAN A FLAG.
//
// The previous attempt was RZP_GUARD_CHILD_STRICT, an environment variable
// consulted at runtime. Review demolished it on two counts and both were right:
// anything running inside the lane could unset it and restore the `sh -c` path,
// and even when set it merely Stat'd a WRITABLE RELATIVE path and executed
// whatever was there -- symlinks followed, no identity check. The unit test
// written to prove strict mode worked actually proved the opposite: it created
// a shell script, renamed it to the strict path, and strict mode ran it.
//
// So the guarantee moved from a runtime check to a compile-time one. Under
// `-tags redteam` the shell branch does not exist in the binary at all. There
// is nothing to unset.
//
// WHAT IS AND IS NOT GUARANTEED, stated exactly, because overstating this is
// what the last three review rounds were about:
//
//	GUARANTEED  this binary contains no code path that runs a shell, reads
//	            RZP_GUARD_CHILD_CMD, or resolves a child from configuration.
//	GUARANTEED  the child path is absolute and fixed at compile time, so it
//	            does not depend on the working directory.
//	NOT         that the file at that path is the real mcp-stub. Anyone who can
//	            write to it can substitute it. The lane addresses that by
//	            building the stub itself, inside a container with no network,
//	            immediately before use -- not by trusting the filesystem.
//
// The honest summary: this removes the accident, not a determined operator who
// controls the machine. The isolation that matters for a red-team -- no
// credentials, no network, no Docker socket -- comes from the container.
const redteamChildPath = "/tmp/rzp-redteam-child/mcp-stub"

// Reported by -version, like the other child builds. Named so an operator
// reading the output cannot mistake this binary for a shipped one.
const PinnedImage = "(redteam build: no container, child is " + redteamChildPath + ")"
const Toolsets = "(redteam build: offline stub only, NEVER for Razorpay)"

func newChild(ctx context.Context, keyID, keySecret string) (*exec.Cmd, error) {
	if !filepath.IsAbs(redteamChildPath) {
		// Unreachable by construction; asserted because a relative path is
		// exactly the defect this file exists to remove.
		return nil, fmt.Errorf("redteam child path %q is not absolute", redteamChildPath)
	}
	fi, err := os.Lstat(redteamChildPath)
	if err != nil {
		return nil, fmt.Errorf("redteam build: the only permitted child is %s and "+
			"it is not present: %w (build it with "+
			"`go build -o %s ./cmd/mcp-stub`)",
			redteamChildPath, err, redteamChildPath)
	}
	// Lstat, not Stat: a symlink here would mean the executed file is chosen by
	// whoever created the link rather than by this constant.
	if fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("redteam build: %s is a symlink; the child must be "+
			"a regular file so that the path names what runs", redteamChildPath)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("redteam build: %s is not a regular file (%s)",
			redteamChildPath, fi.Mode())
	}

	fmt.Fprintln(os.Stderr,
		"rzp-guard: REDTEAM BUILD -- child is "+redteamChildPath+
			"; there is no shell path in this binary")
	c := exec.CommandContext(ctx, redteamChildPath)
	// The stub talks to nobody and has no business holding credentials.
	c.Env = envWithoutRazorpayKeys()
	return c, nil
}
