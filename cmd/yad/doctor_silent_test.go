package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
	"github.com/skkap/yad/internal/store"
)

// loggedInClaude is a claude ready by every probe and logged in on its own
// home, at YAD_CLAUDE_PATH; its home is a fresh CLAUDE_CONFIG_DIR, returned.
func loggedInClaude(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\ncase \"$*\" in\n--help) echo '  --system-prompt-snapshot <on|off>  --fork-session' ;;\n--version) echo '2.1.296 (Claude Code)' ;;\n'auth status') echo '{\"loggedIn\": true}' ;;\n*) exit 2 ;;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAD_CLAUDE_PATH", bin)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	return home
}

// The daemon's record of a login whose last three runs said nothing makes
// doctor say not ready, with until when and what to do — read-only, from the
// state database, as health reads it (decision 0077).
func TestDoctorSaysAHarnessWhoseRunsWentSilentIsNotReady(t *testing.T) {
	p := accountEnv(t)
	noTools(t)
	loggedInClaude(t)
	ctx := context.Background()
	st, err := store.Open(ctx, p.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	for range account.SilentRuns {
		if _, err := account.NoteSilent(ctx, st.Queries, "claude", "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	code, out, errs := yadIn(t, "doctor")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !regexp.MustCompile(`Claude Code +not ready`).MatchString(out) {
		t.Fatalf("doctor does not say Claude Code is not ready:\n%s", out)
	}
	if !strings.Contains(out, "No harness ready now") {
		t.Errorf("doctor's last line does not say why nothing is ready:\n%s", out)
	}
	_, warn, ok := strings.Cut(out, "\nwarning: Claude Code — not ready: its own login has said nothing in its last 3 runs")
	if !ok {
		t.Fatalf("no warning for the silent login:\n%s", out)
	}
	warn, _, _ = strings.Cut(warn, "\n")
	shellwordtest.CheckEnv(t, onlyCommand(t, warn, "yad --profile default daemon restart"), dirsEnv(t), "yad", "--profile", "default", "daemon", "restart")
	shellwordtest.CheckEnv(t, onlyCommand(t, warn, "yad --profile default account add"), dirsEnv(t), "yad", "--profile", "default", "account", "add", "claude", "<label>")
}

// Another Claude in the home is a warning in doctor, with its pid, and in the
// capability document `yad harnesses` prints, with a count alone; in neither
// is the harness anything but ready.
func TestAnotherClaudeInTheHomeIsAWarningInDoctorAndTheDocument(t *testing.T) {
	accountEnv(t)
	noTools(t)
	home := loggedInClaude(t)
	if err := os.MkdirAll(filepath.Join(home, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "sessions", "4242.json"), []byte(`{"pid":4242,"procStart":"Sun Oct 11 09:18:10 2026","kind":"background"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := capability.ProcessesForTests
	capability.ProcessesForTests = func(context.Context) ([]capability.Process, error) {
		return []capability.Process{{PID: 4242, PPID: 1, Comm: "claude", Start: "Sun Oct 11 09:18:10 2026"}}, nil
	}
	capability.ForgetSharedHomes()
	t.Cleanup(func() {
		capability.ProcessesForTests = prev
		capability.ForgetSharedHomes()
	})

	code, out, errs := yadIn(t, "doctor")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if !regexp.MustCompile(`Claude Code +ready`).MatchString(out) {
		t.Errorf("another Claude in the home made Claude Code anything but ready:\n%s", out)
	}
	if !strings.Contains(out, "warning: Claude Code — 1 other Claude process that yad did not start (pid 4242) runs from the home this runner's Claude runs use, `claude --bg` among them") {
		t.Errorf("doctor does not warn of the other Claude:\n%s", out)
	}

	code, out, errs = yadIn(t, "harnesses")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var doc v1.Capabilities
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if !capability.Drivable(doc, "claude") {
		t.Errorf("the document does not offer claude: %s", out)
	}
	for _, h := range doc.Harnesses {
		if h.ID != "claude" {
			continue
		}
		if len(h.Warnings) != 1 || !strings.Contains(h.Warnings[0], "1 other Claude process that yad did not start runs from the home") || strings.Contains(h.Warnings[0], "4242") || strings.Contains(h.Warnings[0], home) {
			t.Errorf("warnings = %q, want one with a count and neither pid nor path", h.Warnings)
		}
	}
}
