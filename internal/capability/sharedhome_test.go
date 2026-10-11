package capability

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

func noProcesses(context.Context) ([]Process, error) { return nil, nil }

// withProcesses makes table the process table for one test.
func withProcesses(t *testing.T, table []Process, err error) {
	t.Helper()
	prev := ProcessesForTests
	ProcessesForTests = func(context.Context) ([]Process, error) { return table, err }
	ForgetSharedHomes()
	t.Cleanup(func() {
		ProcessesForTests = prev
		ForgetSharedHomes()
	})
}

// session writes the record Claude keeps for a live session in home.
func session(t *testing.T, home string, pid int, start, kind string) {
	t.Helper()
	dir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"pid":%d,"sessionId":"x","cwd":"/somewhere/private","procStart":%q,"kind":%q}`, pid, start, kind)
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", pid)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readyClaude() []harness.Detected {
	return []harness.Detected{{Harness: harness.Harness{ID: "claude", Label: "Claude Code", Kind: harness.FirstClass}, Present: true, Path: "/x/claude", Version: "2.1.296"}}
}

const started = "Sun Oct 11 09:18:10 2026"

// Another Claude that yad did not start, running from the home a run uses, is
// found from Claude's own records and the process table: never from another
// process's environment or arguments. What is counted is a live process the
// record names — not a record whose process has gone, not a pid since taken
// by something else, and not a run of yad's own.
func TestAnotherClaudeInTheHomeIsFoundFromItsOwnRecords(t *testing.T) {
	self := os.Getpid()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, home string)
		table []Process
		want  []int
		bg    bool
	}{
		{"a live session", func(t *testing.T, home string) { session(t, home, 500, started, "interactive") },
			[]Process{{PID: 500, PPID: 400, Comm: "/Users/o/.local/bin/claude", Start: "Sun Oct 11  9:18:10 2026"}, {PID: 400, PPID: 1, Comm: "-zsh"}},
			[]int{500}, false},
		{"one started by npm's node, known by its start time", func(t *testing.T, home string) { session(t, home, 501, started, "interactive") },
			[]Process{{PID: 501, PPID: 1, Comm: "node", Start: started + "    "}},
			[]int{501}, false},
		{"a record whose process has gone", func(t *testing.T, home string) { session(t, home, 502, started, "interactive") },
			[]Process{{PID: 9, PPID: 1, Comm: "claude"}}, nil, false},
		{"a pid since taken by something else", func(t *testing.T, home string) { session(t, home, 503, started, "interactive") },
			[]Process{{PID: 503, PPID: 1, Comm: "node", Start: "Mon Oct 12 01:00:00 2026"}}, nil, false},
		{"a background session", func(t *testing.T, home string) { session(t, home, 504, started, "background") },
			[]Process{{PID: 504, PPID: 1, Comm: "claude", Start: started}}, []int{504}, true},
		{"the --bg supervisor", func(t *testing.T, home string) {
			if err := os.MkdirAll(filepath.Join(home, "daemon"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "daemon", "roster.json"), []byte(`{"proto":1,"supervisorPid":505,"workers":{}}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}, []Process{{PID: 505, PPID: 1, Comm: "/Users/o/.local/bin/claude"}}, []int{505}, true},
		{"a run of this yad's", func(t *testing.T, home string) { session(t, home, 506, started, "sdk") },
			[]Process{{PID: 506, PPID: self, Comm: "claude", Start: started}}, nil, false},
		{"a run of another yad's", func(t *testing.T, home string) { session(t, home, 507, started, "sdk") },
			[]Process{{PID: 507, PPID: 300, Comm: "claude", Start: started}, {PID: 300, PPID: 1, Comm: "/usr/local/bin/yad"}}, nil, false},
		{"a record named for another pid than it holds", func(t *testing.T, home string) {
			session(t, home, 508, started, "interactive")
			if err := os.Rename(filepath.Join(home, "sessions", "508.json"), filepath.Join(home, "sessions", "509.json")); err != nil {
				t.Fatal(err)
			}
		}, []Process{{PID: 509, PPID: 1, Comm: "claude", Start: started}}, nil, false},
		{"no records at all", func(*testing.T, string) {}, []Process{{PID: 510, PPID: 1, Comm: "claude"}}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			tc.setup(t, home)
			withProcesses(t, tc.table, nil)
			got, err := otherClaudes(context.Background(), home, self)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(got.pids) != fmt.Sprint(tc.want) || got.daemon != tc.bg {
				t.Errorf("found %v (background %v), want %v (%v)", got.pids, got.daemon, tc.want, tc.bg)
			}
		})
	}
}

