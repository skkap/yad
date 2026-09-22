package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// A gh that only YAD_GH_PATH finds is advertised as this runner's gh, so it is
// the gh a harness's own `gh pr create` runs — not a different one PATH
// happens to hold, nor none at all. The override's file is not even called gh
// (DEV-107, 0045).
func TestE2EAHarnessChildRunsTheOverridesGH(t *testing.T) {
	m := newMachine(t, claudeE2E)
	override := filepath.Join(t.TempDir(), "gh-beta")
	script := "#!/bin/sh\ncase \"$1\" in\n--version) echo 'gh version 9.9.9-override (2026-09-22)' ;;\nauth) echo '{\"hosts\":{}}' ;;\nesac\n"
	if err := os.WriteFile(override, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// And a different gh on PATH, which is what the child found before.
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "gh"), []byte("#!/bin/sh\necho 'gh version 1.0.0 (PATH)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAD_GH_PATH", override)

	var doc v1.Capabilities
	if err := json.Unmarshal([]byte(m.ok("harnesses")), &doc); err != nil {
		t.Fatal(err)
	}
	for _, tool := range doc.HostTools {
		if tool.ID == "gh" && (!tool.Present || tool.Version != "9.9.9-override") {
			t.Fatalf("gh is advertised as %+v, want the override's", tool)
		}
	}

	ran := filepath.Join(t.TempDir(), "gh-version")
	t.Setenv(fakeClaudeGH, ran)
	m.submit("gh-1")
	d := m.daemon()
	if code, out, errs := m.watch("gh-1"); code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
	got, err := os.ReadFile(ran)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "9.9.9-override") {
		t.Errorf("the harness's `gh --version` printed %q, want the override's gh", got)
	}
}
