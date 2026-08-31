package main

import (
	"net/http"
	"testing"
	"time"
)

// Arm C lost 108 of 162 traces to HTTP 429, and the loss correlated with a
// grid dimension because rate-limiting builds up as the runner walks the
// corpus in order. These are the properties the re-run depends on.

func TestRetryableStatus(t *testing.T) {
	retry := []int{http.StatusTooManyRequests, 500, 502, 503, 504}
	for _, c := range retry {
		if !retryableStatus(c) {
			t.Errorf("HTTP %d should be retried", c)
		}
	}
	// A bad request stays bad; retrying it only burns budget and time.
	never := []int{400, 401, 403, 404, 422}
	for _, c := range never {
		if retryableStatus(c) {
			t.Errorf("HTTP %d must NOT be retried", c)
		}
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	var prev time.Duration
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		d := backoffFor(attempt, "")
		if d <= 0 {
			t.Fatalf("attempt %d: non-positive backoff %s", attempt, d)
		}
		if d > maxBackoff+time.Second {
			t.Errorf("attempt %d: backoff %s exceeds the cap %s", attempt, d, maxBackoff)
		}
		if attempt > 1 && d <= prev-time.Second && prev < maxBackoff {
			t.Errorf("attempt %d: backoff %s did not grow from %s", attempt, d, prev)
		}
		prev = d
	}
}

// A server that says how long to wait is more informative than our guess.
func TestBackoffHonoursRetryAfter(t *testing.T) {
	d := backoffFor(1, "30")
	if d != 30*time.Second {
		t.Errorf("Retry-After 30 gave %s, want 30s", d)
	}
	// Absurd or unparseable values fall back to our own schedule rather than
	// letting an endpoint park the run for an hour.
	for _, bad := range []string{"", "soon", "-5", "99999"} {
		if got := backoffFor(1, bad); got > maxBackoff+time.Second {
			t.Errorf("Retry-After %q gave %s, above the cap", bad, got)
		}
	}
}
