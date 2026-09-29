package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// `yad account remove` marks the home before config.toml drops the label
// (DEV-160, decision 0070), so what stops it half-way leaves a home the
// daemon's next start finishes, and never an unlisted home with nothing to say
// it is going.

func listedClaude(t *testing.T, p config.Paths) []string {
	t.Helper()
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return c.Harness["claude"].Accounts
}

func claudeAccounts(t *testing.T, p config.Paths, labels ...string) {
	t.Helper()
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: labels}}
	if err := config.Save(p, cfg); err != nil {
		t.Fatal(err)
	}
}

// With no daemon, a set-aside that fails leaves config.toml changed and the
// home marked: the next start, or the same command again, finishes it.
func TestAccountRemoveThatCannotSetAsideLeavesTheHomeMarked(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root renames in a directory it may not write")
	}
	p := accountEnv(t)
	claudeAccounts(t, p, "work")
	home, err := account.Ensure(p.Data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(home)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	code, _, errs := yadIn(t, "account", "remove", "claude", "work", "--yes")
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if code == 0 {
		t.Fatal("a removal that could not move the home exited 0")
	}
	if got := listedClaude(t, p); len(got) != 0 {
		t.Errorf("config.toml lists %v", got)
	}
	if !account.Marked(p.Data, "claude", "work") {
		t.Error("the home left behind carries no marker")
	}
	if !strings.Contains(errs, "next start") {
		t.Errorf("the error does not say the next start finishes it: %s", errs)
	}
	shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile default account remove"), dirsEnv(t),
		"yad", "--profile", "default", "account", "remove", "claude", "work", "--yes")

	if code, _, errs := yadIn(t, "account", "remove", "claude", "work", "--yes"); code != 0 {
		t.Fatalf("the removal run again: exit %d: %s", code, errs)
	}
	if _, err := os.Lstat(home); !os.IsNotExist(err) {
		t.Errorf("the home survived the second removal: %v", err)
	}
}

// A marker that cannot be written stops the removal before config.toml
// changes: the account is still listed, still takes runs, and nothing is
// half-done.
func TestAccountRemoveThatCannotMarkChangesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes in a directory it may not write")
	}
	p := accountEnv(t)
	claudeAccounts(t, p, "work")
	home, err := account.Ensure(p.Data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	code, _, errs := yadIn(t, "account", "remove", "claude", "work", "--yes")
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if code == 0 {
		t.Fatal("a removal whose marker could not be written exited 0")
	}
	if got := listedClaude(t, p); !slices.Equal(got, []string{"work"}) {
		t.Errorf("config.toml lists %v; the label should still be there", got)
	}
	if _, err := os.Stat(home); err != nil {
		t.Errorf("the home went: %v", err)
	}
	if !strings.Contains(errs, "was not removed") {
		t.Errorf("the error does not say nothing was removed: %s", errs)
	}
	shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile default account remove"), dirsEnv(t),
		"yad", "--profile", "default", "account", "remove", "claude", "work", "--yes")
}

// `yad account add` of a label whose home carries a marker — a removal that
// was waiting on a run when the daemon died, say — takes it off before the
// login, with no daemon to do it: the home is the one being logged in, and
// the next start must not delete it.
func TestAccountAddTakesTheRemovalMarkerOff(t *testing.T) {
	p := accountHarness(t, claudeE2E)
	interactive = func() bool { return false }
	home, err := account.Ensure(p.Data, "claude", "tl")
	if err != nil {
		t.Fatal(err)
	}
	if err := account.Mark(p.Data, "claude", "tl"); err != nil {
		t.Fatal(err)
	}
	old := stdin
	stdin = strings.NewReader("sk-ant-oat01-not-a-real-token\n")
	code, out, errs := yadIn(t, "account", "add", "claude", "tl", "--token", "-")
	stdin = old
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errs, out)
	}
	if account.Marked(p.Data, "claude", "tl") {
		t.Error("the add left the removal marker on the home it logged in")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatal(err)
	}
}

// A daemon that holds the lock and does not answer Keep may still hold the
// label as doomed, and delete its home when the last run on it ends — in the
// middle of the login (DEV-167). The add stops before the login and before it
// takes the marker off, and says to run it again once the daemon answers.
func TestAccountAddStopsWhenTheDaemonDoesNotAnswerKeep(t *testing.T) {
	for _, tc := range []struct {
		name string
		// socket is whether the daemon's socket is there to connect to: one
		// that takes the request and never answers, or none while the lock
		// is held.
		socket bool
	}{
		{"takes the request and never answers", true},
		{"holds the lock with no socket", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := accountHarness(t, claudeE2E)
			interactive = func() bool { return false }
			home, err := account.Ensure(p.Data, "claude", "tl")
			if err != nil {
				t.Fatal(err)
			}
			if err := account.Mark(p.Data, "claude", "tl"); err != nil {
				t.Fatal(err)
			}
			keepWait = 200 * time.Millisecond
			t.Cleanup(func() { keepWait = 5 * time.Second })
			// Claimed and never served: a connect lands in the listener's
			// backlog, the request is written, and no answer ever comes.
			wedged, err := control.Claim(p)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { wedged.Close() })
			if !tc.socket {
				if err := os.Remove(p.Socket()); err != nil {
					t.Fatal(err)
				}
			}

			old := stdin
			stdin = strings.NewReader("sk-ant-oat01-not-a-real-token\n")
			code, out, errs := yadIn(t, "account", "add", "claude", "tl", "--token", "-")
			stdin = old
			if code == 0 {
				t.Fatalf("the add went ahead past a daemon that did not answer Keep:\n%s%s", out, errs)
			}
			if !account.Marked(p.Data, "claude", "tl") {
				t.Error("the add took the removal marker off without the daemon's answer")
			}
			if account.TokenFileExists(home) {
				t.Error("the add stored its token in a home the daemon may still delete")
			}
			if got := listedClaude(t, p); len(got) != 0 {
				t.Errorf("config.toml lists %v", got)
			}
			if !strings.Contains(errs, "was not added") {
				t.Errorf("the error does not say the account was not added: %s", errs)
			}
			shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile default status"), dirsEnv(t),
				"yad", "--profile", "default", "status")
			shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile default account add"), dirsEnv(t),
				"yad", "--profile", "default", "account", "add", "claude", "tl", "--token", "-")
		})
	}
}
