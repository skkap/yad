package main

import (
	"regexp"
	"testing"
)

// A fork submitted without --new-session gets a session named for it. With a
// --run-id, the name is the same on every submit, so a retry is the run the
// hub already queued rather than one refused as different; without one, each
// submit is a new run and gets a new session (decision 0065).
func TestAForksSessionIDFollowsItsRunID(t *testing.T) {
	id := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	a, again, other := forkSessionID("r1"), forkSessionID("r1"), forkSessionID("r2")
	if a != again || a == other {
		t.Errorf("run r1 gets %s then %s, run r2 %s: want one id per run id", a, again, other)
	}
	x, y := forkSessionID(""), forkSessionID("")
	if x == y {
		t.Errorf("two submits without a run id share session %s", x)
	}
	for _, s := range []string{a, other, x} {
		if !id.MatchString(s) {
			t.Errorf("session id %q is not one the hub takes", s)
		}
	}
}
