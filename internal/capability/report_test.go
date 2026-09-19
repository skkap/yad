package capability

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
)

func TestFingerprintIgnoresTimeOnly(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := config.Default()
	a := Build(context.Background(), "r1", cfg, nil)
	b := a
	b.ObservedAt = a.ObservedAt.Add(time.Hour)
	if Fingerprint(a) != Fingerprint(b) {
		t.Error("a timestamp change moved the fingerprint")
	}
	b.Labels = []string{"gpu"}
	if Fingerprint(a) == Fingerprint(b) {
		t.Error("a label change did not move the fingerprint")
	}
}

// Label order is the owner's typing order, not a change in the machine.
func TestLabelOrderDoesNotMoveFingerprint(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := config.Default()
	cfg.Labels = []string{"b", "a"}
	x := Build(context.Background(), "r", cfg, nil)
	cfg.Labels = []string{"a", "b"}
	if Fingerprint(x) != Fingerprint(Build(context.Background(), "r", cfg, nil)) {
		t.Error("label order moved the fingerprint")
	}
}

// The document is public: it must carry account labels and never the binary
// path lookup internals or anything credential-shaped.
func TestDocumentIsPublicSafe(t *testing.T) {
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"personal"}, Cap: 2}}
	found := []harness.Detected{{Harness: harness.Catalog()[0], Present: true, Path: "/x/claude", Version: "2.1"}}
	reps := Harnesses(found, cfg, nil)
	b, _ := json.Marshal(reps)
	s := string(b)
	if !strings.Contains(s, `"label":"personal"`) {
		t.Errorf("account label missing: %s", s)
	}
	for _, leak := range []string{"YAD_CLAUDE_PATH", "/x/claude", "--version"} {
		if strings.Contains(s, leak) {
			t.Errorf("document leaks %q: %s", leak, s)
		}
	}
}
