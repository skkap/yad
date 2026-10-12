package runner

import (
	"testing"
	"time"

	"github.com/skkap/yad/internal/seamtest"
)

// What a shipped runner holds a hub to, asserted beside the seam that lowers it
// for tests: the floor is what stops a hostile or broken hub spinning the
// machine, and a test that drives the protocol in milliseconds is not a reason
// to ship a runner that will.
func TestTheShippedSyncFloorIsThreeSeconds(t *testing.T) {
	if SyncFloorForTests != 0 {
		t.Fatalf("a test left the floor seam set to %s", SyncFloorForTests)
	}
	if minInterval != 3*time.Second {
		t.Errorf("minInterval = %s, want 3s", minInterval)
	}
	for _, tc := range []struct {
		name string
		ms   int
		want time.Duration
	}{
		{"a hub naming a millisecond", 1, minInterval},
		{"a hub naming 50ms", 50, minInterval},
		{"a hub holding a run for it", 3000, 3 * time.Second},
		{"a hub naming its steady floor", 5000, 5 * time.Second},
		{"a hub naming an hour", 3_600_000, maxInterval},
		{"a hub naming nothing", 0, defaultInterval},
	} {
		if got := interval(tc.ms); got != tc.want {
			t.Errorf("%s is synced every %s, want %s", tc.name, got, tc.want)
		}
	}
}

// Nothing but this test stands between the seam and a shipped runner that a hub
// can make sync every millisecond: the sync loop would still pass every test it
// has, because syncing works — it is the machine running yad that pays. So the
// check is the source itself.
func TestOnlyTestsReachTheSyncFloor(t *testing.T) {
	// The protocol has two ends and each holds its own floor, so the seam is
	// declared under this name in both packages; this walks the whole module,
	// so either package's would be caught here.
	for _, at := range seamtest.Writes(t, "../..", "SyncFloorForTests") {
		t.Errorf("%s writes SyncFloorForTests; only a test may lower the floor", at)
	}
}
