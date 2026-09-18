package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
)

// child is `yad daemon start --foreground` as its own process — the test
// binary re-executed as yad — so the test can signal it as a service manager
// or a person at a terminal does.
type child struct {
	cmd  *exec.Cmd
	out  *syncBuffer
	log  string // the daemon's own log: its stdout, a pipe here, carries none of it
	done chan error
}

func (m *machine) childDaemon() *child {
	m.t.Helper()
	self, err := os.Executable()
	if err != nil {
		m.t.Fatal(err)
	}
	c := &child{out: &syncBuffer{}, done: make(chan error, 1), log: filepath.Join(m.p.data, "logs", "yad.log")}
	c.cmd = exec.Command(self, "daemon", "start", "--foreground")
	c.cmd.Env = append(os.Environ(), childYad+"=1", "YAD_CONFIG_DIR="+m.p.config, "YAD_DATA_DIR="+m.p.data)
	c.cmd.Stdout, c.cmd.Stderr = c.out, c.out
	if err := c.cmd.Start(); err != nil {
		m.t.Fatal(err)
	}
	go func() { c.done <- c.cmd.Wait() }()
	m.t.Cleanup(func() {
		// A test that failed half way leaves no runner behind.
		c.cmd.Process.Kill()
		<-c.done
	})
	return c
}

func (c *child) signal(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if err := c.cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
}

// logged is what the runner has written to its log so far.
func (c *child) logged() string {
	b, _ := os.ReadFile(c.log)
	return string(b)
}

// said waits until the runner has logged want.
func (c *child) said(t *testing.T, want string) {
	t.Helper()
	eventually(t, "the runner says "+strconv.Quote(want), func() bool { return strings.Contains(c.logged(), want) })
}

