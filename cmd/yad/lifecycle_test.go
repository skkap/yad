//go:build unix

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/logfile"
	"github.com/skkap/yad/internal/runner"
)

// beYad makes the test binary, re-executed, into yad ("yad") or into a
// daemon that takes the lock and never answers ("wedged").
const beYad = "YAD_TEST_BE"

// wedgedDaemon holds a profile's lock and socket and answers nothing, as a
// daemon stuck in a deadlock would. YAD_TEST_IGNORE_TERM makes it ignore
// SIGTERM too, so only SIGKILL ends it.
func wedgedDaemon() {
	if os.Getenv("YAD_TEST_IGNORE_TERM") != "" {
		signal.Ignore(syscall.SIGTERM)
	}
	p, err := config.Resolve("")
	if err != nil {
		os.Exit(2)
	}
	if _, err := control.Claim(p); err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(2)
	}
	time.Sleep(time.Hour)
}

// lifecycle is one throwaway profile whose daemons are real processes. Every
// daemon a test leaves running is killed at cleanup: nothing outlives a test.
type lifecycle struct {
	t *testing.T
	p config.Paths
}

func newLifecycle(t *testing.T) *lifecycle {
	t.Helper()
	t.Setenv("YAD_CONFIG_DIR", shortDir(t))
	t.Setenv("YAD_DATA_DIR", shortDir(t))
	noTools(t)
	t.Setenv("YAD_PROFILE", "")
	t.Setenv(beYad, "yad")
	p, err := config.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	l := &lifecycle{t: t, p: p}
	t.Cleanup(func() {
		if pid, running, _ := control.Holder(p); running && pid > 1 {
			t.Errorf("a daemon (pid %d) outlived the test; killing it", pid)
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})
	return l
}

func (l *lifecycle) yad(args ...string) (int, string, string) {
	l.t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func (l *lifecycle) ok(args ...string) string {
	l.t.Helper()
	code, out, errs := l.yad(args...)
	if code != 0 {
		l.t.Fatalf("yad %s: exit %d: %s%s", strings.Join(args, " "), code, out, errs)
	}
	return out
}

func (l *lifecycle) pid() int {
	l.t.Helper()
	pid, running, err := control.Holder(l.p)
	if err != nil || !running {
		l.t.Fatalf("no daemon: %v", err)
	}
	return pid
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// eventuallyTrue polls until cond holds or five seconds pass.
func eventuallyTrue(cond func() bool) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

func TestDaemonLifecycle(t *testing.T) {
	l := newLifecycle(t)

	code, out, _ := l.yad("daemon", "status")
	if code != 3 || !strings.Contains(out, "not running") {
		t.Fatalf("status before start: exit %d, %q", code, out)
	}
	if code, _, errs := l.yad("status"); code != 1 || !strings.Contains(errs, "yad --profile default daemon start") {
		t.Errorf("yad status with no daemon: exit %d, %q", code, errs)
	}

	out = l.ok("daemon", "start")
	pid := l.pid()
	if !strings.Contains(out, "started — pid "+strconv.Itoa(pid)) {
		t.Errorf("start said %q; the lock names pid %d", out, pid)
	}
	fi, err := os.Stat(l.p.Socket())
	if err != nil || fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket %v, %v; want a 0600 socket", fi, err)
	}

	code, _, errs := l.yad("daemon", "start")
	if code != 1 || !strings.Contains(errs, "already running") || !strings.Contains(errs, "(pid "+strconv.Itoa(pid)) {
		t.Errorf("a second start: exit %d, %q", code, errs)
	}
	// A foreground start beside it is refused the same way, by the lock.
	if code, _, errs := l.yad("daemon", "start", "--foreground"); code != 1 || !strings.Contains(errs, "already running") {
		t.Errorf("a foreground start beside a daemon: exit %d, %q", code, errs)
	}

	out = l.ok("daemon", "status")
	if !strings.HasPrefix(out, "running — pid "+strconv.Itoa(pid)) || !strings.Contains(out, l.p.Log()) {
		t.Errorf("daemon status:\n%s", out)
	}
	out = l.ok("status")
	if !strings.Contains(out, "pid "+strconv.Itoa(pid)) || !strings.Contains(out, "no connections") || !strings.Contains(out, "no runs held") {
		t.Errorf("status:\n%s", out)
	}
	var st control.Status
	if err := json.Unmarshal([]byte(l.ok("status", "--json")), &st); err != nil || st.PID != pid || st.Capacity.Total < 1 || st.Capacity.Free != st.Capacity.Total {
		t.Errorf("status --json: %+v, %v", st, err)
	}

	if !eventuallyTrue(func() bool {
		_, out, _ := l.yad("daemon", "logs", "-n", "20")
		return strings.Contains(out, "daemon started")
	}) {
		_, out, _ := l.yad("daemon", "logs")
		t.Errorf("the log does not say the daemon started:\n%s", out)
	}
	raw := l.ok("daemon", "logs", "--json", "-n", "1")
	var rec map[string]any
	if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec["msg"] == nil {
		t.Errorf("logs --json is not a JSON line: %q", raw)
	}

	out = l.ok("daemon", "stop")
	if !strings.Contains(out, "stopping — pid "+strconv.Itoa(pid)) || !strings.Contains(out, "stopped") {
		t.Errorf("stop said %q", out)
	}
	if !eventuallyTrue(func() bool { return !alive(pid) }) {
		t.Errorf("pid %d is alive after stop", pid)
	}
	if _, err := os.Stat(l.p.Socket()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the socket outlived the daemon: %v", err)
	}
	if out := l.ok("daemon", "stop"); !strings.Contains(out, "not running") {
		t.Errorf("stop with nothing running: %q", out)
	}
	if _, out, _ := l.yad("daemon", "logs"); !strings.Contains(out, "stop requested through the control socket") || !strings.Contains(out, "daemon stopped") {
		t.Errorf("the log does not record the stop:\n%s", out)
	}
}

// A crash leaves a socket file and a lock file naming a dead pid; neither may
// block the next start.
func TestStartAfterACrash(t *testing.T) {
	l := newLifecycle(t)
	if err := l.p.Ensure(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: l.p.Socket(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	if err := os.WriteFile(l.p.Lock(), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := l.yad("daemon", "status"); code != 3 || !strings.Contains(out, "not running") {
		t.Errorf("status over a stale socket: exit %d, %q", code, out)
	}
	l.ok("daemon", "start")
	l.ok("status")
	l.ok("daemon", "stop")
}

// A daemon holding its lock and answering nothing is stopped by signal: the
// kill fallback.
func TestStopFallsBackToSignals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ignoreTerm bool
		args       []string
		want       string
		code       int
	}{
		{"SIGTERM ends it", false, nil, "sent it SIGTERM", 0},
		{"ignores SIGTERM, no --force", true, []string{"--timeout", "300ms"}, "yad --profile default daemon stop --force", 1},
		{"ignores SIGTERM, --force kills it", true, []string{"--timeout", "300ms", "--force"}, "killed pid", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newLifecycle(t)
			if err := l.p.Ensure(); err != nil {
				t.Fatal(err)
			}
			termGrace, killGrace, forceStep = 300*time.Millisecond, 5*time.Second, 100*time.Millisecond
			t.Cleanup(func() { termGrace, killGrace, forceStep = 10*time.Second, 5*time.Second, time.Second })
			pid := l.spawnWedged(tc.ignoreTerm)

			code, out, errs := l.yad(append([]string{"daemon", "stop"}, tc.args...)...)
			if code != tc.code || !strings.Contains(out+errs, tc.want) {
				t.Errorf("exit %d, want %d: %s%s — want %q", code, tc.code, out, errs, tc.want)
			}
			if tc.code == 0 && !eventuallyTrue(func() bool { return !alive(pid) }) {
				t.Errorf("pid %d is alive", pid)
			}
			if tc.code != 0 {
				// Without --force nothing past SIGTERM is sent: a daemon
				// that ignores it is still there.
				if !alive(pid) {
					t.Errorf("pid %d was killed without --force", pid)
				}
				syscall.Kill(pid, syscall.SIGKILL)
				eventuallyTrue(func() bool { return !alive(pid) })
			}
		})
	}
}

// spawnWedged starts a wedged daemon and waits for it to hold the lock.
func (l *lifecycle) spawnWedged(ignoreTerm bool) int {
	l.t.Helper()
	// This daemon answers nothing, ever, so the ask before the signal ladder is
	// waiting for a certainty: the test need not spend the five seconds a real
	// owner spends finding out.
	control.AskTimeoutForTests = 200 * time.Millisecond
	l.t.Cleanup(func() { control.AskTimeoutForTests = 0 })
	exe, err := os.Executable()
	if err != nil {
		l.t.Fatal(err)
	}
	// os.StartProcess does not dedupe, and getenv reads the first entry: the
	// parent's own setting has to go, not be shadowed.
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, beYad+"=") {
			env = append(env, kv)
		}
	}
	env = append(env, beYad+"=wedged")
	if ignoreTerm {
		env = append(env, "YAD_TEST_IGNORE_TERM=1")
	}
	proc, err := os.StartProcess(exe, []string{exe}, &os.ProcAttr{Env: env, Files: []*os.File{nil, os.Stderr, os.Stderr}})
	if err != nil {
		l.t.Fatal(err)
	}
	go proc.Wait() // reaped as soon as it dies, so alive() sees it gone
	l.t.Cleanup(func() { proc.Kill() })
	if !eventuallyTrue(func() bool { pid, ok, _ := control.Holder(l.p); return ok && pid == proc.Pid }) {
		l.t.Fatal("the wedged daemon never took the lock")
	}
	return proc.Pid
}

