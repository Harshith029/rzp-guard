package main

import "testing"

// A demo that has quietly stopped demonstrating anything is worse than no demo,
// because it is the first thing anyone runs. run() fails if the narration ever
// disagrees with what actually reached the child, or if the tampered mandate
// verifies -- so running it here means CI catches a demo that lies.
func TestTheDemoStillDemonstratesWhatItSays(t *testing.T) {
	if err := run(); err != nil {
		t.Fatalf("the demo no longer holds: %v", err)
	}
}
