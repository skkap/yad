package service

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/config"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fakeRunner answers service-manager commands from a script and records them.
// Nothing here ever reaches launchctl, systemctl or a shell.
type fakeRunner struct {
	home   string // the default user manager's unit path is under it
	calls  []string
	answer func(call string, n int) ([]byte, error) // n: how many times this call was seen before
	seen   map[string]int
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	if f.seen == nil {
		f.seen = map[string]int{}
	}
	n := f.seen[call]
	f.seen[call]++
	var out []byte
	var err error
	if f.answer != nil {
		out, err = f.answer(call, n)
	}
	// Unless a test says otherwise, the user manager has the stock layout.
	if out == nil && err == nil && strings.HasSuffix(call, "--property=UnitPath --value") {
		out = []byte(f.home + "/.config/systemd/user.control " + f.home + "/.config/systemd/user /etc/systemd/user /usr/lib/systemd/user\n")
	}
	return out, err
}

func host(t *testing.T, r Runner) Host {
	t.Helper()
	h := Host{Home: t.TempDir(), UID: 501, EUID: 501, User: "owner", Getenv: func(string) string { return "" }, Run: r}
	if f, ok := r.(*fakeRunner); ok {
		f.home = h.Home
	}
	return h
}

// goldenSpec has the characters each format must escape: a space, an
// ampersand, a percent sign, a dollar and a quote.
func goldenSpec() Spec {
	return Spec{
		Profile:    "work",
		Executable: "/Users/owner/My Tools/yad",
		Env: map[string]string{
			"PATH":         "/Users/owner/.local/bin:/opt/homebrew/bin:/usr/bin:/bin",
			"YAD_DATA_DIR": `/Volumes/R&D 100%/yad $data "x"`,
		},
		WorkingDir: "/Users/owner",
		LogFile:    "/Users/owner/.local/share/yad/profiles/work/logs/service.log",
		// The default drain wait's budget: 30 min, the ladder, a flush.
		StopTimeout: 31 * time.Minute,
	}
}

// The stop timeout each unit carries is the Spec's — the drain wait's budget —
// and never below the floor a runner with nothing to drain needs. Launchd's
// own wait for a stopping job reads it back from the installed plist.
func TestStopTimeoutReachesTheUnits(t *testing.T) {
	h := host(t, &fakeRunner{})
	for _, tc := range []struct {
		name string
		stop time.Duration
		secs string
	}{
		{"the drain budget", 2*time.Hour + 75*time.Second, "7275"},
		{"unset is the floor", 0, "30"},
		{"below the floor", time.Second, "30"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := goldenSpec()
			sp.StopTimeout = tc.stop
			l := &Launchd{Host: h}
			plist, err := l.Render(sp)
			if err != nil {
				t.Fatal(err)
			}
			if want := "<key>ExitTimeOut</key>\n\t<integer>" + tc.secs + "</integer>"; !strings.Contains(string(plist), want) {
				t.Errorf("plist lacks %q:\n%s", want, plist)
			}
			unit, err := (&Systemd{Host: h}).Render(sp)
			if err != nil {
				t.Fatal(err)
			}
			if want := "TimeoutStopSec=" + tc.secs + "s\n"; !strings.Contains(string(unit), want) {
				t.Errorf("unit lacks %q:\n%s", want, unit)
			}
			if err := os.MkdirAll(filepath.Dir(l.File("work")), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(l.File("work"), plist, 0o600); err != nil {
				t.Fatal(err)
			}
			n, _ := strconv.Atoi(tc.secs)
			if got := l.installedStop("work"); got != time.Duration(n)*time.Second {
				t.Errorf("installed stop %s, want %ss", got, tc.secs)
			}
		})
	}
	if got := (&Launchd{Host: h}).installedStop("none"); got != stopTimeout {
		t.Errorf("no plist: %s, want the floor", got)
	}
}

