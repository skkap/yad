package harness

import (
	"context"
	"os"
	"path/filepath"
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
func TestDetectReportsAbsentHarnesses(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, h := range Catalog() {
		t.Setenv(h.EnvPath, "")
	}
	got := Detect(context.Background())
	if len(got) != len(Catalog()) {
		t.Fatalf("Detect returned %d entries, want %d", len(got), len(Catalog()))
	}
	for i, d := range got {
		if d.ID != Catalog()[i].ID {
			t.Errorf("entry %d is %q, want catalog order %q", i, d.ID, Catalog()[i].ID)
		}
		if d.Present || d.Ready() {
			t.Errorf("%s reported present with an empty PATH", d.ID)
		}
	}
}

// A present harness whose version probe fails is reported broken, not dropped,
// and is never Ready — a runner must not take work for it.
func TestDetectReportsBrokenHarness(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("YAD_CLAUDE_PATH", "")
	for _, d := range Detect(context.Background()) {
		if d.ID != "claude" {
			continue
		}
		if !d.Present || d.Error == "" || d.Ready() {
			t.Errorf("broken claude: present=%v error=%q ready=%v", d.Present, d.Error, d.Ready())
		}
		return
	}
	t.Fatal("claude missing from Detect")
}

func TestDetectReadsVersionAndHonoursEnvPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "anything")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'codex-cli 0.147.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("YAD_CODEX_PATH", bin)
	for _, d := range Detect(context.Background()) {
		if d.ID != "codex" {
			continue
		}
		if !d.Ready() || d.Version != "codex-cli 0.147.0" || d.Path != bin {
			t.Errorf("codex = %+v", d)
		}
		return
	}
	t.Fatal("codex missing from Detect")
}

func TestLookup(t *testing.T) {
	if h, ok := Lookup("claude"); !ok || h.Kind != FirstClass {
		t.Errorf("Lookup(claude) = %+v, %v", h, ok)
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("Lookup(nope) found something")
	}
}
