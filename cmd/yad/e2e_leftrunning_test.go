package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// A Claude run whose last tool call left a sleep running ends succeeded with
// left_running = 1 and a status event naming sleep, through the whole path to
// yad hub, and nothing in its argv or environment reaches the hub (DEV-186's
// acceptance). The sleep is in a session of its own, as Claude Code starts
// every Bash command, so the runner's group kill never reaches it.
func TestE2EWorkLeftRunningIsReported(t *testing.T) {
	const marker = "LEFT-RUNNING-E2E-MARKER-41c9"
	pidFile := filepath.Join(t.TempDir(), "left.pid")
	m := newMachine(t, claudeE2E)
	t.Setenv(fakeClaudeLeave, pidFile)
	t.Setenv(fakeClaudeLeaveMarker, marker)
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(string(b)); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	m.submit("e2e-left")
	d := m.daemon()

	code, out, errs := m.watch("e2e-left")
	if code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
	if !strings.Contains(out, "── succeeded in") || !strings.Contains(out, ", 1 left running") {
		t.Errorf("watch output does not show the success and what it left running:\n%s", out)
	}
	run, err := m.client().Run(context.Background(), "e2e-left")
	if err != nil {
		t.Fatal(err)
	}
	if run.Result == nil || run.Result.State != v1.RunSucceeded {
		t.Fatalf("result %+v", run.Result)
	}
	if n := run.Result.Metrics.LeftRunning; n == nil || *n != 1 {
		t.Errorf("left_running = %v; want 1", n)
	}
	evs := m.hubEvents("e2e-left")
	contiguous(t, evs)
	var named bool
	for _, e := range evs {
		if e.Kind == v1.EventStatus && e.Status == "left_running" {
			named = strings.Contains(e.Text, "sleep (running ")
		}
	}
	if !named {
		t.Errorf("no left_running status event naming sleep in %+v", evs)
	}
	if run.Result.LastSeq != int64(len(evs)) {
		t.Errorf("last_seq %d; the hub holds %d events", run.Result.LastSeq, len(evs))
	}
	body, err := json.Marshal(struct {
		Events []v1.Event
		Result *v1.Result
	}{evs, run.Result})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), marker) {
		t.Errorf("the leftover's argv or environment reached the hub:\n%s", body)
	}
}
