package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Open now RETRIES a contended lock instead of failing on the first attempt.
// That is a real change to a startup path, so the three things it must not
// break are asserted here rather than argued: the wait is bounded, a decision
// is never retried, and waiting never costs the incumbent its ownership.
//
// The reason the retry exists is in Open's own comment: under
// locking_mode = EXCLUSIVE, simultaneous openers can all hold SHARED and none
// can upgrade, so a single attempt yields ZERO owners rather than one.

// The wait has an upper bound and the answer is still a named ownership
// conflict, not an anonymous timeout string.
func TestAContendedOpenGivesUpWithinItsDeadlineAndNamesTheConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held.db")
	incumbent, err := Open(path, "mnd_incumbent")
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	defer incumbent.Close()

	const deadline = 200 * time.Millisecond
	start := time.Now()
	second, err := openWithDeadline(path, "mnd_incumbent", deadline)
	elapsed := time.Since(start)
	if err == nil {
		second.Close()
		t.Fatal("a second instance opened a state file another instance owns")
	}
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("refused with %v, want ErrNotOwner: a caller must be able to "+
			"tell contention from a corrupt or unreadable state file", err)
	}
	if elapsed < deadline {
		t.Fatalf("gave up after %v, before its own %v deadline: the retry is "+
			"not actually waiting", elapsed, deadline)
	}
	// Generous, because this must hold under -race on a constrained CPU. It
	// still fails if a loser ever waits without a bound, which is the property
	// under test.
	if max := 10*deadline + 5*time.Second; elapsed > max {
		t.Fatalf("waited %v for a %v deadline (bound %v): the retry loop is "+
			"not bounded", elapsed, deadline, max)
	}

	// Losing a takeover must not cost the incumbent its lock, or the guard
	// would fail closed on every refund after someone rattled the door.
	if err := incumbent.Reserve("rfa_001", "rzpg_aaaaaaaaaaaa", 5000); err != nil {
		t.Fatalf("the incumbent could not write after refusing a takeover: %v", err)
	}
}

// A retry must not delay an answer this build has already reached. A state file
// at an unsupported schema version is a decision, not contention: retrying it
// for the full deadline would turn a clear refusal into a slow one and, worse,
// would report it as an ownership conflict.
func TestOpenDoesNotRetryADecisionItHasAlreadyMade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	s, err := Open(path, "mnd_version")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	s.Close()
	setVersion(t, path, schemaVersion+1)

	start := time.Now()
	_, err = Open(path, "mnd_version")
	elapsed := time.Since(start)
	if !errors.Is(err, ErrSchemaVersion) {
		t.Fatalf("got %v, want ErrSchemaVersion", err)
	}
	if elapsed >= lockAcquireDeadline {
		t.Fatalf("a schema-version refusal took %v, at or past the %v lock "+
			"deadline: it is being retried as if it were contention",
			elapsed, lockAcquireDeadline)
	}
}