// exited waits for the runner to exit by itself and fails unless it exited 0.
func (c *child) exited(t *testing.T) {
	t.Helper()
	select {
	case err := <-c.done:
		c.done <- err // for the cleanup
		if err != nil {
			t.Fatalf("runner exited with %v:\n%s%s", err, c.out.String(), c.logged())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("runner did not exit:\n%s", c.logged())
	}
}

func (m *machine) setDrainWait(d time.Duration) {
	m.t.Helper()
	p := config.Paths{Config: m.p.config, Data: m.p.data}
	cfg, err := config.Load(p)
	if err != nil {
		m.t.Fatal(err)
	}
	cfg.Drain.Wait = config.Duration{Duration: d}
	if err := config.Save(p, cfg); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) runnerID() string {
	m.t.Helper()
	b, err := os.ReadFile(filepath.Join(m.p.config, "runner-id"))
	if err != nil {
		m.t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// hubSeesDraining is whether the runner's last sync said it is draining.
func (m *machine) hubSeesDraining() bool {
	r, err := m.hubDB.GetRunner(context.Background(), m.runnerID())
	if err != nil || !r.Health.Valid {
		return false
	}
	var h v1.Health
	return json.Unmarshal([]byte(r.Health.String), &h) == nil && h.Draining && h.FreeCapacity.Total == 0
}

func harnessPID(t *testing.T, file string) int {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// The signal ladder, with real signals to a real runner process (decision
// 0029). The first SIGTERM drains: the hub hears it, offers nothing more, and
// the run held finishes and is reported before the runner exits on its own.
// The second cancels the run down the cancel ladder, and the result says the
// runner did it. The third exits at once, killing a harness still deaf to the
// cancel; the run was being cancelled, and says so once the next start
// delivers it. (A run cut short with no cancel under way is reported lost —
// TestE2ERunnerRestartMidRun.) In every case no harness process is left
// behind.
func TestE2EStopSignals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		signals int
		// deaf makes the harness ignore the interrupt, so the cancel ladder
		// is still climbing when the third signal comes.
		deaf  bool
		state v1.RunState
		class string
	}{
		{"one drains", 1, false, v1.RunSucceeded, ""},
		{"two cancel", 2, false, v1.RunCancelled, "runner_stopping"},
		{"three exit now", 3, true, v1.RunCancelled, "runner_stopping"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t)
			pidFile := filepath.Join(t.TempDir(), "claude.pid")
			t.Setenv(fakeClaudePID, pidFile)
			if tc.deaf {
				t.Setenv(fakeClaudeDeaf, "1")
			}
			m.setDrainWait(time.Hour)
			m.gated()
			m.submit("e2e-held")
			c := m.childDaemon()
			m.waitAtGate()

			c.signal(t, syscall.SIGTERM)
			c.said(t, "draining: no new runs")
			eventually(t, "the hub sees the runner draining", m.hubSeesDraining)
			m.submit("e2e-after")
			if tc.signals >= 2 {
				// Each is sent once the last was taken: two signals sent at
				// once may arrive as one.
				c.signal(t, syscall.SIGINT)
				c.said(t, "cancelling every run held")
			}
			if tc.signals >= 3 {
				c.signal(t, syscall.SIGTERM)
			}
			if tc.signals == 1 {
				// Nothing ends the run but the run: the runner waits for it.
				time.Sleep(300 * time.Millisecond)
				select {
				case err := <-c.done:
					t.Fatalf("the runner exited with the run still going: %v\n%s", err, c.out.String())
				default:
				}
				m.open()
			}
			c.exited(t)
			if want := map[int]string{1: `"drained":true`, 2: `"drained":true`, 3: `"drained":false`}[tc.signals]; !strings.Contains(c.logged(), want) {
				t.Errorf("runner log lacks %q:\n%s", want, c.logged())
			}
			eventually(t, "the harness process is gone", func() bool { return syscall.Kill(harnessPID(t, pidFile), 0) != nil })

			if tc.signals == 3 {
				// Whatever exit now left undelivered, the next start delivers.
				m.open()
				d := m.daemon()
				defer d.halt(t)
			}
			code, out, errs := m.watch("e2e-held")
			if (code == 0) != (tc.state == v1.RunSucceeded) || !strings.Contains(out, "── "+string(tc.state)) {
				t.Fatalf("watch exit %d: %s\n%s\nrunner:\n%s", code, errs, out, c.out.String())
			}
			run, err := m.client().Run(context.Background(), "e2e-held")
			if err != nil || run.Result == nil {
				t.Fatalf("run %+v, %v", run, err)
			}
			switch {
			case tc.class == "" && run.Result.Error != nil:
				t.Errorf("result error %+v", run.Result.Error)
			case tc.class != "" && (run.Result.Error == nil || run.Result.Error.Class != tc.class):
				t.Errorf("result error %+v, want class %s", run.Result.Error, tc.class)
			}
			// Queued while the runner drained: never offered to it.
			if tc.signals < 3 {
				if after, err := m.client().Run(context.Background(), "e2e-after"); err != nil || after.State != "queued" {
					t.Errorf("a run queued during the drain is %+v, %v", after.State, err)
				}
			}
		})
	}
}

