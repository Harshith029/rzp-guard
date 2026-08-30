// Package buildinfo reports what this binary actually is.
//
// A deployed process that cannot say which commit it came from is a process
// nobody can reason about during an incident. "Which version is running?" is
// the first question after "what broke", and until now this project had no
// answer: the binaries carried no version, no commit and no build date.
//
// Values are injected at link time by run.sh and the release workflow:
//
//	-ldflags "-X github.com/harshith/rzp-guard/internal/buildinfo.Version=v0.1.0 ..."
//
// They deliberately default to "dev"/"unknown" rather than to a plausible
// version string. A binary built without stamping must be obviously unstamped,
// not quietly claim to be a release.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

var (
	// Version is the release tag, or "dev" for an unstamped build.
	Version = "dev"
	// Commit is the git revision. Falls back to the VCS stamp Go embeds.
	Commit = ""
	// BuildDate is RFC3339 UTC, or empty when unstamped.
	BuildDate = ""
)

// Commitish returns the best available revision string.
//
// Go embeds VCS metadata automatically, but this repository builds with
// -buildvcs=false everywhere (a .git Go can find but not read fails the build
// outright -- see FAILURES.md), so that fallback is usually absent and the
// link-time value is what matters.
func Commitish() string {
	if Commit != "" {
		return Commit
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return "unknown"
}

// Short renders a one-line identity for logs and status files.
func Short() string {
	c := Commitish()
	if len(c) > 12 {
		c = c[:12]
	}
	return fmt.Sprintf("%s (%s)", Version, c)
}

// String renders the full identity for a -version flag.
func String(program string) string {
	date := BuildDate
	if date == "" {
		date = "unknown"
	}
	return fmt.Sprintf("%s %s\n  commit:   %s\n  built:    %s\n  go:       %s\n  platform: %s/%s",
		program, Version, Commitish(), date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
