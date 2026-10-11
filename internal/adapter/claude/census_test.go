package claude

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// A turn whose last tool call left a sleep running ends with that sleep
// counted, by the kernel's name for it alone: what a hub sees as
// left_running = 1 (DEV-186). The sleeper is in a session of its own, as
// Claude Code puts every Bash command, so only the census before input
// closes can see it as this turn's.
func TestWorkLeftRunningIsCountedByNameAlone(t *testing.T) {
	const marker = "LEFT-RUNNING-MARKER-7f3a"
	pidFile := filepath.Join(t.TempDir(), "left.pid")
	h := &harness{fixture: fixture("plain"), env: map[string]string{
		"CLAUDE_TEST_LEAVE": pidFile, "CLAUDE_TEST_LEAVE_MARKER": marker,
	}}
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(string(b)); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	evs, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded {
		t.Fatalf("state %s, error %+v", out.State, out.Error)
	}
	if out.LeftRunning == nil || len(*out.LeftRunning) != 1 {
		t.Fatalf("left running %v; want the one sleep", out.LeftRunning)
	}
	l := (*out.LeftRunning)[0]
	if l.Name != "sleep" {
		t.Errorf("leftover named %q; want sleep, the executable's name, not its argv[0]", l.Name)
	}
	if strings.Contains(l.Name, marker) {
		t.Errorf("the leftover's name %q carries what was in its argv", l.Name)
	}
	for _, e := range evs {
		if strings.Contains(e.Text, marker) || strings.Contains(e.Status, marker) {
			t.Errorf("event %+v carries the marker", e)
		}
	}
}