// yad hub drain: the runner hears it at its next sync, lets its run finish,
// and exits by itself; the run is delivered first.
func TestE2EHubDrain(t *testing.T) {
	m := newMachine(t)
	m.gated()
	m.submit("e2e-held")
	d := m.daemon()
	m.waitAtGate()

	out := m.ok("hub", "drain", "--hub", m.service, m.runnerID())
	if !strings.Contains(out, "drains at its next sync") {
		t.Errorf("drain printed %q", out)
	}
	eventually(t, "the hub sees the runner draining", m.hubSeesDraining)
	eventually(t, "the runner's sync answers the request", func() bool {
		r, err := m.hubDB.GetRunner(context.Background(), m.runnerID())
		return err == nil && !r.DrainRequestedAt.Valid
	})
	m.submit("e2e-after")
	m.open()
	select {
	case code := <-d.done:
		d.done <- code // for halt
		if code != 0 {
			t.Fatalf("runner exited %d:\n%s", code, d.out.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the runner did not exit after draining:\n%s", d.out.String())
	}
	if !strings.Contains(d.out.String(), "drained") {
		t.Errorf("runner output:\n%s", d.out.String())
	}
	if code, out, errs := m.watch("e2e-held"); code != 0 {
		t.Fatalf("watch exit %d: %s\n%s", code, errs, out)
	}
	if after, err := m.client().Run(context.Background(), "e2e-after"); err != nil || after.State != "queued" {
		t.Errorf("a run queued during the drain is %+v, %v", after.State, err)
	}
	// The next process is not drained again.
	d2 := m.daemon()
	code, out2, errs := m.watch("e2e-after")
	if code != 0 {
		t.Fatalf("the next process: watch exit %d: %s\n%s\n%s", code, errs, out2, d2.out.String())
	}
}

// yad daemon stop, through the control socket, is the first stop signal: the
// runner drains — the hub sees it, and offers it nothing — the run it holds
// finishes and is delivered, and the daemon exits, which is when stop returns.
func TestE2EDaemonStopDrains(t *testing.T) {
	m := newMachine(t)
	m.gated()
	m.submit("e2e-held")
	d := m.daemon()
	m.waitAtGate()

	var out syncBuffer
	stopped := make(chan int, 1)
	go func() {
		stopped <- run(context.Background(), []string{"daemon", "stop", "--timeout", "1m"}, &out, &out)
	}()
	eventually(t, "the hub sees the runner draining", m.hubSeesDraining)
	m.submit("e2e-after")
	select {
	case code := <-stopped:
		t.Fatalf("stop returned %d with the run still going: %s", code, out.String())
	case <-time.After(300 * time.Millisecond):
	}
	m.open()
	select {
	case code := <-stopped:
		if code != 0 || !strings.Contains(out.String(), "stopped") {
			t.Fatalf("stop exit %d: %s", code, out.String())
		}
	case <-time.After(40 * time.Second):
		t.Fatalf("stop did not return:\n%s\ndaemon:\n%s", out.String(), d.out.String())
	}
	select {
	case code := <-d.done:
		d.done <- code // for halt
		if code != 0 || !strings.Contains(d.out.String(), "drained") {
			t.Fatalf("daemon exit %d:\n%s", code, d.out.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon is still up after stop returned")
	}
	if code, out, errs := m.watch("e2e-held"); code != 0 {
		t.Fatalf("watch exit %d: %s\n%s", code, errs, out)
	}
	if after, err := m.client().Run(context.Background(), "e2e-after"); err != nil || after.State != "queued" {
		t.Errorf("a run queued during the drain is %+v, %v", after.State, err)
	}
}

// yad daemon stop --force on a runner whose drain has not finished: its two
// SIGTERMs are the owner's second and third requests — cancel, then exit now —
// so the runner kills its own harnesses' process groups and exits by itself,
// with no SIGKILL and no harness left running. The run was being cancelled and
// says so once the next start delivers it.
func TestE2EDaemonStopForce(t *testing.T) {
	m := newMachine(t)
	pidFile := filepath.Join(t.TempDir(), "claude.pid")
	t.Setenv(fakeClaudePID, pidFile)
	t.Setenv(fakeClaudeDeaf, "1")
	m.setDrainWait(time.Hour)
	m.gated()
	m.submit("e2e-held")
	c := m.childDaemon()
	m.waitAtGate()

	code, out, errs := m.p.yad("", "daemon", "stop", "--timeout", "500ms", "--force")
	if code != 0 || !strings.Contains(out, "(exit now)") || strings.Contains(out, "killed pid") {
		t.Fatalf("stop --force exit %d: %s%s\nrunner log:\n%s", code, out, errs, c.logged())
	}
	c.exited(t)
	eventually(t, "the harness process is gone", func() bool { return syscall.Kill(harnessPID(t, pidFile), 0) != nil })

	m.open()
	d := m.daemon()
	defer d.halt(t)
	code, out, errs = m.watch("e2e-held")
	if code == 0 || !strings.Contains(out, "── cancelled") || !strings.Contains(errs, "runner_stopping") {
		t.Fatalf("watch exit %d: %s\n%s", code, errs, out)
	}
}
