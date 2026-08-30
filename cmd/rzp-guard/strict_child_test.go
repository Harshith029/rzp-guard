//go:build testhook

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RZP_GUARD_CHILD_CMD runs through `sh -c`. That is deliberate and the recovery
// gate depends on it -- but in a red-team context it is arbitrary command
// execution, and "please use the stub" is advice rather than a boundary.
//
// Strict mode is the boundary. These pin that setting the variable actually
// removes the shell, rather than merely being documented as doing so.

func TestStrictModeIgnoresAnArbitraryChildCommand(t *testing.T) {
	t.Setenv(strictChildEnv, "1")
	// The thing an isolated reviewer must not be able to run by accident.
	t.Setenv("RZP_GUARD_CHILD_CMD", "docker run --rm alpine echo pwned")

	dir := t.TempDir()
	stub := filepath.Join(dir, "stub")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Run from a directory where the strict path resolves.
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(".gotmp", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stub, strictChildPath); err != nil {
		t.Fatal(err)
	}

	c, err := newChild(context.Background(), "k", "s")
	if err != nil {
		t.Fatalf("strict mode with the stub present must succeed: %v", err)
	}
	joined := strings.Join(c.Args, " ")
	if strings.Contains(joined, "docker") || strings.Contains(joined, "pwned") {
		t.Fatalf("strict mode executed RZP_GUARD_CHILD_CMD: %v", c.Args)
	}
	if strings.Contains(joined, "sh") && strings.Contains(joined, "-c") {
		t.Fatalf("strict mode still goes through a shell: %v", c.Args)
	}
	if len(c.Args) != 1 || c.Args[0] != strictChildPath {
		t.Fatalf("strict child is %v, want exactly [%s]", c.Args, strictChildPath)
	}
}

// A strict build with no stub must refuse rather than silently fall back to the
// shell path -- a fallback would make the whole control optional.
func TestStrictModeRefusesWhenTheStubIsAbsent(t *testing.T) {
	t.Setenv(strictChildEnv, "1")
	t.Setenv("RZP_GUARD_CHILD_CMD", "echo anything")

	dir := t.TempDir()
	cwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	if _, err := newChild(context.Background(), "k", "s"); err == nil {
		t.Fatal("strict mode fell back to the shell when the stub was missing; " +
			"a control with a silent fallback is not a control")
	}
}

// Without the flag, the existing behaviour must be untouched: the recovery gate
// and the lifecycle tests legitimately need an arbitrary command.
func TestNonStrictModeStillAcceptsAShellCommand(t *testing.T) {
	t.Setenv(strictChildEnv, "")
	t.Setenv("RZP_GUARD_CHILD_CMD", "cat > /dev/null")

	c, err := newChild(context.Background(), "k", "s")
	if err != nil {
		t.Fatalf("non-strict mode must still work: %v", err)
	}
	if !strings.Contains(strings.Join(c.Args, " "), "cat > /dev/null") {
		t.Fatalf("non-strict child lost its command: %v", c.Args)
	}
}
