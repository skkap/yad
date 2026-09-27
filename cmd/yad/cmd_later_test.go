package main

import (
	"os"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// A command the CLI names but does not build points at where the work is now,
// never at an epic that has finished, and what it offers meanwhile runs as
// printed — under the profile and the directories the refusal was made in.
func TestUnbuiltCommandsSayWhereTheWorkIs(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		want     []string // each appears in the refusal
		not      []string // none does
		commands [][]string
	}{{
		name:     "disconnect",
		args:     []string{"disconnect", "work"},
		want:     []string{"still being designed"},
		not:      []string{"DEV-", "Zumino"},
		commands: [][]string{{"daemon", "stop"}},
	}, {
		// Which account a run takes is the soonest refill (0039), so the
		// refusal must not send the owner to reorder a list that decides
		// nothing but ties.
		name:     "account use",
		args:     []string{"account", "use", "claude", "work"},
		want:     []string{"0039", "refills soonest", "only breaks ties"},
		not:      []string{"order runs take", "put the account you want first"},
		commands: [][]string{{"account", "list"}},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, errs := yad(t, append([]string{"--profile", "side"}, tc.args...)...)
			if code != 1 {
				t.Fatalf("exit %d: %s", code, errs)
			}
			for _, stale := range []string{"epic", "DEV-28", "arrives"} {
				if strings.Contains(errs, stale) {
					t.Errorf("the refusal still points at finished work (%q): %s", stale, errs)
				}
			}
			for _, n := range tc.not {
				if strings.Contains(errs, n) {
					t.Errorf("the refusal says %q: %s", n, errs)
				}
			}
			for _, w := range tc.want {
				if !strings.Contains(errs, w) {
					t.Errorf("the refusal does not say %q: %s", w, errs)
				}
			}
			env := map[string]string{"YAD_CONFIG_DIR": os.Getenv("YAD_CONFIG_DIR"), "YAD_DATA_DIR": os.Getenv("YAD_DATA_DIR")}
			for _, c := range tc.commands {
				want := append([]string{"yad", "--profile", "side"}, c...)
				shellwordtest.CheckEnv(t, onlyCommand(t, errs, strings.Join(want, " ")), env, want...)
			}
		})
	}
}

// The help lists both, and no longer promises an epic for either.
func TestHelpDoesNotPromiseAnEpic(t *testing.T) {
	code, out, _ := yad(t, "help")
	if code != 0 || strings.Contains(out, "which epic") || !strings.Contains(out, "disconnect · account use") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}
