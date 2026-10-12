package claude

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// A turn whose last tool call left a sleep running ends with that sleep
// counted, by the kernel's name for it alone: what a hub sees as
// left_running = 1 (DEV-186). The sleeper is in a session of its own, as
// Claude Code puts every Bash command, so only the census before input
// closes can see it as this turn's. With the owner's stop_left_running the
// census also stops it, before the turn ends, and says so; without, it is
// left alone (decision 0086).
func TestWorkLeftRunningIsCountedByNameAlone(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run("stop="+strconv.FormatBool(stop), func(t *testing.T) { testLeftRunning(t, stop) })
	}
}

func testLeftRunning(t *testing.T, stop bool) {
	const marker = "LEFT-RUNNING-MARKER-7f3a"
	pidFile := filepath.Join(t.TempDir(), "left.pid")
	h := &harness{fixture: fixture("plain"), env: map[string]string{
		"CLAUDE_TEST_LEAVE": pidFile, "CLAUDE_TEST_LEAVE_MARKER": marker,
	}}
	pid := func() int {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(string(b))
		return n
	}
	t.Cleanup(func() {
		if p := pid(); p > 0 {
			syscall.Kill(p, syscall.SIGKILL)
		}
	})
	spec := h.spec(t)
	spec.StopLeftRunning = stop
	evs, out, _ := drive(t, context.Background(), spec, nil)
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
	if l.Stopped != stop {
		t.Errorf("stopped = %v with stop_left_running %v", l.Stopped, stop)
	}
	for _, e := range evs {
		if strings.Contains(e.Text, marker) || strings.Contains(e.Status, marker) {
			t.Errorf("event %+v carries the marker", e)
		}
	}
	// The fake has exited by now, so its sleep, stopped, is init's to reap;
	// left alone, it runs on.
	if pid() <= 0 {
		t.Fatal("the fake wrote no pid for its sleep")
	}
	gone := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if gone = errors.Is(syscall.Kill(pid(), 0), syscall.ESRCH); gone || !stop {
			break
		}
	}
	if gone != stop {
		t.Errorf("the sleep is gone: %v; want %v with stop_left_running %v", gone, stop, stop)
	}
}
