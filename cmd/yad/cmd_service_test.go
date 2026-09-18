package main

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/runner"
	"github.com/skkap/yad/internal/service"
)

// fakeManager records what the command asked of the service manager.
type fakeManager struct {
	installed   []service.Spec
	uninstalled []string
	notes       []string
}

func (f *fakeManager) Name(p string) string                { return "yad.runner." + p }
func (f *fakeManager) File(p string) string                { return "/units/yad.runner." + p }
func (f *fakeManager) Render(service.Spec) ([]byte, error) { return nil, nil }
func (f *fakeManager) Install(_ context.Context, s service.Spec) ([]string, error) {
	f.installed = append(f.installed, s)
	return f.notes, nil
}
func (f *fakeManager) Uninstall(_ context.Context, p string) error {
	f.uninstalled = append(f.uninstalled, p)
	return nil
}
func (f *fakeManager) Status(_ context.Context, p string) (service.Status, error) {
	return service.Status{Name: f.Name(p), File: f.File(p), Installed: true, Loaded: true, Running: true, PID: 9}, nil
}

// shellRunner is a login shell that prints a fixed PATH.
type shellRunner struct{ path string }

func (s shellRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return []byte("motd\n__YAD_LOGIN_PATH__" + s.path + "__YAD_LOGIN_PATH__\n"), nil
}

func withFakeService(t *testing.T, euid int) *fakeManager {
	t.Helper()
	m := &fakeManager{}
	env := map[string]string{"SHELL": "/bin/zsh", "PATH": "/usr/bin"}
	h := service.Host{Home: t.TempDir(), UID: 501, EUID: euid, User: "owner",
		Getenv: func(k string) string {
			if v, ok := env[k]; ok {
				return v
			}
			return os.Getenv(k)
		},
		Run: shellRunner{"/home/owner/.local/bin:/usr/bin"}}
	oldM, oldE := serviceManager, executable
	serviceManager = func() (service.Manager, service.Host, error) { return m, h, nil }
	executable = func() (string, error) { return "/usr/local/bin/yad", nil }
	t.Cleanup(func() { serviceManager, executable = oldM, oldE })
	return m
}

func TestServiceInstall(t *testing.T) {
	m := withFakeService(t, 501)
	code, out, errs := yad(t, "service", "install", "--profile", "e3-check")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if len(m.installed) != 1 {
		t.Fatalf("installed %d times", len(m.installed))
	}
	sp := m.installed[0]
	if sp.Profile != "e3-check" || sp.Env["PATH"] != "/home/owner/.local/bin:/usr/bin" {
		t.Errorf("spec = %+v", sp)
	}
	// The directories the test set must reach the unit, or the service would
	// run a different, empty runner.
	if sp.Env["YAD_CONFIG_DIR"] != os.Getenv("YAD_CONFIG_DIR") || sp.Env["YAD_DATA_DIR"] == "" {
		t.Errorf("env = %v", sp.Env)
	}
	if !strings.HasPrefix(sp.LogFile, os.Getenv("YAD_DATA_DIR")) {
		t.Errorf("log %s is outside the data directory", sp.LogFile)
	}
	for _, want := range []string{"installed yad.runner.e3-check", "runs as owner", "login shell", "no hub is connected"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// The units a service install renders stop the runner no sooner than its
// drain can end: the drain wait from config.toml, the cancel ladder and a
// last flush. Changing the wait takes a reinstall.
func TestServiceInstallDerivesTheStopTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		toml string
		wait time.Duration
	}{
		{"the default drain wait", "", config.DefaultDrainWait},
		{"a longer one", "capacity = 2\n[drain]\nwait = \"2h\"\n", 2 * time.Hour},
		{"none at all", "capacity = 2\n[drain]\nwait = \"0s\"\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := withFakeService(t, 501)
			p := newProfile(t)
			if tc.toml != "" {
				if err := os.WriteFile(filepath.Join(p.config, "config.toml"), []byte(tc.toml), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			code, out, errs := p.yad("", "service", "install", "--profile", "e3-check-stop")
			if code != 0 || len(m.installed) != 1 {
				t.Fatalf("exit %d: %s", code, errs)
			}
			sp := m.installed[0]
			budget := runner.StopBudget(tc.wait)
			if sp.StopTimeout != budget || budget < tc.wait+45*time.Second {
				t.Fatalf("stop timeout %s, want the budget %s for a %s drain wait", sp.StopTimeout, budget, tc.wait)
			}
			secs := strconv.Itoa(int(max(budget, 30*time.Second).Seconds()))
			plist, err := (&service.Launchd{}).Render(sp)
			if err != nil {
				t.Fatal(err)
			}
			if want := "<key>ExitTimeOut</key>\n\t<integer>" + secs + "</integer>"; !strings.Contains(string(plist), want) {
				t.Errorf("rendered plist lacks %q", want)
			}
			unit, err := (&service.Systemd{}).Render(sp)
			if err != nil {
				t.Fatal(err)
			}
			if want := "TimeoutStopSec=" + secs + "s\n"; !strings.Contains(string(unit), want) {
				t.Errorf("rendered unit lacks %q", want)
			}
			if !strings.Contains(out, "run install again after changing [drain] wait") {
				t.Errorf("install does not say a changed drain wait needs a reinstall:\n%s", out)
			}
		})
	}
}

func TestServiceTakesTheGlobalProfile(t *testing.T) {
	m := withFakeService(t, 501)
	if code, _, errs := yad(t, "--profile", "work", "service", "uninstall"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if code, _, errs := yad(t, "service", "uninstall"); code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if strings.Join(m.uninstalled, ",") != "work,default" {
		t.Errorf("uninstalled %q", m.uninstalled)
	}
}

func TestServiceRefusesRoot(t *testing.T) {
	for _, sub := range []string{"install", "uninstall", "status"} {
		m := withFakeService(t, 0)
		code, _, errs := yad(t, "service", sub)
		if code != 1 || !strings.Contains(errs, "without sudo") {
			t.Errorf("%s as root: exit %d, %q", sub, code, errs)
		}
		if len(m.installed)+len(m.uninstalled) != 0 {
			t.Errorf("%s as root reached the service manager", sub)
		}
	}
}

func TestServiceInstallRefusesABrokenConfig(t *testing.T) {
	m := withFakeService(t, 501)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("capacity = [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAD_CONFIG_DIR", dir)
	t.Setenv("YAD_DATA_DIR", t.TempDir())
	var out, errs strings.Builder
	if code := run(context.Background(), []string{"service", "install"}, &out, &errs); code != 1 {
		t.Errorf("exit %d with a broken config.toml: %s", code, errs.String())
	}
	if len(m.installed) != 0 {
		t.Error("a runner that cannot read its config was installed")
	}
}

func TestServiceStatus(t *testing.T) {
	withFakeService(t, 501)
	code, out, _ := yad(t, "service", "status")
	if code != 0 || !strings.Contains(out, "yad.runner.default running, pid 9") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if code, _, errs := yad(t, "service", "restart"); code != 1 || !strings.Contains(errs, "install, uninstall or status") {
		t.Errorf("unknown subcommand: exit %d %q", code, errs)
	}
}
