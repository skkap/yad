//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/service"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// fakeService is a service manager holding one unit, the profile's runner.
// Start spawns the test binary as the command a unit runs — `yad daemon
// start --foreground` — in a session of its own, as launchd and systemd do;
// nothing here reaches launchctl or systemctl.
type fakeService struct {
	t         *testing.T
	p         config.Paths
	loaded    bool   // the manager holds the job
	file      string // the unit file, which exists
	logFile   string // the log that file names
	statusErr error  // the manager cannot be asked

	mu     sync.Mutex
	starts int
	procs  []*os.Process
	exited map[int]bool
}

// withService gives the lifecycle's profile a loaded unit with nothing
// running under it — what `launchctl list` showed in YAD-173.
func withService(l *lifecycle) *fakeService {
	l.t.Helper()
	f := &fakeService{t: l.t, p: l.p, loaded: true, file: filepath.Join(l.t.TempDir(), "yad.runner.default.plist"),
		logFile: service.LogFile(l.p), exited: map[int]bool{}}
	if err := os.WriteFile(f.file, []byte("a unit\n"), 0o644); err != nil {
		l.t.Fatal(err)
	}
	old := serviceManager
	serviceManager = func() (service.Manager, service.Host, error) { return f, service.Host{}, nil }
	l.t.Cleanup(func() {
		serviceManager = old
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, p := range f.procs {
			if !f.exited[p.Pid] {
				p.Kill()
			}
		}
	})
	return f
}

func (f *fakeService) Name(p string) string                { return "yad.runner." + p }
func (f *fakeService) File(string) string                  { return f.file }
func (f *fakeService) Render(service.Spec) ([]byte, error) { return nil, nil }
func (f *fakeService) Install(context.Context, service.Spec) ([]string, error) {
	return nil, errors.New("not installed by these tests")
}
func (f *fakeService) Uninstall(context.Context, string) error {
	return errors.New("not uninstalled by these tests")
}
func (f *fakeService) StartCommand(p string) []string {
	return []string{"launchctl", "kickstart", "gui/501/" + f.Name(p)}
}
func (f *fakeService) RestartCommand(p string) []string {
	return []string{"launchctl", "kickstart", "-k", "gui/501/" + f.Name(p)}
}
func (f *fakeService) StatusCommand(p string) []string {
	return []string{"launchctl", "print", "gui/501/" + f.Name(p)}
}
func (f *fakeService) UnloadCommand(p string) []string {
	return []string{"launchctl", "bootout", "gui/501/" + f.Name(p)}
}

// pid is the runner the manager started and still watches, or 0.
func (f *fakeService) pid() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.procs); n > 0 && !f.exited[f.procs[n-1].Pid] {
		return f.procs[n-1].Pid
	}
	return 0
}

func (f *fakeService) started() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts
}

func (f *fakeService) Status(_ context.Context, p string) (service.Status, error) {
	st := service.Status{Name: f.Name(p), File: f.file}
	if _, err := os.Stat(f.file); err == nil {
		st.Installed, st.LogFile = true, f.logFile
	}
	// As the real managers do: a status that failed still says what the
	// unit file says.
	if f.statusErr != nil {
		return st, f.statusErr
	}
	st.Loaded = f.loaded
	if pid := f.pid(); f.loaded && pid != 0 {
		st.Running, st.PID, st.Detail = true, pid, "running"
	}
	return st, nil
}

func (f *fakeService) Start(_ context.Context, p string) error {
	f.mu.Lock()
	f.starts++
	f.mu.Unlock()
	if !f.loaded {
		return errors.New("Could not find service in domain")
	}
	if f.pid() != 0 {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.logFile), 0o700); err != nil {
		return err
	}
	log, err := os.OpenFile(f.logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(exe, "--profile", p, "daemon", "start", "--foreground")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	f.mu.Lock()
	f.procs = append(f.procs, cmd.Process)
	f.mu.Unlock()
	go func() {
		cmd.Wait()
		f.mu.Lock()
		f.exited[cmd.Process.Pid] = true
		f.mu.Unlock()
	}()
	return nil
}

