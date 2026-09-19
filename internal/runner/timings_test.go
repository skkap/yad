package runner

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// What a shipped runner holds a hub to, asserted beside the seam that lowers it
// for tests: the floor is what stops a hostile or broken hub spinning the
// machine, and a test that drives the protocol in milliseconds is not a reason
// to ship a runner that will.
func TestTheShippedSyncFloorIsFiveSeconds(t *testing.T) {
	if SyncFloorForTests != 0 {
		t.Fatalf("a test left the floor seam set to %s", SyncFloorForTests)
	}
	if minInterval != 5*time.Second {
		t.Errorf("minInterval = %s, want 5s", minInterval)
	}
	for _, tc := range []struct {
		name string
		ms   int
		want time.Duration
	}{
		{"a hub naming a millisecond", 1, minInterval},
		{"a hub naming 50ms", 50, minInterval},
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