func TestRenderMatchesGolden(t *testing.T) {
	h := host(t, &fakeRunner{})
	for _, tc := range []struct {
		golden string
		m      Manager
	}{
		{"launchd.plist", &Launchd{Host: h}},
		{"systemd.service", &Systemd{Host: h}},
	} {
		t.Run(tc.golden, func(t *testing.T) {
			got, err := tc.m.Render(goldenSpec())
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join("testdata", tc.golden)
			if *update {
				if err := os.WriteFile(file, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(file)
			if err != nil {
				t.Fatalf("%v — run go test ./internal/service -update", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("render differs from %s (go test ./internal/service -update rewrites it):\n%s", file, got)
			}
		})
	}
}

func TestNamesKeepProfilesApart(t *testing.T) {
	h := host(t, &fakeRunner{})
	l, s := &Launchd{Host: h}, &Systemd{Host: h}
	for _, tc := range []struct{ got, want string }{
		{l.Name(config.DefaultProfile), "yad.runner.default"},
		{l.Name("work"), "yad.runner.work"},
		{l.File("work"), filepath.Join(h.Home, "Library", "LaunchAgents", "yad.runner.work.plist")},
		{s.Name(config.DefaultProfile), "yad-runner-default.service"},
		{s.Name("work"), "yad-runner-work.service"},
		{s.File("work"), filepath.Join(h.Home, ".config", "systemd", "user", "yad-runner-work.service")},
	} {
		if tc.got != tc.want {
			t.Errorf("got %s, want %s", tc.got, tc.want)
		}
	}
}

// plutil is on every Mac; where it is, the plist must be one launchd accepts.
func TestPlistLints(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil is macOS-only")
	}
	data, err := (&Launchd{Host: host(t, &fakeRunner{})}).Render(goldenSpec())
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t.TempDir(), "yad.runner.work.plist")
	if err := os.WriteFile(f, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(plutil, "-lint", f).CombinedOutput(); err != nil {
		t.Fatalf("plutil -lint: %v\n%s", err, out)
	}
	// What launchd will read back: the escaped values must round-trip.
	out, err := exec.Command(plutil, "-extract", "EnvironmentVariables.YAD_DATA_DIR", "raw", f).Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != goldenSpec().Env["YAD_DATA_DIR"] {
		t.Errorf("YAD_DATA_DIR round-tripped as %q", got)
	}
}

// systemd-analyze is on the Linux CI runner and absent on macOS; where it is,
// the unit must verify clean.
func TestSystemdUnitVerifies(t *testing.T) {
	analyze, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze is not installed")
	}
	dir := t.TempDir()
	// verify checks that ExecStart names an executable file, so point it at one.
	exe := filepath.Join(dir, "bin dir", "yad")
	if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sp := goldenSpec()
	sp.Executable, sp.WorkingDir, sp.LogFile = exe, dir, filepath.Join(dir, "logs", "service.log")
	data, err := (&Systemd{Host: host(t, &fakeRunner{})}).Render(sp)
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "yad-runner-work.service")
	if err := os.WriteFile(f, data, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(analyze, "verify", "--man=no", f).CombinedOutput()
	if err != nil {
		t.Fatalf("systemd-analyze verify: %v\n%s\n%s", err, out, data)
	}
	// Warnings go to the output without failing the command; any line about
	// this unit is a directive systemd did not take as written.
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "yad-runner-work.service") {
			t.Errorf("systemd-analyze: %s", line)
		}
	}
}

func TestSystemdQuoting(t *testing.T) {
	for _, tc := range []struct{ in, exec, env string }{
		{"/usr/bin", `"/usr/bin"`, `"/usr/bin"`},
		{"a b", `"a b"`, `"a b"`},
		{`say "hi"`, `"say \"hi\""`, `"say \"hi\""`},
		{`C:\x`, `"C:\\x"`, `"C:\\x"`},
		{"100%", `"100%%"`, `"100%%"`},
		{"$HOME", `"$$HOME"`, `"$HOME"`},
	} {
		if got := quoteExec(tc.in); got != tc.exec {
			t.Errorf("quoteExec(%q) = %s, want %s", tc.in, got, tc.exec)
		}
		if got := quoteEnv(tc.in); got != tc.env {
			t.Errorf("quoteEnv(%q) = %s, want %s", tc.in, got, tc.env)
		}
	}
}

