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
		commands [][]string
	}{{
		name:     "disconnect",
		args:     []string{"disconnect", "work"},
		want:     []string{"DEV-81"},
		commands: [][]string{{"daemon", "stop"}},
	}, {
		name:     "account use for a harness with accounts",
		args:     []string{"account", "use", "claude", "work"},
		want:     []string{"[harness.claude]", "config.toml", "order runs take"},
		commands: [][]string{{"account", "list"}, {"daemon", "restart"}},
	}, {
		// What the owner typed is not a harness, so it is not repeated as
		// though it named a section of config.toml.
		name:     "account use for something that is not a harness",
		args:     []string{"account", "use", "it's $(id)"},
		want:     []string{"[harness.<id>]"},
		commands: [][]string{{"account", "list"}, {"daemon", "restart"}},
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
