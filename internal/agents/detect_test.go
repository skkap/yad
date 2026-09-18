package agents

import (
	"context"
	"testing"
)

func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"bare", "2.4.1\n", "2.4.1"},
		{"banner", "claude 2.4.1 (Claude Code)\n", "claude 2.4.1 (Claude Code)"},
		{"update notice below", "codex 0.58.0\n\nA new version is available\n", "codex 0.58.0"},
		{"leading blank line", "\n  opencode 1.2.3  \n", "opencode 1.2.3"},
		{"empty", "\n\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseVersion(tc.raw); got != tc.want {
				t.Errorf("ParseVersion(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// A machine with nothing installed must still produce a full, ordered report —
// absence is a capability fact, not a failure.
func TestDetectReportsAbsentAgents(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got := Detect(context.Background())
	if len(got) != len(Catalog()) {
		t.Fatalf("Detect returned %d entries, want %d", len(got), len(Catalog()))
	}
	for i, d := range got {
		if d.ID != Catalog()[i].ID {
			t.Errorf("entry %d is %q, want catalog order %q", i, d.ID, Catalog()[i].ID)
		}
		if d.Present {
			t.Errorf("%s reported present with an empty PATH", d.ID)
		}
	}
}
