//go:build redteam

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The previous attempt at this guarantee was an environment switch that
// Stat'd a writable RELATIVE path and executed whatever was there. The test
// written to prove it worked actually proved the opposite: it created a shell
// script, renamed it to the strict path, and strict mode ran it.
//
// These test the guarantee this build actually makes, which is narrower and
// therefore true: no shell branch exists, the path is absolute, and a symlink
// or a non-regular file is refused rather than followed.

func withChildDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Dir(redteamChildPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// RemoveAll, not Remove: one test below deliberately creates a DIRECTORY at
	// this path, and os.Remove fails on a non-empty one. These tests share a
	// fixed absolute path because the constant is fixed by design, so leftover
	// state from one leaks into the next. A single unreproduced failure in this
	// package pointed here; this is the most likely cause, hardened rather than
	// dismissed.
	_ = os.RemoveAll(redteamChildPath)
	t.Cleanup(func() { _ = os.RemoveAll(redteamChildPath) })
	return dir
}

func TestRedteamChildIsAnAbsolutePath(t *testing.T) {
	if !filepath.IsAbs(redteamChildPath) {
		t.Fatalf("child path %q is relative; it would resolve against whatever "+
			"directory the process happens to be in, which is what the previous "+
			"attempt got wrong", redteamChildPath)
	}
}

func TestRedteamChildRefusesASymlink(t *testing.T) {
	dir := withChildDir(t)

	// Something else entirely, pointed at by the name the constant names.
	target := filepath.Join(dir, "not-the-stub")
	if err := os.WriteFile(target, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(target) })
	if err := os.Symlink(target, redteamChildPath); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	_, err := newChild(context.Background(), "k", "s")
	if err == nil {
		t.Fatal("followed a symlink; the executed file would then be chosen by " +
			"whoever created the link rather than by the constant")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("error does not name the cause: %v", err)
	}
}

func TestRedteamChildRefusesWhenAbsent(t *testing.T) {
	withChildDir(t)

	_, err := newChild(context.Background(), "k", "s")
	if err == nil {
		t.Fatal("accepted a missing child; a fallback here would make the whole " +
			"control optional")
	}
	if !strings.Contains(err.Error(), "not present") {
		t.Fatalf("error does not name the cause: %v", err)
	}
}

func TestRedteamChildRefusesANonRegularFile(t *testing.T) {
	withChildDir(t)
	if err := os.Mkdir(redteamChildPath, 0o755); err != nil {
		t.Skipf("cannot create a directory there: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(redteamChildPath) })

	if _, err := newChild(context.Background(), "k", "s"); err == nil {
		t.Fatal("accepted a directory as the child")
	}
}

// The positive case: a regular file at the exact path is executed directly,
// with no shell and no arguments.
func TestRedteamChildExecsExactlyThePathWithNoShell(t *testing.T) {
	withChildDir(t)
	if err := os.WriteFile(redteamChildPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	c, err := newChild(context.Background(), "k", "s")
	if err != nil {
		t.Fatalf("refused a regular file at the exact path: %v", err)
	}
	if len(c.Args) != 1 || c.Args[0] != redteamChildPath {
		t.Fatalf("child is %v, want exactly [%s]", c.Args, redteamChildPath)
	}
	// Whatever else is true, it must not have gone through a shell.
	for _, a := range c.Args {
		if a == "-c" || strings.HasSuffix(a, "/sh") || a == "sh" {
			t.Fatalf("child went through a shell: %v", c.Args)
		}
	}
	// And it must not carry Razorpay credentials.
	for _, kv := range c.Env {
		if strings.HasPrefix(kv, "RAZORPAY_KEY_ID=") || strings.HasPrefix(kv, "RAZORPAY_KEY_SECRET=") {
			t.Fatalf("child environment carries a Razorpay credential: %q", kv)
		}
	}
}

// This build must not consult the variable at all -- not even to ignore it.
func TestRedteamChildIgnoresTheShellVariableEntirely(t *testing.T) {
	withChildDir(t)
	if err := os.WriteFile(redteamChildPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RZP_GUARD_CHILD_CMD", "docker run --rm alpine echo pwned")

	c, err := newChild(context.Background(), "k", "s")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(c.Args, " ")
	if strings.Contains(joined, "docker") || strings.Contains(joined, "pwned") {
		t.Fatalf("the shell variable reached the child: %v", c.Args)
	}
}
