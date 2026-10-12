package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
)

// A Claude run whose last tool call left a sleep running ends succeeded with
// left_running = 1 and a status event naming sleep, through the whole path to
// yad hub, and nothing in its argv or environment reaches the hub (YAD-186's
// acceptance). The sleep is in a session of its own, as Claude Code starts
// every Bash command, so the runner's group kill never reaches it. With the
// owner's [supervise] stop_left_running the runner stops it as the turn
// ends, and the event says so; without, it runs on (decision 0086).
func TestE2EWorkLeftRunningIsReported(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run("stop="+strconv.FormatBool(stop), func(t *testing.T) { testE2ELeftRunning(t, stop) })
	}
}

func testE2ELeftRunning(t *testing.T, stop bool) {
	const marker = "LEFT-RUNNING-E2E-MARKER-41c9"
	pidFile := filepath.Join(t.TempDir(), "left.pid")
	m := newMachine(t, claudeE2E)
	t.Setenv(fakeClaudeLeave, pidFile)
	t.Setenv(fakeClaudeLeaveMarker, marker)
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
	if stop {
		m.setStopLeftRunning()
	}
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
	// Counted whoever stopped it: the figure is what the turn left.
	if n := run.Result.Metrics.LeftRunning; n == nil || *n != 1 {
		t.Errorf("left_running = %v; want 1", n)
	}
	evs := m.hubEvents("e2e-left")
	contiguous(t, evs)
	var text string
	for _, e := range evs {
		if e.Kind == v1.EventStatus && e.Status == "left_running" {
			text = e.Text
		}
	}
	if !strings.Contains(text, "sleep (running ") {
		t.Errorf("no left_running status event naming sleep in %+v", evs)
	}
	if said := strings.HasSuffix(text, "; yad stopped it"); said != stop {
		t.Errorf("left_running event %q; with stop_left_running %v it should say yad stopped it: %v", text, stop, stop)
	}
	if !stop && strings.Contains(text, "yad") {
		t.Errorf("left_running event %q speaks of stopping with stop_left_running off", text)
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
	if pid() <= 0 {
		t.Fatal("the fake wrote no pid for its sleep")
	}
	// Stopped, it is init's to reap once the fake has gone; off, nothing
	// touches it.
	gone := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if gone = errors.Is(syscall.Kill(pid(), 0), syscall.ESRCH); gone || !stop {
			break
		}
	}
	if gone != stop {
		t.Errorf("the sleep is gone: %v; want %v with stop_left_running %v\ndaemon:\n%s", gone, stop, stop, d.out.String())
	}
}

func (m *machine) setStopLeftRunning() {
	m.t.Helper()
	p := config.Paths{Config: m.p.config, Data: m.p.data}
	cfg, err := config.Load(p)
	if err != nil {
		m.t.Fatal(err)
	}
	cfg.Supervise.StopLeftRunning = true
	if err := config.Save(p, cfg); err != nil {
		m.t.Fatal(err)
	}
}