// YAD-173: a restart of a runner that is a launchd job or a systemd unit
// starts it through the service manager, so the manager watches the new
// daemon and restarts it after a crash. From the state the report found — a
// daemon spawned outside the unit holding the lock, the job loaded with no
// pid — a restart puts the runner back under the manager; a restart of the
// job's own daemon does the same, and so does a start of a stopped one.
func TestRestartGoesThroughTheService(t *testing.T) {
	l := newLifecycle(t)
	t.Cleanup(func() { l.yad("daemon", "stop", "--force") })
	l.ok("daemon", "start") // no service yet: a daemon of its own
	stray := l.pid()
	svc := withService(l)

	out := l.ok("daemon", "restart")
	first := l.pid()
	if svc.started() != 1 || first != svc.pid() || first == stray {
		t.Fatalf("restart from a stray daemon: %d → %d, service started %d times, its pid %d:\n%s", stray, first, svc.started(), svc.pid(), out)
	}
	if !strings.Contains(out, "stopping — pid "+strconv.Itoa(stray)) || !strings.Contains(out, "started by the service manager as yad.runner.default — pid "+strconv.Itoa(first)) {
		t.Errorf("restart output:\n%s", out)
	}
	if !eventuallyTrue(func() bool { return !alive(stray) }) {
		t.Errorf("the stray daemon %d is alive", stray)
	}

	// The unit runs the command it was installed with; a flag it does not
	// carry is refused before anything is stopped.
	if code, _, errs := l.yad("daemon", "restart", "--interval", "5s"); code != 1 || !strings.Contains(errs, "--interval") {
		t.Errorf("restart --interval under a service: exit %d, %q", code, errs)
	}
	if pid := l.pid(); pid != first {
		t.Fatalf("a refused restart changed the daemon from %d to %d", first, pid)
	}

	out = l.ok("daemon", "restart")
	second := l.pid()
	if svc.started() != 2 || second != svc.pid() || second == first {
		t.Fatalf("restart of the job's daemon: %d → %d, service started %d times, its pid %d:\n%s", first, second, svc.started(), svc.pid(), out)
	}

	l.ok("daemon", "stop")
	out = l.ok("daemon", "start")
	if third := l.pid(); svc.started() != 3 || third != svc.pid() {
		t.Fatalf("start of a stopped service: daemon %d, service started %d times, its pid %d:\n%s", third, svc.started(), svc.pid(), out)
	}
	l.ok("daemon", "stop")
}

// A unit is the profile's only when it runs under the profile's directories:
// the label is just the profile's name, and the owner's real
// yad.runner.default is not the runner under another YAD_DATA_DIR. A unit
// that is this profile's but not loaded keeps the start outside it, and says
// how to put the runner back.
func TestStartOutsideAUnitThatIsNotRunningThisProfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		other   bool // the unit names another data directory
		loaded  bool
		advises string
	}{
		{"a unit for other directories", true, true, ""},
		{"this profile's unit, booted out", false, false, "has not loaded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLifecycle(t)
			svc := withService(l)
			svc.loaded = tc.loaded
			if tc.other {
				svc.logFile = filepath.Join(t.TempDir(), "logs", "service.log")
			}
			out := l.ok("daemon", "start")
			t.Cleanup(func() { l.yad("daemon", "stop", "--force") })
			if svc.started() != 0 || svc.pid() != 0 || !strings.Contains(out, "started — pid "+strconv.Itoa(l.pid())) {
				t.Errorf("start: service started %d times:\n%s", svc.started(), out)
			}
			if tc.advises == "" && strings.Contains(out, "note:") {
				t.Errorf("start notes a unit that is not this profile's:\n%s", out)
			}
			if tc.advises != "" && (!strings.Contains(out, tc.advises) || !strings.Contains(out, "yad --profile default service install`")) {
				t.Errorf("start does not say the unit is not loaded:\n%s", out)
			}
			l.ok("daemon", "stop")
		})
	}
}

