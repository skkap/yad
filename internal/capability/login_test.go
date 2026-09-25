package capability

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// fakeLoginHarness installs a claude or codex that answers its version, its
// --help (with every flag a Claude run passes) and its own login check the way
// the real one does, logged in or not, and records each login check in calls.
func fakeLoginHarness(t *testing.T, id, answer string) (calls string) {
	t.Helper()
	noTools(t)
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	var status string
	switch id + "/" + answer {
	case "claude/in":
		status = `echo '{"loggedIn": true, "email": "owner@example.com"}'`
	case "claude/out":
		status = `echo '{"loggedIn": false}'; exit 1`
	case "codex/in":
		status = `echo 'Logged in using ChatGPT'`
	case "codex/out":
		status = `echo 'Not logged in'; exit 1`
	default: // unreadable: an answer that is neither
		status = `echo 'Error loading configuration: /home/owner/.codex/config.toml'; exit 1`
	}
	check := "auth status"
	if id == "codex" {
		check = "login status"
	}
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"'" + check + "') echo x >> '" + calls + "'; " + status + " ;;\n" +
		"--help) echo '  --system-prompt-snapshot <on|off>' ;;\n" +
		"*) echo '9.9.9' ;;\n" +
		"esac\n"
	bin := filepath.Join(dir, id)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAD_"+strings.ToUpper(id)+"_PATH", bin)
	return calls
}

func report(t *testing.T, id string, cfg config.Config) v1.HarnessReport {
	t.Helper()
	found := Detect(context.Background())
	DefaultLogins(context.Background(), found, cfg)
	for _, h := range Harnesses(found, cfg, nil) {
		if h.ID == id {
			return h
		}
	}
	t.Fatalf("no %s in the report", id)
	return v1.HarnessReport{}
}

// A harness on its own default home takes runs only while that home holds a
// login (decision 0053). Logged out, it is not drivable, and says how to log
// in; a check that cannot answer is a warning and leaves it drivable.
func TestAHarnessOnItsOwnLoginTakesRunsOnlyWhileLoggedIn(t *testing.T) {
	for _, tc := range []struct {
		id, answer string
		drivable   bool
		warned     bool
	}{
		{"claude", "in", true, false},
		{"claude", "out", false, false},
		{"codex", "in", true, false},
		{"codex", "out", false, false},
		{"codex", "unreadable", true, true},
	} {
		t.Run(tc.id+"/"+tc.answer, func(t *testing.T) {
			fakeLoginHarness(t, tc.id, tc.answer)
			h := report(t, tc.id, config.Default())
			doc := v1.Capabilities{Harnesses: []v1.HarnessReport{h}}
			if got := Drivable(doc, tc.id); got != tc.drivable {
				t.Fatalf("drivable = %v, want %v: %+v", got, tc.drivable, h)
			}
			if !tc.drivable && !strings.Contains(h.Error, "not logged in") {
				t.Errorf("error %q does not say it is not logged in", h.Error)
			}
			warned := false
			for _, w := range h.Warnings {
				if strings.Contains(w, "could not tell whether it is logged in") {
					warned = true
				}
			}
			if warned != tc.warned {
				t.Errorf("login warning = %v, want %v: %q", warned, tc.warned, h.Warnings)
			}
			// What the harness printed — an e-mail, a path — never reaches
			// the document a hub receives.
			for _, s := range append([]string{h.Error}, h.Warnings...) {
				if strings.Contains(s, "example.com") || strings.Contains(s, "/home/owner") {
					t.Errorf("the report quotes the harness: %q", s)
				}
			}
		})
	}
}

// With accounts, a harness's runs never use its default home, so that home's
// login is neither asked about nor held against it.
func TestAHarnessWithAccountsIsNotAskedAboutItsDefaultLogin(t *testing.T) {
	calls := fakeLoginHarness(t, "claude", "out")
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"work"}}}
	h := report(t, "claude", cfg)
	if h.Error != "" {
		t.Errorf("an account-backed claude was reported unable to run: %q", h.Error)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Error("the default home's login was checked for a harness with accounts")
	}
}

// The answer is kept for a minute, so a daemon re-probing every 15 seconds
// does not start the harness four times a minute.
func TestTheDefaultLoginAnswerIsKept(t *testing.T) {
	calls := fakeLoginHarness(t, "claude", "in")
	for range 3 {
		report(t, "claude", config.Default())
	}
	b, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "x"); n != 1 {
		t.Errorf("the login was checked %d times in a row, want once", n)
	}
}

// The login a logged-out report names is a command an owner pastes: sh runs
// the harness's own login with the home the runner's environment moved it to
// set apart as a placeholder, never the path itself.
func TestTheLoginCommandRunsAsPrinted(t *testing.T) {
	for _, tc := range []struct {
		id, homeVar string
		moved       bool
		want        []string
	}{
		{"claude", "CLAUDE_CONFIG_DIR", false, []string{"claude", "auth", "login"}},
		{"claude", "CLAUDE_CONFIG_DIR", true, []string{"claude", "auth", "login"}},
		{"codex", "CODEX_HOME", false, []string{"codex", "login"}},
		{"codex", "CODEX_HOME", true, []string{"codex", "login"}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			fakeLoginHarness(t, tc.id, "out")
			env := map[string]string{tc.homeVar: ""}
			if tc.moved {
				moved := filepath.Join(t.TempDir(), "it's home")
				t.Setenv(tc.homeVar, moved)
				env[tc.homeVar] = "<runner " + tc.homeVar + ">"
			}
			h := report(t, tc.id, config.Default())
			if tc.moved && strings.Contains(h.Error, "it's home") {
				t.Errorf("the error names the runner's home: %q", h.Error)
			}
			cmds := shellwordtest.Commands(h.Error, tc.id)
			if len(cmds) != 1 {
				t.Fatalf("want one %s command in %q, got %q", tc.id, h.Error, cmds)
			}
			shellwordtest.CheckEnv(t, cmds[0], env, tc.want...)
		})
	}
}

// yad doctor calls a missing login what it is, not a broken harness.
func TestDetectMarksAMissingLogin(t *testing.T) {
	fakeLoginHarness(t, "claude", "out")
	found := Detect(context.Background())
	DefaultLogins(context.Background(), found, config.Default())
	for _, d := range found {
		if d.ID == "claude" && !d.NeedsLogin {
			t.Errorf("a logged-out claude is not marked as needing a login: %+v", d)
		}
	}
}
