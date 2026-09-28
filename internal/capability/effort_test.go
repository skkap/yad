package capability

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/claude"
	"github.com/skkap/yad/internal/adapter/codex"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
)

// effort is advertised by every build, so it is true only while every
// first-class harness's adapter hands a run's effort over. A harness made
// first-class without it would take effort runs from hubs and refuse each one.
func TestEffortIsAdvertisedOnlyWhileEveryAdapterAppliesIt(t *testing.T) {
	if !slices.Contains(Features(), FeatureEffort) {
		t.Fatalf("features %v do not advertise %q", Features(), FeatureEffort)
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
		if !adapter.AppliesEffort(a) {
			t.Errorf("the %s adapter does not apply a run's effort, and this runner advertises %q: stop advertising it, or teach the adapter", h.ID, FeatureEffort)
		}
	}
}

// A Claude that does not know a flag every run passes is reported unable to
// take runs, so no hub offers it one it would fail at its arguments; a current
// one is drivable (decision 0050).
func TestAClaudeWithoutTheFlagsRunsNeedIsNotDrivable(t *testing.T) {
	for _, tc := range []struct {
		name, help string
		drivable   bool
	}{
		{"current", "  --system-prompt-snapshot <on|off>  --fork-session", true},
		{"too old", "  --append-system-prompt <prompt>", false},
		// A Claude that cannot fork would take the forks this runner
		// advertises it can (decision 0065).
		{"unforking", "  --system-prompt-snapshot <on|off>", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noTools(t)
			bin := filepath.Join(t.TempDir(), "claude")
			script := "#!/bin/sh\ncase \"$1\" in\n--help) echo '" + tc.help + "' ;;\n*) echo '2.1.1-" + tc.name[:3] + " (Claude Code)' ;;\nesac\n"
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("YAD_CLAUDE_PATH", bin)
			doc := v1.Capabilities{Harnesses: Harnesses(Detect(context.Background()), config.Default(), nil)}
			if got := Drivable(doc, "claude"); got != tc.drivable {
				t.Fatalf("drivable = %v, want %v: %+v", got, tc.drivable, doc.Harnesses[0])
			}
			if !tc.drivable && !strings.Contains(doc.Harnesses[0].Error, "update") {
				t.Errorf("error %q does not say to upgrade claude", doc.Harnesses[0].Error)
			}
		})
	}
}