func TestSystemdRefusesLinesItCannotWrite(t *testing.T) {
	s := &Systemd{Host: host(t, &fakeRunner{})}
	for name, mut := range map[string]func(*Spec){
		"newline in log":   func(sp *Spec) { sp.LogFile = "/tmp/a\n[Service]\nExecStartPre=/bin/evil" },
		"trailing space":   func(sp *Spec) { sp.WorkingDir = "/home/owner " },
		"newline in PATH":  func(sp *Spec) { sp.Env["PATH"] = "/bin\nExecStartPre=/bin/evil" },
		"newline in a key": func(sp *Spec) { sp.Env["X\nY"] = "1" },
	} {
		sp := goldenSpec()
		mut(&sp)
		if _, err := s.Render(sp); err == nil {
			t.Errorf("%s: rendered", name)
		}
	}
}

func TestLoginPATH(t *testing.T) {
	const script = `printf '\n` + pathMarker + `%s` + pathMarker + `\n' "$PATH"`
	marked := func(p string) []byte {
		return []byte("Welcome back!\nnvm: using node 22\n" + pathMarker + p + pathMarker + "\nbye\n")
	}
	for _, tc := range []struct {
		name, shell, fallback string
		answer                func(call string) ([]byte, error)
		want                  string
		wantNote              bool
		wantCalls             int
	}{
		{
			name: "interactive login shell", shell: "/bin/zsh",
			answer: func(string) ([]byte, error) { return marked("/Users/o/.local/bin:/opt/homebrew/bin:/usr/bin"), nil },
			want:   "/Users/o/.local/bin:/opt/homebrew/bin:/usr/bin", wantCalls: 1,
		},
		{
			name: "rc noise and an rc error around a good PATH", shell: "/bin/bash",
			answer: func(string) ([]byte, error) {
				return marked("/home/o/.local/bin:/usr/bin"), errors.New("exit status 1: .bashrc: line 3: nope")
			},
			want: "/home/o/.local/bin:/usr/bin", wantCalls: 1,
		},
		{
			name: "interactive fails, login alone works", shell: "/usr/bin/zsh",
			answer: func(call string) ([]byte, error) {
				if strings.Contains(call, " -ilc ") {
					return nil, errors.New("signal: killed")
				}
				return marked("/home/o/.local/bin:/usr/bin"), nil
			},
			want: "/home/o/.local/bin:/usr/bin", wantCalls: 2,
		},
		{
			name: "relative, empty and repeated entries dropped", shell: "/bin/sh",
			answer: func(string) ([]byte, error) { return marked(".:/usr/bin::bin:/usr/bin:/bin:"), nil },
			want:   "/usr/bin:/bin", wantCalls: 1,
		},
		{
			name: "shell prints no PATH", shell: "/bin/zsh", fallback: "/usr/local/bin:.:/usr/bin",
			answer: func(string) ([]byte, error) { return []byte("hello\n"), nil },
			want:   "/usr/local/bin:/usr/bin", wantNote: true, wantCalls: 2,
		},
		{
			name: "fish is not asked", shell: "/opt/homebrew/bin/fish", fallback: "/opt/homebrew/bin:/usr/bin",
			want: "/opt/homebrew/bin:/usr/bin", wantNote: true,
		},
		{
			name: "no SHELL", fallback: "/usr/bin",
			want: "/usr/bin", wantNote: true,
		},
		{
			name: "relative shell is not run", shell: "zsh", fallback: "/usr/bin",
			want: "/usr/bin", wantNote: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{answer: func(call string, _ int) ([]byte, error) { return tc.answer(call) }}
			got, note := LoginPATH(context.Background(), r, tc.shell, tc.fallback)
			if got != tc.want {
				t.Errorf("PATH = %q, want %q", got, tc.want)
			}
			if (note != "") != tc.wantNote {
				t.Errorf("note = %q", note)
			}
			if len(r.calls) != tc.wantCalls {
				t.Errorf("calls = %q", r.calls)
			}
			for _, c := range r.calls {
				if !strings.HasPrefix(c, tc.shell+" -") || !strings.HasSuffix(c, script) {
					t.Errorf("ran %q", c)
				}
			}
		})
	}
}

