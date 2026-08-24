//go:build !testhook

package main

// allowUnprotectedFlag is absent from shipped builds.
//
// A typed acknowledgement is not evidence that a directory is protected, and a
// production flag that bypasses the 0600 verification contradicts the guarantee
// the README states. The escape hatch exists only under -tags testhook, for
// gates writing into throwaway scratch directories.
func allowUnprotectedFlag() *bool {
	no := false
	return &no
}

const unprotectedHelp = ""
