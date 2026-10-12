package service

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// A launchctl print that fails with anything but 113 is launchd not
// answering — over SSH with no desktop session, there is no GUI domain to
// ask. Install and uninstall then change nothing: an uninstall that took it
// for an absent job would remove the plist from under a job still loaded,
// and an install would bootstrap a second time beside it (YAD-176).
func TestLaunchdThatCannotBeAskedChangesNothing(t *testing.T) {
	unanswered := func(call string, _ int) ([]byte, error) {
		if strings.HasPrefix(call, "launchctl print") {
			return nil, errors.New("exit status 112: Could not find domain for port identifier")
		}
		return nil, nil
	}
	for _, tc := range []struct {
		name string
		run  func(l *Launchd, sp Spec) error
	}{
		{"install", func(l *Launchd, sp Spec) error { _, err := l.Install(context.Background(), sp); return err }},
		{"uninstall", func(l *Launchd, sp Spec) error { return l.Uninstall(context.Background(), sp.Profile) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{answer: unanswered}
			h := host(t, r)
			l := &Launchd{Host: h}
			if err := writeFile(l.File("work"), []byte("the plist launchd loaded\n")); err != nil {
				t.Fatal(err)
			}
			err := tc.run(l, specIn(t, h))
			var unanswered *UnansweredError
			if !errors.As(err, &unanswered) || unanswered.Name != "yad.runner.work" || !strings.Contains(err.Error(), "exit status 112") {
				t.Fatalf("err = %v, want launchd's own failure as an UnansweredError", err)
			}
			checkCalls(t, r.calls, []string{"launchctl print gui/501/yad.runner.work"}, "")
			if b, err := os.ReadFile(l.File("work")); err != nil || string(b) != "the plist launchd loaded\n" {
				t.Errorf("plist changed: %q, %v", b, err)
			}
		})
	}

	// A bootout that fails, then a print that cannot answer, is not a job
	// that went in between.
	r := &fakeRunner{answer: func(call string, n int) ([]byte, error) {
		switch {
		case strings.HasPrefix(call, "launchctl print") && n == 0:
			return nil, nil // loaded
		case strings.HasPrefix(call, "launchctl bootout"):
			return nil, errors.New("exit status 5: Input/output error")
		}
		return unanswered(call, n)
	}}
	l := &Launchd{Host: host(t, r)}
	if err := writeFile(l.File("work"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := l.Uninstall(context.Background(), "work"); err == nil {
		t.Error("an uninstall whose bootout failed and whose re-check could not be answered succeeded")
	}
	if _, err := os.Stat(l.File("work")); err != nil {
		t.Errorf("plist removed: %v", err)
	}
}

// A bootout launchd took has changed the job, so a launchd that stops
// answering after it — the owner logged out during the drain — is not an
// UnansweredError, which the CLI reports as a refusal that changed nothing.
func TestLaunchdUnansweredAfterABootoutIsNotARefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(l *Launchd, sp Spec) error
	}{
		{"install", func(l *Launchd, sp Spec) error { _, err := l.Install(context.Background(), sp); return err }},
		{"uninstall", func(l *Launchd, sp Spec) error { return l.Uninstall(context.Background(), sp.Profile) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{answer: func(call string, n int) ([]byte, error) {
				if strings.HasPrefix(call, "launchctl print") && n > 0 {
					return nil, errors.New("exit status 112: Could not find domain for port identifier")
				}
				return nil, nil // loaded at first, and the bootout is taken
			}}
			h := host(t, r)
			l := &Launchd{Host: h, settle: 1}
			if err := writeFile(l.File("work"), []byte("the plist launchd loaded\n")); err != nil {
				t.Fatal(err)
			}
			err := tc.run(l, specIn(t, h))
			var unanswered *UnansweredError
			if err == nil || errors.As(err, &unanswered) || !strings.Contains(err.Error(), "accepted the bootout") {
				t.Fatalf("err = %v, want a plain error saying the bootout was taken", err)
			}
			checkCalls(t, r.calls, []string{
				"launchctl print gui/501/yad.runner.work",
				"launchctl bootout gui/501/yad.runner.work",
				"launchctl print gui/501/yad.runner.work",
			}, "")
			if b, err := os.ReadFile(l.File("work")); err != nil || string(b) != "the plist launchd loaded\n" {
				t.Errorf("plist changed: %q, %v", b, err)
			}
		})
	}
}

// With no user bus, systemd install and uninstall stop at the first question,
// before the unit is touched, and say it is the manager that did not answer.
func TestSystemdThatCannotBeAskedChangesNothing(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(s *Systemd, sp Spec) error
	}{
		{"install", func(s *Systemd, sp Spec) error { _, err := s.Install(context.Background(), sp); return err }},
		{"uninstall", func(s *Systemd, sp Spec) error { return s.Uninstall(context.Background(), sp.Profile) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &fakeRunner{answer: func(string, int) ([]byte, error) {
				return nil, errors.New("exit status 1: Failed to connect to bus: No medium found")
			}}
			h := host(t, r)
			s := &Systemd{Host: h}
			if err := writeFile(s.File("work"), []byte("the unit systemd loaded\n")); err != nil {
				t.Fatal(err)
			}
			err := tc.run(s, specIn(t, h))
			var unanswered *UnansweredError
			if !errors.As(err, &unanswered) || unanswered.Name != "yad-runner-work.service" || !strings.Contains(err.Error(), "XDG_RUNTIME_DIR") {
				t.Fatalf("err = %v, want the bus failure as an UnansweredError", err)
			}
			checkCalls(t, r.calls, []string{"systemctl --user show --property=UnitPath --value"}, "")
			if b, err := os.ReadFile(s.File("work")); err != nil || string(b) != "the unit systemd loaded\n" {
				t.Errorf("unit changed: %q, %v", b, err)
			}
		})
	}
}

// The commands a refusal offers are the managers' own.
func TestStatusAndUnloadCommands(t *testing.T) {
	h := host(t, &fakeRunner{})
	for _, tc := range []struct {
		m              Manager
		status, unload string
	}{
		{&Launchd{Host: h}, "launchctl print gui/501/yad.runner.work", "launchctl bootout gui/501/yad.runner.work"},
		{&Systemd{Host: h}, "systemctl --user status yad-runner-work.service", "systemctl --user disable --now yad-runner-work.service"},
	} {
		if got := strings.Join(tc.m.StatusCommand("work"), " "); got != tc.status {
			t.Errorf("status command %q, want %q", got, tc.status)
		}
		if got := strings.Join(tc.m.UnloadCommand("work"), " "); got != tc.unload {
			t.Errorf("unload command %q, want %q", got, tc.unload)
		}
	}
}
