package hub

import (
	"testing"
	"time"

	"github.com/skkap/yad/internal/seamtest"
)

// What a shipped hub clamps to, asserted beside the seam that lowers it for
// tests: the floor is what keeps a lease outlasting the runner's backoff, and a
// test that runs the protocol in milliseconds must not take that with it.
func TestTheShippedSyncFloorIsFiveSeconds(t *testing.T) {
	if SyncFloorForTests != 0 {
		t.Fatalf("a test left the floor seam set to %s", SyncFloorForTests)
	}
	if LeaseForTests != 0 {
		t.Fatalf("a test left the lease seam set to %s", LeaseForTests)
	}
	if MinSyncInterval != 5*time.Second {
		t.Errorf("MinSyncInterval = %s, want 5s", MinSyncInterval)
	}
	h := New(Options{SyncInterval: 50 * time.Millisecond})
	if h.interval != 5*time.Second {
		t.Errorf("a hub asked for 50ms names %s, want the 5s floor", h.interval)
	}
	if h.lease != minLease {
		t.Errorf("lease = %s, want %s", h.lease, minLease)
	}
}

// Nothing but this test stands between the seam and a shipped hub that syncs
// every millisecond: lowering the floor in a production path would leave every
// protocol test passing, because the protocol still works — it is the machine
// running yad that pays. So the check is the source itself.
func TestOnlyTestsReachTheSyncFloor(t *testing.T) {
	// The protocol has two ends and each holds its own floor, so the seam is
	// declared under this name in both packages; this walks the whole module,
	// so either package's would be caught here.
	for _, at := range seamtest.Writes(t, "../..", "SyncFloorForTests") {
		t.Errorf("%s writes SyncFloorForTests; only a test may lower the floor", at)
	}
}

// The lease seam is the same bargain as the floor above and needs the same
// guard: a shipped hub that names a one-second lease loses every run of a
// runner whose network blinked, and every protocol test would still pass.
func TestOnlyTestsReachTheLease(t *testing.T) {
	for _, at := range seamtest.Writes(t, "../..", "LeaseForTests") {
		t.Errorf("%s writes LeaseForTests; only a test may shorten the lease", at)
	}
}
