package capability

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
)

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
