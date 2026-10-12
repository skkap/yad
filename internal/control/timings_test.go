//go:build unix

package control

import (
	"testing"
	"time"

	"github.com/skkap/yad/internal/seamtest"
)

// What a shipped yad waits for an answer, asserted beside the seam that lowers
// it for tests. Asserted as the value rather than by waiting it out, because
// the five seconds the seam exists to save are exactly what such a test would
// spend; what the value protects is described where it is declared.
func TestTheShippedAskTimeoutIsFiveSeconds(t *testing.T) {
	if AskTimeoutForTests != 0 {
		t.Fatalf("a test left the ask seam set to %s", AskTimeoutForTests)
	}
	if askTimeout != 5*time.Second {
		t.Errorf("askTimeout = %s, want 5s", askTimeout)
	}
}

// A shipped yad that gave up on the socket sooner would call a daemon merely
// busy with a sync wedged, and signal where it should have asked. The seam that makes
// the stop tests fast must not travel into the binary, and the check is the
// source: any shipped file that writes it, this package's own included.
func TestOnlyTestsReachTheAskTimeout(t *testing.T) {
	for _, at := range seamtest.Writes(t, "../..", "AskTimeoutForTests") {
		t.Errorf("%s writes AskTimeoutForTests; only a test may shorten it", at)
	}
}