func TestNewSpec(t *testing.T) {
	p := config.Paths{Profile: "work", Config: "/c", Data: "/d"}
	env := map[string]string{"YAD_CONFIG_DIR": "/c", "YAD_DATA_DIR": "/d", "HOME": "/home/o", "ANTHROPIC_API_KEY": "sk-never"}
	h := Host{Home: "/home/o", Getenv: func(k string) string { return env[k] }}

	sp, err := NewSpec(p, "/usr/local/bin/yad", "/usr/bin", h)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"PATH": "/usr/bin", "YAD_CONFIG_DIR": "/c", "YAD_DATA_DIR": "/d"}
	if len(sp.Env) != len(want) {
		t.Errorf("env = %v — only PATH and the directory overrides belong in a unit", sp.Env)
	}
	for k, v := range want {
		if sp.Env[k] != v {
			t.Errorf("env[%s] = %q, want %q", k, sp.Env[k], v)
		}
	}
	if got := strings.Join(sp.Args(), " "); got != "/usr/local/bin/yad --profile work daemon start --foreground" {
		t.Errorf("args = %s", got)
	}
	if sp.LogFile != "/d/logs/service.log" || sp.WorkingDir != "/home/o" {
		t.Errorf("spec = %+v", sp)
	}

	// A fork's install carries its repository, which a self-update in the
	// unit fetches from (decision 0071).
	env["YAD_REPO"] = "someone/fork"
	if sp, err := NewSpec(p, "/usr/local/bin/yad", "/usr/bin", h); err != nil || sp.Env["YAD_REPO"] != "someone/fork" {
		t.Errorf("YAD_REPO in the unit: %v, %v", sp.Env, err)
	}

	for _, k := range envCarried {
		rel := Host{Home: "/home/o", Getenv: func(key string) string {
			if key == k {
				return "state/yad"
			}
			return ""
		}}
		if _, err := NewSpec(p, "/usr/local/bin/yad", "/usr/bin", rel); err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("relative %s: %v", k, err)
		}
	}

	for _, exe := range []string{"yad", filepath.Join(os.TempDir(), "go-build123", "b001", "exe", "yad")} {
		if _, err := NewSpec(p, exe, "/usr/bin", h); err == nil {
			t.Errorf("accepted %s", exe)
		}
	}
}

func TestRootIsRefusedBeforeAnythingRuns(t *testing.T) {
	r := &fakeRunner{}
	h := host(t, r)
	h.EUID = 0
	for _, m := range []Manager{&Launchd{Host: h}, &Systemd{Host: h}} {
		_, errInstall := m.Install(context.Background(), goldenSpec())
		errUninstall := m.Uninstall(context.Background(), "work")
		_, errStatus := m.Status(context.Background(), "work")
		for _, err := range []error{errInstall, errUninstall, errStatus} {
			if err == nil || !strings.Contains(err.Error(), "without sudo") {
				t.Errorf("%T: %v", m, err)
			}
		}
	}
	if len(r.calls) != 0 {
		t.Errorf("ran %q as root", r.calls)
	}
}

func specIn(t *testing.T, h Host) Spec {
	sp := goldenSpec()
	sp.WorkingDir = h.Home
	sp.LogFile = filepath.Join(h.Home, "data", "logs", "service.log")
	return sp
}

func TestLaunchdInstall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		loaded bool
		want   []string
	}{
		{"first install", false, []string{
			"launchctl print gui/501/yad.runner.work",
			"launchctl enable gui/501/yad.runner.work",
			"launchctl bootstrap gui/501 {file}",
		}},
		{"re-install replaces the loaded job", true, []string{
			"launchctl print gui/501/yad.runner.work",
			"launchctl bootout gui/501/yad.runner.work",
			"launchctl print gui/501/yad.runner.work",
			"launchctl enable gui/501/yad.runner.work",
			"launchctl bootstrap gui/501 {file}",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{answer: func(call string, n int) ([]byte, error) {
				// Loaded until the bootout: the first print only.
				if strings.HasPrefix(call, "launchctl print") && !(tc.loaded && n == 0) {
					return nil, errors.New("exit status 113: Could not find service")
				}
				return nil, nil
			}}
			h := host(t, r)
			l := &Launchd{Host: h}
			sp := specIn(t, h)
			if _, err := l.Install(context.Background(), sp); err != nil {
				t.Fatal(err)
			}
			checkCalls(t, r.calls, tc.want, l.File("work"))
			fi, err := os.Stat(l.File("work"))
			if err != nil || fi.Mode().Perm() != 0o644 {
				t.Fatalf("plist: %v %v", fi, err)
			}
			if _, err := os.Stat(filepath.Dir(sp.LogFile)); err != nil {
				t.Errorf("log directory not created: %v", err)
			}
		})
	}
}

