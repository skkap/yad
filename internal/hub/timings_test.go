package hub

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	// declared under this name twice; both declarations are shipped code, and
	// every other mention of it outside a test is the thing being caught.
	assertSeamIsTestOnly(t, "SyncFloorForTests", "internal/hub/hub.go", "internal/runner/sync.go")
}

// assertSeamIsTestOnly fails unless name appears in no shipped file of the
// module but the ones that declare it.
func assertSeamIsTestOnly(t *testing.T, name string, declaredIn ...string) {
	t.Helper()
	const root = "../.."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata"):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		case slices.Contains(declaredIn, filepath.ToSlash(strings.TrimPrefix(path, root+string(filepath.Separator)))):
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(raw), name) {
			t.Errorf("%s names %s; only a test may lower the floor", path, name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
