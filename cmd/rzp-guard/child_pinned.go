//go:build !testhook

package main

import (
	"context"
	"os/exec"
)

// PinnedImage is the child, fixed at a DIGEST. The :latest tag is mutable and
// has been observed to lag the repository's main branch by ~6 months with
// renamed tools (evidence/tools_list.json).
//
// The shipped binary has no flag to change this and no way to run an arbitrary
// child command. An operator who wants a different server edits and rebuilds,
// which is a reviewable act; a runtime override would not be.
const PinnedImage = "razorpay/mcp@sha256:435109006d6247103899938cf7b1747ba8be1c1a8a28d452cf9fa8eff506e5c6"

// newChild builds the child process command.
//
// The entrypoint is overridden to invoke the SAME unmodified binary in the SAME
// pinned image with its own documented --toolsets flag. All three documented
// configuration routes are broken in this image; see FAILURES.md F10. The
// override also keeps the keys in the environment rather than the container's
// argv, where the stock entrypoint places them.
func newChild(ctx context.Context, toolsets, keyID, keySecret string) (*exec.Cmd, error) {
	c := exec.CommandContext(ctx, "docker", "run", "--rm", "-i",
		"--entrypoint", "./razorpay-mcp-server",
		"-e", "RAZORPAY_KEY_ID", "-e", "RAZORPAY_KEY_SECRET", PinnedImage,
		"stdio", "--toolsets", toolsets)
	c.Env = append(envWithoutRazorpayKeys(),
		"RAZORPAY_KEY_ID="+keyID,
		"RAZORPAY_KEY_SECRET="+keySecret,
	)
	return c, nil
}