func TestLaunchdBootstrapFailureNamesTheSession(t *testing.T) {
	r := &fakeRunner{answer: func(call string, _ int) ([]byte, error) {
		if strings.HasPrefix(call, "launchctl print") || strings.HasPrefix(call, "launchctl bootstrap") {
			return nil, errors.New("exit status 125: Domain does not support specified action")
		}
		return nil, nil
	}}
	h := host(t, r)
	_, err := (&Launchd{Host: h}).Install(context.Background(), specIn(t, h))
	if err == nil || !strings.Contains(err.Error(), "log in to this Mac") {
		t.Errorf("err = %v", err)
	}
}

func TestLaunchdUninstall(t *testing.T) {
	for _, tc := range []struct {
		name         string
		loaded, file bool
		want         []string
	}{
		{"nothing there", false, false, []string{"launchctl print gui/501/yad.runner.work"}},
		{"file only", false, true, []string{"launchctl print gui/501/yad.runner.work"}},
		{"loaded and on disk", true, true, []string{
			"launchctl print gui/501/yad.runner.work",
			"launchctl bootout gui/501/yad.runner.work",
			"launchctl print gui/501/yad.runner.work",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{answer: func(call string, n int) ([]byte, error) {
				if strings.HasPrefix(call, "launchctl print") && !(tc.loaded && n == 0) {
					return nil, errors.New("exit status 113")
				}
				return nil, nil
			}}
			l := &Launchd{Host: host(t, r)}
			if tc.file {
				if err := writeFile(l.File("work"), []byte("x")); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 { // idempotent: the second run finds nothing and succeeds
				if err := l.Uninstall(context.Background(), "work"); err != nil {
					t.Fatal(err)
				}
			}
			checkCalls(t, r.calls[:len(tc.want)], tc.want, l.File("work"))
			if _, err := os.Stat(l.File("work")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("plist left behind: %v", err)
			}
		})
	}
}

func TestLaunchdUninstallWaitsForTheJobToGo(t *testing.T) {
	r := &fakeRunner{answer: func(call string, _ int) ([]byte, error) { return nil, nil }} // never lets go
	l := &Launchd{Host: host(t, r), settle: 1}
	if err := l.Uninstall(context.Background(), "work"); err == nil || !strings.Contains(err.Error(), "still holds") {
		t.Errorf("err = %v", err)
	}
}

func TestLaunchdStatus(t *testing.T) {
	const running = "gui/501/yad.runner.work = {\n\tactive count = 1\n\tpath = /x.plist\n\ttype = LaunchAgent\n\tstate = running\n\n\tprogram = /usr/local/bin/yad\n\tpid = 4242\n\tendpoints = {\n\t\tstate = active\n\t}\n}\n"
	const stopped = "gui/501/yad.runner.work = {\n\tstate = not running\n\tlast exit code = 1\n}\n"
	for _, tc := range []struct {
		name, out string
		loaded    bool
		want      Status
	}{
		{"running", running, true, Status{Loaded: true, Running: true, PID: 4242, Detail: "running"}},
		{"crashed", stopped, true, Status{Loaded: true, Detail: "not running, last exit 1"}},
		{"not loaded", "", false, Status{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{answer: func(string, int) ([]byte, error) {
				if !tc.loaded {
					return nil, errors.New("exit status 113")
				}
				return []byte(tc.out), nil
			}}
			l := &Launchd{Host: host(t, r)}
			st, err := l.Status(context.Background(), "work")
			if err != nil {
				t.Fatal(err)
			}
			tc.want.Name, tc.want.File = "yad.runner.work", l.File("work")
			if st != tc.want {
				t.Errorf("status = %+v, want %+v", st, tc.want)
			}
		})
	}
}

func TestSystemdInstall(t *testing.T) {
	for _, tc := range []struct {
		name, linger string
		wantNotes    bool
	}{
		{"lingering on", "yes\n", false},
		{"lingering off", "no\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{answer: func(call string, _ int) ([]byte, error) {
				if strings.HasPrefix(call, "loginctl") {
					return []byte(tc.linger), nil
				}
				return nil, nil
			}}
			h := host(t, r)
			s := &Systemd{Host: h}
			notes, err := s.Install(context.Background(), specIn(t, h))
			if err != nil {
				t.Fatal(err)
			}
			checkCalls(t, r.calls, []string{
				"systemctl --user show --property=UnitPath --value",
				"systemctl --user stop yad-runner-work.service",
				"systemctl --user daemon-reload",
				"systemctl --user enable yad-runner-work.service",
				"systemctl --user restart yad-runner-work.service",
				"loginctl show-user owner --property=Linger --value",
			}, s.File("work"))
			if (len(notes) > 0) != tc.wantNotes {
				t.Errorf("notes = %q", notes)
			}
			if tc.wantNotes && !strings.Contains(strings.Join(notes, "\n"), "loginctl enable-linger owner") {
				t.Errorf("notes do not name the command: %q", notes)
			}
			if _, err := os.Stat(s.File("work")); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestSystemdWithoutAUserManagerSaysWhy(t *testing.T) {
	r := &fakeRunner{answer: func(string, int) ([]byte, error) {
		return nil, errors.New("exit status 1: Failed to connect to bus: No medium found")
	}}
	h := host(t, r)
	_, err := (&Systemd{Host: h}).Install(context.Background(), specIn(t, h))
	if err == nil || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR=/run/user/501") {
		t.Errorf("err = %v", err)
	}
}

func TestSystemdUninstall(t *testing.T) {
	for _, tc := range []struct {
		name  string
		load  string
		file  bool
		want  []string
		calls int
	}{
		{"nothing there", "not-found", false, []string{
			"systemctl --user show --property=UnitPath --value",
			"systemctl --user show yad-runner-work.service --property=LoadState,ActiveState,SubState,MainPID,Result",
		}, 2},
		{"loaded and on disk", "loaded", true, []string{
			"systemctl --user show --property=UnitPath --value",
			"systemctl --user show yad-runner-work.service --property=LoadState,ActiveState,SubState,MainPID,Result",
			"systemctl --user disable --now yad-runner-work.service",
			"systemctl --user daemon-reload",
			"systemctl --user reset-failed yad-runner-work.service",
		}, 5},
		{"file never loaded", "not-found", true, []string{
			"systemctl --user show --property=UnitPath --value",
			"systemctl --user show yad-runner-work.service --property=LoadState,ActiveState,SubState,MainPID,Result",
			"systemctl --user daemon-reload",
			"systemctl --user reset-failed yad-runner-work.service",
		}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{answer: func(call string, n int) ([]byte, error) {
				switch {
				case strings.Contains(call, "UnitPath"):
					return nil, nil // the stock layout
				case strings.Contains(call, " show ") && n == 0:
					return []byte("LoadState=" + tc.load + "\nActiveState=active\n"), nil
				case strings.Contains(call, " show "):
					return []byte("LoadState=not-found\n"), nil
				case strings.Contains(call, "reset-failed"):
					return nil, errors.New("exit status 1: Unit not loaded") // best effort
				}
				return nil, nil
			}}
			s := &Systemd{Host: host(t, r)}
			if tc.file {
				if err := writeFile(s.File("work"), []byte("x")); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 {
				if err := s.Uninstall(context.Background(), "work"); err != nil {
					t.Fatal(err)
				}
			}
			checkCalls(t, r.calls[:tc.calls], tc.want, s.File("work"))
			if got := len(r.calls) - tc.calls; got != 2 {
				t.Errorf("second uninstall ran %q, want only the two shows", r.calls[tc.calls:])
			}
			if _, err := os.Stat(s.File("work")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("unit left behind: %v", err)
			}
		})
	}
}

func TestSystemdStatus(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      Status
	}{
		{"running", "LoadState=loaded\nActiveState=active\nSubState=running\nMainPID=77\nResult=success\n",
			Status{Loaded: true, Running: true, PID: 77, Detail: "active (running)"}},
		{"restarting", "LoadState=loaded\nActiveState=activating\nSubState=auto-restart\nMainPID=0\nResult=exit-code\n",
			Status{Loaded: true, Detail: "activating (auto-restart), result exit-code"}},
		{"not there", "LoadState=not-found\nActiveState=inactive\n", Status{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{answer: func(call string, _ int) ([]byte, error) {
				if strings.Contains(call, "UnitPath") {
					return nil, nil
				}
				return []byte(tc.out), nil
			}}
			s := &Systemd{Host: host(t, r)}
			st, err := s.Status(context.Background(), "work")
			if err != nil {
				t.Fatal(err)
			}
			tc.want.Name, tc.want.File = "yad-runner-work.service", s.File("work")
			if st != tc.want {
				t.Errorf("status = %+v, want %+v", st, tc.want)
			}
		})
	}
}

