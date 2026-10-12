package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/service"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// A service manager that could not say whether it holds the profile's job
// is a refusal, as for a start (decision 0074): install and uninstall each
// decide from that answer, so the owner is given the manager's own command
// to ask it, and the yad command to run again where it answers (YAD-176).
func TestServiceInstallAndUninstallRefuseAManagerThatCannotBeAsked(t *testing.T) {
	cause := errors.New("exit status 112: Could not find domain for port identifier")
	for _, tc := range []struct {
		sub    string
		refuse string
		unload bool
	}{
		{"install", "install refused, and nothing changed", false},
		{"uninstall", "uninstall refused, and yad.runner.e3-check left in place", true},
	} {
		t.Run(tc.sub, func(t *testing.T) {
			m := withFakeService(t, 501)
			m.err = &service.UnansweredError{Manager: "launchd", Name: "yad.runner.e3-check", Err: cause}
			code, out, errs := yad(t, "service", tc.sub, "--profile", "e3-check")
			if code != 1 || !strings.Contains(errs, tc.refuse) || !strings.Contains(errs, "exit status 112") {
				t.Fatalf("exit %d:\n%s\n%s", code, errs, out)
			}
			if strings.Contains(out, "installed") || strings.Contains(out, "removed") {
				t.Errorf("a refusal reported success:\n%s", out)
			}
			shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile e3-check service"), dirsEnv(t),
				"yad", "--profile", "e3-check", "service", tc.sub)
			shellwordtest.Check(t, onlyCommand(t, errs, "launchctl print"), "launchctl", "print", "gui/501/yad.runner.e3-check")
			if bootouts := shellwordtest.Commands(errs, "launchctl bootout"); tc.unload {
				if len(bootouts) != 1 {
					t.Fatalf("want one `launchctl bootout` in\n%s", errs)
				}
				shellwordtest.Check(t, bootouts[0], "launchctl", "bootout", "gui/501/yad.runner.e3-check")
			} else if len(bootouts) != 0 {
				t.Errorf("an install offered to unload the job:\n%s", errs)
			}
		})
	}

	// Any other failure is shown as it is, with no refusal dressed around it.
	m := withFakeService(t, 501)
	m.err = errors.New("write /units/yad.runner.default: permission denied")
	code, _, errs := yad(t, "service", "uninstall")
	if code != 1 || strings.Contains(errs, "refused") || !strings.Contains(errs, "permission denied") {
		t.Errorf("exit %d: %s", code, errs)
	}
}
