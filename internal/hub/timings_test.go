package hub

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
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
	assertSeamIsNeverAssignedOutsideTests(t, "SyncFloorForTests")
}

// The lease seam is the same bargain as the floor above and needs the same
// guard: a shipped hub that names a one-second lease loses every run of a
// runner whose network blinked, and every protocol test would still pass.
func TestOnlyTestsReachTheLease(t *testing.T) {
	assertSeamIsNeverAssignedOutsideTests(t, "LeaseForTests")
}

// assertSeamIsNeverAssignedOutsideTests fails when any shipped file of the
// module assigns name or takes its address. Reading it is what the clamp does;
// writing it is what only a test may do, and the file that declares it is not
// exempt — an init or a setter beside the declaration is exactly where someone
// would write one. Matching the identifier is a backstop, not a proof: a value
// reached some other way is beyond what reading the source can see.
func assertSeamIsNeverAssignedOutsideTests(t *testing.T, name string) {
	t.Helper()
	written := regexp.MustCompile(`&\s*` + name + `\b|\b` + name + `\s*(?:[-+*/%|&^]|<<|>>)?=[^=]`)
	const root = "../.."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata"):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := written.FindIndex(raw); loc != nil {
			line := 1 + strings.Count(string(raw[:loc[0]]), "\n")
			t.Errorf("%s:%d assigns %s; only a test may lower the floor", path, line, name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