func checkCalls(t *testing.T, got, want []string, file string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("calls:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	for i := range want {
		if w := strings.ReplaceAll(want[i], "{file}", file); got[i] != w {
			t.Errorf("call %d = %q, want %q", i, got[i], w)
		}
	}
}

// The unit goes where the running user manager searches, whatever the
// installing shell or environment.d say XDG_CONFIG_HOME is; uninstall asks
// again and removes it from the same place.
func TestSystemdUnitGoesWhereTheManagerLooks(t *testing.T) {
	for _, tc := range []struct {
		name, unitPath, shellXDG string
		want                     string // relative to home; "" means refused
	}{
		{"default layout", "{home}/.config/systemd/user.control /run/user/501/systemd/user.control {home}/.config/systemd/user /etc/systemd/user {home}/.local/share/systemd/user /usr/lib/systemd/user", "", ".config/systemd/user"},
		{"manager started with its own XDG_CONFIG_HOME", "{home}/cfg/systemd/user.control {home}/cfg/systemd/user /etc/systemd/user {home}/.local/share/systemd/user", "", "cfg/systemd/user"},
		{"shell's XDG_CONFIG_HOME is not searched", "{home}/.config/systemd/user.control {home}/.config/systemd/user /etc/systemd/user", "{home}/elsewhere", ".config/systemd/user"},
		{"only the data directory", "/etc/systemd/user {home}/.local/share/systemd/user /usr/lib/systemd/user", "", ".local/share/systemd/user"},
		{"nothing in the home", "/opt/units /etc/systemd/user /usr/lib/systemd/user", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{}
			h := host(t, r)
			sub := func(s string) string { return strings.ReplaceAll(s, "{home}", h.Home) }
			r.answer = func(call string, _ int) ([]byte, error) {
				switch {
				case strings.Contains(call, "UnitPath"):
					return []byte(sub(tc.unitPath) + "\n"), nil
				case strings.Contains(call, " show "):
					return []byte("LoadState=loaded\n"), nil
				}
				return nil, nil
			}
			h.Getenv = func(k string) string {
				if k == "XDG_CONFIG_HOME" {
					return sub(tc.shellXDG)
				}
				return ""
			}
			s := &Systemd{Host: h}
			_, err := s.Install(context.Background(), specIn(t, h))
			if tc.want == "" {
				if err == nil || !strings.Contains(err.Error(), "SYSTEMD_UNIT_PATH") {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(h.Home, tc.want, "yad-runner-work.service")
			if _, err := os.Stat(want); err != nil {
				t.Fatalf("unit not at %s: %v", want, err)
			}
			if tc.shellXDG != "" {
				if _, err := os.Stat(sub(tc.shellXDG)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("something written under the shell's XDG_CONFIG_HOME")
				}
			}
			if err := (&Systemd{Host: h}).Uninstall(context.Background(), "work"); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(want); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("uninstall left %s: %v", want, err)
			}
		})
	}
}