// The document's warning is a count and a next action, with no path and no
// pid: it reaches every hub. Doctor's names the pids. Neither is an error:
// the harness stays drivable.
func TestAnotherClaudeInTheDefaultHomeIsAWarningNeverAnError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	session(t, home, 600, started, "background")
	session(t, home, 601, started, "interactive")
	withProcesses(t, []Process{{PID: 600, PPID: 1, Comm: "claude", Start: started}, {PID: 601, PPID: 1, Comm: "claude", Start: started}}, nil)
	paths := config.Paths{Profile: "work"}

	for _, detail := range []bool{false, true} {
		found := readyClaude()
		ForgetSharedHomes()
		if err := SharedHomes(context.Background(), found, config.Default(), nil, paths.RemoteCommand, detail); err != nil {
			t.Fatal(err)
		}
		if !found[0].Ready() || found[0].Error != "" {
			t.Fatalf("the harness is no longer drivable: %+v", found[0])
		}
		if len(found[0].Warnings) != 1 {
			t.Fatalf("warnings = %q", found[0].Warnings)
		}
		w := found[0].Warnings[0]
		for _, s := range []string{"2 other Claude processes that yad did not start", "run from the home this runner's Claude runs use", "`claude --bg` among them", "harness_silent"} {
			if !strings.Contains(w, s) {
				t.Errorf("warning %q lacks %q", w, s)
			}
		}
		if strings.Contains(w, home) || strings.Contains(w, "/somewhere/private") {
			t.Errorf("warning %q names a path", w)
		}
		if strings.Contains(w, "600") != detail {
			t.Errorf("detail %v, and the warning %q names the pids or does not", detail, w)
		}
		cmds := shellwordtest.Commands(w, "yad ")
		if len(cmds) != 1 {
			t.Fatalf("want one command in %q", w)
		}
		shellwordtest.Check(t, cmds[0], "yad", "--profile", "work", "account", "add", "claude", "<label>")
	}
}

// A harness with accounts never runs from its default home, so only the
// accounts' homes are looked in.
func TestOnlyTheHomesRunsUseAreLookedIn(t *testing.T) {
	def := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", def)
	session(t, def, 700, started, "interactive")
	data := t.TempDir()
	home, err := account.Ensure(data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	session(t, home, 701, started, "interactive")
	withProcesses(t, []Process{{PID: 700, PPID: 1, Comm: "claude", Start: started}, {PID: 701, PPID: 1, Comm: "claude", Start: started}}, nil)
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"work"}}}
	accounts := []account.Account{{Harness: "claude", Label: "work", Home: home}}

	found := readyClaude()
	if err := SharedHomes(context.Background(), found, cfg, accounts, config.Paths{}.RemoteCommand, true); err != nil {
		t.Fatal(err)
	}
	if len(found[0].Warnings) != 1 || !strings.Contains(found[0].Warnings[0], `account "work": 1 other Claude process that yad did not start (pid 701) runs from its home`) {
		t.Errorf("warnings = %q, want one for account work alone", found[0].Warnings)
	}
}

// A process table that cannot be read is nothing to warn about: absence is
// data, and doctor says why from the error.
func TestNoProcessTableIsNoWarning(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	session(t, home, 800, started, "interactive")
	withProcesses(t, nil, errors.New("there is no ps on this machine to list its processes with"))
	found := readyClaude()
	err := SharedHomes(context.Background(), found, config.Default(), nil, config.Paths{}.RemoteCommand, true)
	if err == nil || len(found[0].Warnings) != 0 {
		t.Errorf("error %v, warnings %q", err, found[0].Warnings)
	}
}

// ps's two answers as macOS prints them: a command name that is a path with
// spaces in it, and a start time padded on both sides.
func TestPSIsReadAsMacOSPrintsIt(t *testing.T) {
	procs, err := parseProcs([]byte("    1     0 /sbin/launchd\n  612     1 /Applications/Google Chrome.app/Contents/MacOS/Google Chrome\n14428 90501 claude\nnonsense\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Process{{PID: 1, Comm: "/sbin/launchd"}, {PID: 612, PPID: 1, Comm: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}, {PID: 14428, PPID: 90501, Comm: "claude"}}
	if fmt.Sprint(procs) != fmt.Sprint(want) {
		t.Errorf("procs = %+v", procs)
	}
	if _, err := parseProcs([]byte("\n")); err == nil {
		t.Error("an empty table read as one")
	}
	starts := parseStarts([]byte("59157 Thu Oct  8 16:31:41 2026    \n14428 Sun Oct 11 09:18:10 2026    \n"))
	if !sameTime(starts[59157], "Thu Oct  8 16:31:41 2026") || !sameTime(starts[14428], started) {
		t.Errorf("starts = %v", starts)
	}
}