// A profile with a unit installed whose manager cannot be asked may be a
// service: restart and start refuse, leave the daemon as it is, and name the
// manager's own command, which runs as printed.
func TestRestartRefusedWhenTheServiceManagerCannotBeAsked(t *testing.T) {
	l := newLifecycle(t)
	t.Cleanup(func() { l.yad("daemon", "stop", "--force") })
	l.ok("daemon", "start")
	first := l.pid()
	svc := withService(l)
	svc.statusErr = errors.New("launchctl print: exit status 5")

	code, _, errs := l.yad("daemon", "restart")
	if code != 1 || !strings.Contains(errs, "left alone") || !strings.Contains(errs, "exit status 5") {
		t.Errorf("restart: exit %d, %q", code, errs)
	}
	if pid := l.pid(); pid != first || svc.started() != 0 {
		t.Fatalf("a refused restart changed the daemon from %d to %d, service started %d times", first, pid, svc.started())
	}
	cmds := shellwordtest.Commands(errs, "launchctl")
	if len(cmds) != 1 {
		t.Fatalf("restart names %d launchctl commands: %q", len(cmds), errs)
	}
	shellwordtest.Check(t, cmds[0], "launchctl", "kickstart", "-k", "gui/501/yad.runner.default")

	l.ok("daemon", "stop")
	code, _, errs = l.yad("daemon", "start")
	if code != 1 || svc.started() != 0 {
		t.Fatalf("start: exit %d, service started %d times: %q", code, svc.started(), errs)
	}
	if _, running, _ := control.Holder(l.p); running {
		t.Error("a refused start left a daemon running")
	}
	cmds = shellwordtest.Commands(errs, "launchctl")
	if len(cmds) != 1 {
		t.Fatalf("start names %d launchctl commands: %q", len(cmds), errs)
	}
	shellwordtest.Check(t, cmds[0], "launchctl", "kickstart", "gui/501/yad.runner.default")
}

// With the manager unable to answer, only the unit file can say whether the
// profile may be a service. No file, or one naming other directories, is no
// service of this runner's, and the start goes ahead; a file that cannot be
// looked at is not taken for absent.
func TestStartWhenTheServiceManagerCannotBeAsked(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unit    func(t *testing.T, svc *fakeService)
		refused bool
	}{
		{"no unit file", func(t *testing.T, svc *fakeService) {
			svc.file = filepath.Join(t.TempDir(), "absent.plist")
		}, false},
		{"a unit for other directories", func(t *testing.T, svc *fakeService) {
			svc.logFile = filepath.Join(t.TempDir(), "logs", "service.log")
		}, false},
		{"a unit file that cannot be looked at", func(t *testing.T, svc *fakeService) {
			dir := filepath.Join(t.TempDir(), "locked")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			svc.file = filepath.Join(dir, "yad.runner.default.plist")
			if err := os.WriteFile(svc.file, []byte("a unit\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(dir, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0o700) })
			if _, err := os.Stat(svc.file); err == nil {
				t.Skip("a directory with no permissions is still searchable here (root?)")
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLifecycle(t)
			svc := withService(l)
			svc.statusErr = errors.New("launchctl print: exit status 5")
			tc.unit(t, svc)
			t.Cleanup(func() { l.yad("daemon", "stop", "--force") })
			code, out, errs := l.yad("daemon", "start")
			if tc.refused {
				if code != 1 || !strings.Contains(errs, "start refused") || len(shellwordtest.Commands(errs, "launchctl")) != 1 {
					t.Errorf("start: exit %d, %q", code, errs)
				}
				return
			}
			if code != 0 || svc.started() != 0 || !strings.Contains(out, "started — pid") {
				t.Errorf("start: exit %d, service started %d times: %s%s", code, svc.started(), out, errs)
			}
			l.ok("daemon", "stop")
		})
	}
}