// A Ctrl-C during uninstall cancels launchctl print; that must stop uninstall,
// not read as "no job" and remove the plist from under a running one.
func TestLaunchdCancelledPrintIsNotAnAbsentJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &fakeRunner{answer: func(call string, _ int) ([]byte, error) {
		if strings.HasPrefix(call, "launchctl print") {
			cancel()
			return nil, errors.New("signal: interrupt")
		}
		return nil, nil
	}}
	l := &Launchd{Host: host(t, r)}
	if err := writeFile(l.File("work"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := l.Uninstall(ctx, "work"); !errors.Is(err, context.Canceled) {
		t.Errorf("uninstall = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(l.File("work")); err != nil {
		t.Errorf("plist removed after a cancelled check: %v", err)
	}
	if _, err := l.Status(ctx, "work"); !errors.Is(err, context.Canceled) {
		t.Errorf("status = %v, want context.Canceled", err)
	}
}

// Only a failure to reach the user bus earns the lingering/session advice; any
// other systemctl error is shown as it is.
func TestSystemdBusAdviceOnlyForTheBus(t *testing.T) {
	for _, tc := range []struct {
		msg    string
		advice bool
	}{
		{"exit status 1: Failed to connect to bus: No medium found", true},
		{"exit status 1: Failed to connect to user scope bus via local transport: $DBUS_SESSION_BUS_ADDRESS not set", true},
		{"exit status 5: Unit yad-runner-busy.service not found.", false},
	} {
		r := &fakeRunner{answer: func(string, int) ([]byte, error) { return nil, errors.New(tc.msg) }}
		_, err := (&Systemd{Host: host(t, r)}).Status(context.Background(), "busy")
		if got := err != nil && strings.Contains(err.Error(), "XDG_RUNTIME_DIR"); got != tc.advice {
			t.Errorf("%q: advice %v, err %v", tc.msg, got, err)
		}
	}
}

// A reinstall stops the running runner under the unit it started with, before
// the new unit — whose stop timeout may be shorter — is written and loaded.
func TestSystemdReinstallStopsUnderTheOldUnit(t *testing.T) {
	var s *Systemd
	var atStop []byte
	r := &fakeRunner{answer: func(call string, _ int) ([]byte, error) {
		if strings.Contains(call, " stop ") {
			atStop, _ = os.ReadFile(s.File("work"))
		}
		return nil, nil
	}}
	h := host(t, r)
	s = &Systemd{Host: h}
	// systemd answers a unit it never loaded as the first install finds it.
	r.answer = func(call string, _ int) ([]byte, error) {
		if strings.Contains(call, " stop ") {
			if _, err := os.Stat(s.File("work")); err != nil {
				return nil, errors.New("exit status 5: Failed to stop yad-runner-work.service: Unit yad-runner-work.service not loaded.")
			}
			atStop, _ = os.ReadFile(s.File("work"))
		}
		return nil, nil
	}
	old := specIn(t, h)
	old.StopTimeout = 2 * time.Hour
	if _, err := s.Install(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	r.calls = nil
	shorter := specIn(t, h)
	shorter.StopTimeout = time.Minute
	if _, err := s.Install(context.Background(), shorter); err != nil {
		t.Fatal(err)
	}
	checkCalls(t, r.calls, []string{
		"systemctl --user show --property=UnitPath --value",
		"systemctl --user stop yad-runner-work.service",
		"systemctl --user daemon-reload",
		"systemctl --user enable yad-runner-work.service",
		"systemctl --user restart yad-runner-work.service",
		"loginctl show-user owner --property=Linger --value",
	}, s.File("work"))
	if !strings.Contains(string(atStop), "TimeoutStopSec=7200s") {
		t.Errorf("at the stop the unit said:\n%s\nwant the old unit's 2h timeout", atStop)
	}
	if now, _ := os.ReadFile(s.File("work")); !strings.Contains(string(now), "TimeoutStopSec=60s") {
		t.Errorf("the new unit was not written:\n%s", now)
	}

	// The file removed under a unit systemd still runs: it is stopped all
	// the same.
	if err := os.Remove(s.File("work")); err != nil {
		t.Fatal(err)
	}
	r.answer = func(string, int) ([]byte, error) { return nil, nil }
	r.calls = nil
	if _, err := s.Install(context.Background(), shorter); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) < 2 || r.calls[1] != "systemctl --user stop yad-runner-work.service" {
		t.Errorf("a loaded unit whose file is gone was not stopped first: %q", r.calls)
	}
}