// restart checks the credentials before it stops anything: a bad one leaves
// the running daemon exactly as it was.
func TestRestartPreflightsTheCredential(t *testing.T) {
	l := newLifecycle(t)
	l.ok("daemon", "start")
	first := l.pid()
	t.Cleanup(func() { l.yad("daemon", "stop", "--force") })

	cfg, err := config.Load(l.p)
	if err != nil {
		t.Fatal(err)
	}
	// A hub that is not there: syncs fail and retry, which is not fatal.
	cfg.Connections = []config.Connection{{Name: "home", URL: "http://127.0.0.1:9/v1"}}
	if err := config.Save(l.p, cfg); err != nil {
		t.Fatal(err)
	}

	code, _, errs := l.yad("daemon", "restart")
	if code != 1 || !strings.Contains(errs, "restart refused") || !strings.Contains(errs, "yad --profile default connect") {
		t.Errorf("restart with no credential: exit %d, %q", code, errs)
	}
	if pid := l.pid(); pid != first {
		t.Fatalf("the daemon changed from %d to %d on a refused restart", first, pid)
	}
	cred := filepath.Join(l.p.Config, "credentials", "home")
	if err := os.MkdirAll(filepath.Dir(cred), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cred, []byte("yadrun_test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errs := l.yad("daemon", "restart"); code != 1 || !strings.Contains(errs, "readable by others") {
		t.Errorf("restart with an exposed credential: exit %d, %q", code, errs)
	}
	if err := os.Chmod(cred, 0o600); err != nil {
		t.Fatal(err)
	}

	out := l.ok("daemon", "restart")
	second := l.pid()
	if second == first || !strings.Contains(out, "stopping — pid "+strconv.Itoa(first)) || !strings.Contains(out, "started — pid "+strconv.Itoa(second)) {
		t.Errorf("restart: %d → %d:\n%s", first, second, out)
	}
	if !eventuallyTrue(func() bool { return !alive(first) }) {
		t.Errorf("the old daemon %d is alive", first)
	}
	var st control.Status
	if err := json.Unmarshal([]byte(l.ok("status", "--json")), &st); err != nil || len(st.Connections) != 1 || st.Connections[0].Name != "home" {
		t.Errorf("status after restart: %+v, %v", st, err)
	}
	l.ok("daemon", "stop")
}

// Refused before any process is spawned, with the fix named.
func TestStartRefusesALongSocketPath(t *testing.T) {
	l := newLifecycle(t)
	long := filepath.Join(shortDir(t), strings.Repeat("d", 110))
	t.Setenv("YAD_DATA_DIR", long)
	for _, args := range [][]string{{"daemon", "start"}, {"daemon", "start", "--foreground"}, {"status"}} {
		code, _, errs := l.yad(args...)
		if code != 1 || !strings.Contains(errs, "set YAD_DATA_DIR to a shorter directory") {
			t.Errorf("yad %v: exit %d, %q", args, code, errs)
		}
	}
}

func TestRenderLogLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`{"time":"2026-09-19T03:04:05.123Z","level":"WARN","msg":"sync failed","connection":"home","err":"dial tcp: refused"}`,
			time.Date(2026, 9, 19, 3, 4, 5, 0, time.UTC).Local().Format("2006-01-02 15:04:05") + ` WARN  sync failed connection=home err="dial tcp: refused"`},
		{`not json`, `not json`},
	} {
		if got := renderLogLine(tc.in); got != tc.want {
			t.Errorf("renderLogLine(%s)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

// Run fields, errors and log text can come from a hub: none of them may
// reach the terminal as a control sequence or as a row of its own.
func TestStatusAndLogsEscapeHubText(t *testing.T) {
	evil := "run\x1b[2J\nFAKE ROW\tx"
	now := time.Now()
	s := control.Status{
		Connections: []control.Connection{{Name: "home", URL: "https://hub", State: "retrying", LastError: evil, LastErrorAt: &now}},
		Runs:        []control.Run{{Connection: "home", ID: evil, Session: evil, Harness: "claude", Model: evil, State: "running", Reason: evil, Since: now}},
		Errors:      []control.LogRecord{{Time: now, Level: "ERROR", Message: evil, Attrs: "err=" + evil}},
	}
	var b bytes.Buffer
	printStatus(&b, config.Paths{}, s, now)
	got := b.String()
	if strings.Contains(got, "\x1b") || strings.Contains(got, "\nFAKE ROW") {
		t.Errorf("status passed hub text through:\n%q", got)
	}

	var lb bytes.Buffer
	line, _ := json.Marshal(map[string]string{"time": now.Format(time.RFC3339Nano), "level": "WARN", "msg": "sync failed", "err": evil})
	(&logPrinter{w: &lb}).Write(append(line, '\n'))
	if strings.Contains(lb.String(), "\x1b") || strings.Count(lb.String(), "\n") != 1 {
		t.Errorf("logs passed hub text through:\n%q", lb.String())
	}
}

// Load and Save refuse a connection URL with userinfo, so this Config is one
// only a bug could build. `yad status` redacts anyway: it prints every
// connection's URL, and the control socket hands the same document to anything
// that asks, so a credential that got past the check is still out before
// either sees it.
func TestStatusNeverCarriesACredentialFromAConnectionURL(t *testing.T) {
	const password = "hunter2"
	cfg := config.Config{Connections: []config.Connection{{Name: "home", URL: "https://runner:" + password + "@hub.example/v1"}}}
	st := statusOf(context.Background(), config.Paths{}, cfg, v1.Capabilities{}, time.Now(), runner.NewMonitor(), logfile.NewRecent(1))
	doc, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	printStatus(&b, config.Paths{}, st, time.Now())
	for _, out := range []string{string(doc), b.String()} {
		if strings.Contains(out, password) {
			t.Errorf("the password is in the status:\n%s", out)
		}
		if !strings.Contains(out, "redacted@hub.example/v1") {
			t.Errorf("the status no longer says which hub the connection is:\n%s", out)
		}
	}
}

// A daemon whose only connection cannot start exits at once. The start must
// say so, never report it started.
func TestStartReportsADaemonThatCannotRun(t *testing.T) {
	l := newLifecycle(t)
	cfg, err := config.Load(l.p)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Connections = []config.Connection{{Name: "home", URL: "http://127.0.0.1:9/v1"}} // no credential
	if err := config.Save(l.p, cfg); err != nil {
		t.Fatal(err)
	}
	code, out, errs := l.yad("daemon", "start")
	if code != 1 || strings.Contains(out, "started") || !strings.Contains(errs, "exited as it started") || !strings.Contains(errs, "yad --profile default connect") {
		t.Errorf("start with no usable connection: exit %d\n%s%s", code, out, errs)
	}
	if !eventuallyTrue(func() bool { _, running, _ := control.Holder(l.p); return !running }) {
		t.Error("a daemon is still running")
	}
}

// A foreground daemon whose stdout is a file — service.log under launchd or
// systemd — writes its log only to the rotated file, never to that one.
func TestServiceStdoutGetsNothingOnceTheLogIsOpen(t *testing.T) {
	l := newLifecycle(t)
	out, err := os.OpenFile(filepath.Join(shortDir(t), "service.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"daemon", "start", "--foreground"}, out, out) }()
	if !eventuallyTrue(func() bool {
		res, err := control.Ask(context.Background(), l.p, "status")
		return err == nil && res.Status.Ready
	}) {
		t.Fatal("the foreground daemon never became ready")
	}
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d", code)
	}
	if b, _ := os.ReadFile(out.Name()); len(b) != 0 {
		t.Errorf("service.log got:\n%s", b)
	}
	if _, logged, _ := l.yad("daemon", "logs"); !strings.Contains(logged, "daemon started") || !strings.Contains(logged, "daemon stopped") {
		t.Errorf("the rotated log lacks the start and stop:\n%s", logged)
	}
	// A terminal still gets it live: only a char device counts as one.
	if tty, err := os.Open("/dev/null"); err == nil {
		defer tty.Close()
		if shownOnlyToAPerson(tty) != io.Writer(tty) {
			t.Error("/dev/null, a character device, was treated as captured")
		}
	}
	if shownOnlyToAPerson(out) != io.Discard {
		t.Error("a regular file was treated as a terminal")
	}
}

// A daemon a service manager started has an environment `yad doctor`, run in
// the owner's shell, never sees. So the daemon names its own account variables
// in its log at start (decision 0059) — the names, never a value.
func TestDaemonLogsItsOwnAccountVariables(t *testing.T) {
	l := newLifecycle(t)
	t.Setenv("OPENAI_API_KEY", "sk-daemon-secret-value")
	t.Setenv("CODEX_HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"daemon", "start", "--foreground"}, io.Discard, io.Discard) }()
	if !eventuallyTrue(func() bool {
		res, err := control.Ask(context.Background(), l.p, "status")
		return err == nil && res.Status.Ready
	}) {
		t.Fatal("the foreground daemon never became ready")
	}
	cancel()
	if code := <-done; code != 0 {
		t.Errorf("exit %d", code)
	}
	_, logged, _ := l.yad("daemon", "logs")
	if !strings.Contains(logged, "OPENAI_API_KEY") || !strings.Contains(logged, "decision 0059") {
		t.Errorf("the log does not name the variable and the decision:\n%s", logged)
	}
	if strings.Contains(logged, "CODEX_HOME") {
		t.Errorf("the log warns about the harness's own home:\n%s", logged)
	}
	b, _ := os.ReadFile(l.p.Log())
	if strings.Contains(logged+string(b), "sk-daemon-secret-value") {
		t.Errorf("the daemon logged a value:\n%s", logged)
	}
}

// A stop --force the owner cuts short sends nothing more: a wait that ended
// because the command did is not one that elapsed, and escalating on it would
// SIGKILL the runner before it had killed its harnesses.
func TestStopForceInterruptedEscalatesNothing(t *testing.T) {
	l := newLifecycle(t)
	if err := l.p.Ensure(); err != nil {
		t.Fatal(err)
	}
	pid := l.spawnWedged(true)
	t.Cleanup(func() {
		syscall.Kill(pid, syscall.SIGKILL)
		eventuallyTrue(func() bool { return !alive(pid) })
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	err := stopDaemon(ctx, global{paths: l.p}, stopFlags{timeout: time.Minute, force: true}, &out)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err = %v, out %s", err, out.String())
	}
	if strings.Contains(out.String(), "(cancel") || strings.Contains(out.String(), "killed") || !alive(pid) {
		t.Errorf("an interrupted stop escalated: %s (alive %v)", out.String(), alive(pid))
	}
}
