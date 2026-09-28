package capability

import (
	"slices"
	"testing"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/claude"
	"github.com/skkap/yad/internal/adapter/codex"
	"github.com/skkap/yad/internal/harness"
)

// fork is advertised by every build, so it is true only while every
// first-class harness's adapter can open a session as a fork. A harness made
// first-class without it would take forks from hubs and refuse each one
// (decision 0064).
func TestForkIsAdvertisedOnlyWhileEveryAdapterForks(t *testing.T) {
	if !slices.Contains(Features(), FeatureFork) {
		t.Fatalf("features %v do not advertise %q", Features(), FeatureFork)
	}
	// The adapters `yad daemon` registers.
	adapters := map[string]adapter.Adapter{"claude": claude.Adapter{}, "codex": codex.Adapter{}}
	for _, h := range harness.Catalog() {
		if h.Kind != harness.FirstClass {
			continue
		}
		a, ok := adapters[h.ID]
		if !ok {
			t.Fatalf("first-class harness %s has no adapter here — add the one yad daemon registers", h.ID)
		}
		if !adapter.Forks(a) {
			t.Errorf("the %s adapter cannot fork a session, and this runner advertises %q: stop advertising it, or teach the adapter", h.ID, FeatureFork)
		}
	}
}
