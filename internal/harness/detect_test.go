package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"bare", "2.4.1\n", "2.4.1"},
		{"banner", "claude 2.4.1 (Claude Code)\n", "2.4.1"},
		{"update notice below", "codex 0.58.0\n\nA new version is available\n", "0.58.0"},
		{"leading blank line", "\n  opencode 1.2.3  \n", "1.2.3"},
		{"a warning printed first", "Warning: proxy https://user:hunter2@10.0.0.1:8080/ from /Users/someone/.npmrc\n0.9.1\n", "0.9.1"},
		{"a path and a credential beside it", "codex-cli 0.147.0 (/Users/someone/.codex, https://user:hunter2@proxy/)\n", "0.147.0"},
		{"a pre-release", "gemini 1.0.0-beta.12\n", "1.0.0-beta.12"},
		{"no version at all", "dyld: Library not loaded: /Users/someone/lib/libnode.dylib\n", ""},
		{"a suffix past the bound", "tool 1.2.3-" + strings.Repeat("a", 64) + "\n", "1.2.3-" + strings.Repeat("a", 26)},
		{"empty", "\n\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseVersion(tc.raw); got != tc.want {
				t.Errorf("ParseVersion(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// A machine with nothing installed must still produce a full, ordered report —
// absence is a capability fact, not a failure.
func TestDetectReportsAbsentHarnesses(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, h := range Catalog() {
		t.Setenv(h.EnvPath, "")
	}
	got := Detect(context.Background())
	if len(got) != len(Catalog()) {
		t.Fatalf("Detect returned %d entries, want %d", len(got), len(Catalog()))
	}
	for i, d := range got {
		if d.ID != Catalog()[i].ID {
			t.Errorf("entry %d is %q, want catalog order %q", i, d.ID, Catalog()[i].ID)
		}
		if d.Present || d.Ready() {
			t.Errorf("%s reported present with an empty PATH", d.ID)
		}
	}
}

// A present harness whose version probe fails is reported broken, not dropped,
// and is never Ready — a runner must not take work for it.
func TestDetectReportsBrokenHarness(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("YAD_CLAUDE_PATH", "")
	for _, d := range Detect(context.Background()) {
		if d.ID != "claude" {
			continue
		}
		if !d.Present || d.Error == "" || d.Ready() {
			t.Errorf("broken claude: present=%v error=%q ready=%v", d.Present, d.Error, d.Ready())
		}
		return
	}
	t.Fatal("claude missing from Detect")
}

func TestDetectReadsVersionAndHonoursEnvPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "anything")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'codex-cli 0.147.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("YAD_CODEX_PATH", bin)
	for _, d := range Detect(context.Background()) {
		if d.ID != "codex" {
			continue
		}
		if !d.Present || d.Error != "" || d.Version != "0.147.0" || d.Path != bin {
			t.Errorf("codex = %+v", d)
		}
		return
	}
	t.Fatal("codex missing from Detect")
}

func TestLookup(t *testing.T) {
	// Claude and Codex have adapters; the rest are recognised, and nothing
	// becomes first-class without one (CLAUDE.md).
	for _, id := range []string{"claude", "codex"} {
		if h, ok := Lookup(id); !ok || h.Kind != FirstClass {
			t.Errorf("Lookup(%s) = %+v, %v", id, h, ok)
		}
	}
	if h, ok := Lookup("gemini"); !ok || h.Kind != Recognised {
		t.Errorf("Lookup(gemini) = %+v, %v", h, ok)
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("Lookup(nope) found something")
	}
}

// Leader fate is what these two tests prove, and no absolute duration proves it
// on a loaded machine: spawning a shell here took over a second under the full
// suite, which is what made the old budgets red in a tree nobody had touched
// ("took 1.385s, want under 1s", DEV-56). Nor does the outcome separate the two
// — a probe held by a detached child answers the same in the end, once its
// deadline closes the pipe underneath it. What separates them is which side of
// the deadline the answer came from: the leader's, or the deadline's. So every
// case names that side, and the deadline is either far above any spawn a
// machine could plausibly take, or brought by the test itself.
const (
	// Never reached while leader fate holds, and far above any plausible spawn:
	// a case that returns with its leader returns long before this.
	leaderProbeTimeout = 10 * time.Second
	// Reached on purpose, so the one case that waits out a real versionTimeout
	// waits as little as it can.
	hangingProbeTimeout = 250 * time.Millisecond
	// Every child below sleeps a minute. A probe still running this long after
	// its deadline is held by one of them, which is the regression these tests
	// exist for; it is a bound on a broken build, not a budget for a busy one.
	afterTheDeadline = 15 * time.Second
)

// A probe that hangs takes its descendants with it: a launcher that forks and
// hangs would otherwise leave one orphan per daemon tick. The deadline is the
// test's own, so the grandchild is in place before it arrives however slow the
// machine is; that a versionTimeout ends a hanging probe by itself is the
// "hangs" case of TestProbeOutcomeFollowsTheLeader.
func TestHangingProbeIsBoundedAndLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "grandchild.pid")
	script := filepath.Join(dir, "codex")
	body := "#!/bin/sh\nsleep 60 &\necho $! > " + pidfile + "\nsleep 60\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAD_CODEX_PATH", script)
	old := versionTimeout
	versionTimeout = leaderProbeTimeout
	t.Cleanup(func() { versionTimeout = old })

	h, _ := Lookup("codex")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan Detected, 1)
	go func() { done <- detectOne(ctx, h) }()
	waitForPIDs(t, pidfile)
	cancel()
	d := answer(t, done)
	if !strings.Contains(d.Error, "no answer") {
		t.Errorf("Error = %q, want a timeout report", d.Error)
	}
	// The case the action is worth most in: a CLI that hangs on its own version
	// flag has stopped saying anything, so the report has to.
	if !strings.Contains(d.Error, "run it on this machine to see what it waits on") {
		t.Errorf("Error = %q, want the next action", d.Error)
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("grandchild never started: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	// Generous, because only a failing run waits it out: the grandchild dies
	// with its group, and a loaded machine may take a moment to reap it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived the probe", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Ready is the gate on accepting a run, so every way to fail it is a case.
func TestReady(t *testing.T) {
	first := Harness{Kind: FirstClass}
	rec := Harness{Kind: Recognised}
	for _, tc := range []struct {
		name string
		d    Detected
		want bool
	}{
		{"first-class, present, answering", Detected{Harness: first, Present: true}, true},
		{"recognised, present, answering", Detected{Harness: rec, Present: true}, false},
		{"first-class, present, broken", Detected{Harness: first, Present: true, Error: "exit 1"}, false},
		{"first-class, absent", Detected{Harness: first}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.Ready(); got != tc.want {
				t.Errorf("Ready() = %v, want %v", got, tc.want)
			}
		})
	}
}

// DOMAIN.md's **First-class harness** entry names the closed set of kinds; the
// capability document publishes these strings, so the two must not drift.
func TestKindsMatchDomain(t *testing.T) {
	raw, err := os.ReadFile("../../DOMAIN.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(raw), "**First-class harness**")
	if !ok {
		t.Fatal("DOMAIN.md has no **First-class harness** entry")
	}
	_, kinds, ok := strings.Cut(after, "_Kinds_:")
	if !ok {
		t.Fatal("**First-class harness** has no _Kinds_ line")
	}
	line, _, _ := strings.Cut(kinds, "\n")
	var got []string
	for _, v := range strings.Split(line, "|") {
		got = append(got, strings.TrimSpace(v))
	}
	want := []string{string(FirstClass), string(Recognised)}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("DOMAIN.md kinds %v, catalog kinds %v", got, want)
	}
}

// The probe's outcome is the leader's, whoever else holds the pipe. Every
// leader fate against every kind of descendant.
func TestProbeOutcomeFollowsTheLeader(t *testing.T) {
	// Each child records its pid only once it is in place, and the leader waits
	// for that: a leader that exited at once would have its group killed before
	// the child had left it, and the test would not be testing a detached child.
	detached := "perl -MPOSIX -e 'POSIX::setsid(); open(F, \">>\", $ENV{PIDS}); print F \"$$\\n\"; close F; sleep 60' &\n" +
		"while [ ! -s \"$PIDS\" ]; do sleep 0.02; done\n"
	inGroup := "sleep 60 &\necho $! >> \"$PIDS\"\n"
	for _, tc := range []struct {
		name, body  string
		wantVersion string
		wantErr     string
		// waitsItOut says the answer must come from the deadline rather than
		// from the leader, and endsAtCancel says the test brings that deadline
		// itself — once the script has recorded the descendant that holds the
		// pipe, so no machine is too slow to have started it in time.
		waitsItOut   bool
		endsAtCancel bool
	}{
		{name: "exits 0, alone", body: "echo 'codex-cli 1.0'\n",
			wantVersion: "1.0"},
		{name: "exits 0, in-group child holds stdout", body: inGroup + "echo 'codex-cli 1.0'\n",
			wantVersion: "1.0"},
		{name: "exits 0, detached child holds stdout", body: detached + "echo 'codex-cli 1.0'\n",
			wantVersion: "1.0"},
		// The status and the stderr these two used to be asserted on are the
		// leak DEV-60 closed; what the report says now is the command and the
		// next action, and the check below proves the child's words are gone.
		{name: "exits 3", body: "echo oops >&2\nexit 3\n",
			wantErr: "`codex --version` exited with an error"},
		{name: "exits 3, detached child holds stdout", body: detached + "exit 3\n",
			wantErr: "`codex --version` exited with an error"},
		{name: "hangs", body: "sleep 60\n",
			wantErr: "no answer to `codex --version`", waitsItOut: true},
		{name: "hangs, detached child holds stdout", body: detached + "sleep 60\n",
			wantErr: "no answer to `codex --version`", waitsItOut: true, endsAtCancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pids := filepath.Join(dir, "pids")
			script := filepath.Join(dir, "codex")
			if err := os.WriteFile(script, []byte("#!/bin/sh\n"+tc.body), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PIDS", pids)
			t.Setenv("YAD_CODEX_PATH", script)
			t.Cleanup(func() {
				raw, _ := os.ReadFile(pids)
				for _, f := range strings.Fields(string(raw)) {
					if pid, _ := strconv.Atoi(f); pid > 0 {
						syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			})
			timeout := leaderProbeTimeout
			if tc.waitsItOut && !tc.endsAtCancel {
				timeout = hangingProbeTimeout
			}
			old := versionTimeout
			versionTimeout = timeout
			t.Cleanup(func() { versionTimeout = old })

			h, _ := Lookup("codex")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan Detected, 1)
			start := time.Now()
			go func() { done <- detectOne(ctx, h) }()
			if tc.endsAtCancel {
				waitForPIDs(t, pids)
				cancel()
			}
			d := answer(t, done)
			// The deadline is the only clock trusted here, and only for which
			// side of it the answer came from; see the note above the constants.
			if took := time.Since(start); !tc.waitsItOut && took >= timeout {
				t.Errorf("answered after %s: it waited out its deadline instead of following the leader", took)
			} else if tc.waitsItOut && !tc.endsAtCancel && took < timeout {
				t.Errorf("answered in %s, before the %s deadline it had to reach", took, timeout)
			}
			if d.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", d.Version, tc.wantVersion)
			}
			if (tc.wantErr == "") != (d.Error == "") || !strings.Contains(d.Error, tc.wantErr) {
				t.Errorf("Error = %q, want %q", d.Error, tc.wantErr)
			}
			// Whatever the leader's fate, nothing it printed and no wrapped
			// exit status travels: the report goes to every connected hub.
			for _, leak := range []string{"oops", "exit status"} {
				if strings.Contains(d.Error, leak) {
					t.Errorf("Error carries %q: %q", leak, d.Error)
				}
			}
		})
	}
}

// answer is the probe's. One still running this long past its deadline is held
// by a descendant's pipe — the regression leader fate exists to prevent — and
// the test says so rather than hanging until the package times out.
func answer(t *testing.T, done <-chan Detected) Detected {
	t.Helper()
	select {
	case d := <-done:
		return d
	case <-time.After(afterTheDeadline):
		t.Fatalf("the probe was still running %s past its deadline: a descendant is holding it", afterTheDeadline)
		return Detected{}
	}
}

// waitForPIDs returns once the script has recorded a descendant, so a test that
// ends the probe's deadline itself never ends it before there is a descendant
// to hold the pipe. Polling, not a duration: the deadline it gates is the
// test's, so a slow machine only makes this take longer.
func waitForPIDs(t *testing.T, path string) {
	t.Helper()
	for deadline := time.Now().Add(afterTheDeadline); ; time.Sleep(5 * time.Millisecond) {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the fake harness recorded no descendant in %s", afterTheDeadline)
		}
	}
}

// A harness that is installed and will not start is reported broken without a
// word of the machine it is on. supervise.Start wraps a start failure as
// `start <path>: fork/exec <path>: permission denied`, and that path is under
// the owner's home the moment an override names a file there that lost its
// execute bit — the ordinary way this happens (DEV-60). HOME is the test's own,
// so the assertion holds wherever the suite runs; the real one is checked too,
// since a leak of that is the thing being prevented.
func TestStartFailureNamesNoPath(t *testing.T) {
	realHome, _ := os.UserHomeDir()
	for _, tc := range []struct {
		name string
		// body and mode make a binary that cannot be started for the reason
		// this case is about; fromEnv picks which of the two ways detection
		// finds it, because that is what the next action must name. absent
		// writes no file at all, which only an override can reach.
		// want is where the message must send its reader and wantAction is what
		// it must tell them to do there; wantNot is what it must not say.
		body, want, wantAction, wantNot string
		mode                            os.FileMode
		fromEnv, absent                 bool
	}{
		{name: "an override that lost its execute bit", body: "#!/bin/sh\necho 'claude 1.0'\n",
			mode: 0o644, fromEnv: true, want: "YAD_CLAUDE_PATH",
			wantAction: "unset it and let PATH decide"},
		// locate does not stat an override, so this reaches the same branch —
		// which must therefore not claim the harness is installed.
		{name: "an override naming nothing at all", absent: true, fromEnv: true,
			want: "YAD_CLAUDE_PATH", wantAction: "unset it and let PATH decide",
			wantNot: "is installed"},
		// Found on PATH, so it must be executable to be found at all; what it
		// cannot do is exec, because the interpreter it names is not there.
		{name: "on PATH, naming an interpreter that is gone", body: "#!/nonexistent/interpreter\n",
			mode: 0o755, want: "PATH", wantAction: "run `claude --version` on this machine",
			// LookPath has already proved this file executable, so an action
			// saying to check that would be a dead end — and "PATH" alone
			// would not discriminate, being a substring of YAD_CLAUDE_PATH.
			wantNot: "YAD_CLAUDE_PATH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			bin := filepath.Join(home, "bin", "claude")
			if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
				t.Fatal(err)
			}
			if !tc.absent {
				if err := os.WriteFile(bin, []byte(tc.body), tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			if tc.fromEnv {
				t.Setenv("PATH", t.TempDir())
				t.Setenv("YAD_CLAUDE_PATH", bin)
			} else {
				t.Setenv("PATH", filepath.Dir(bin))
				t.Setenv("YAD_CLAUDE_PATH", "")
			}

			h, _ := Lookup("claude")
			d := detectOne(context.Background(), h)
			if !d.Present || d.Error == "" {
				t.Fatalf("claude = %+v, want it present and broken", d)
			}
			if tc.wantNot != "" && strings.Contains(d.Error, tc.wantNot) {
				t.Errorf("Error = %q, want it not to say %q", d.Error, tc.wantNot)
			}
			leaks := []string{home, bin, "fork/exec", "/Users/", "permission denied", "no such file"}
			// The literal above is macOS-only, and this runner deploys on Linux.
			if realHome != "" && realHome != "/" {
				leaks = append(leaks, realHome)
			}
			for _, leak := range leaks {
				if strings.Contains(d.Error, leak) {
					t.Errorf("Error carries %q: %q", leak, d.Error)
				}
			}
			// Where to look is half the next action: the name of an override is
			// safe to print where its value is not.
			if !strings.Contains(d.Error, tc.want) || !strings.Contains(d.Error, tc.wantAction) {
				t.Errorf("Error = %q, want %q and %q", d.Error, tc.want, tc.wantAction)
			}
		})
	}
}

// What a harness prints on its way out is unbounded text nobody vetted: a proxy
// URL with a password in it, a loader error naming the owner's home. None of it
// reaches the capability document, which every connected hub reads.
func TestHarnessOutputIsNeverQuotedInTheReport(t *testing.T) {
	const secret = "https://user:hunter2@proxy.internal/"
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	body := "#!/bin/sh\necho 'fatal: unable to access " + secret + "' >&2\nexit 128\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAD_CLAUDE_PATH", script)

	h, _ := Lookup("claude")
	d := detectOne(context.Background(), h)
	if d.Error == "" {
		t.Fatalf("claude = %+v, want the failure reported", d)
	}
	for _, leak := range []string{secret, "hunter2", "fatal:", "exit status", dir} {
		if strings.Contains(d.Error, leak) {
			t.Errorf("Error carries %q: %q", leak, d.Error)
		}
	}
	if !strings.Contains(d.Error, "run it on this machine") {
		t.Errorf("Error = %q, want the next action", d.Error)
	}
}

// A probe whose deadline fires reports the timeout, never the exit status of
// the child its own kill produced. Whether it timed out is supervise.Run's to
// say, and supervise.TestRunTimedOutIsTheLeadersFate proves it says so every
// time; this is the same pair of children seen through the harness report,
// which is where DEV-69 found them going red.
func TestATimedOutProbeIsNeverReportedAsAnExit(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		// Closing stdout is what makes this deterministic — a wrapper that
		// redirects and then waits on something does it for real.
		{"closes stdout, then hangs", "#!/bin/sh\nexec 1>&-\nsleep 60\n"},
		// Holding it is the ordinary hang, where the same three cases are ready
		// at once and the choice among them is random: green here on an idle
		// machine, red on a loaded one, which is how CI found this.
		{"hangs holding stdout", "#!/bin/sh\nsleep 60\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			script := filepath.Join(dir, "codex")
			if err := os.WriteFile(script, []byte(tc.body), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("YAD_CODEX_PATH", script)
			old := versionTimeout
			// Short, because every run waits it out; all the case needs is that
			// the deadline arrive while the child is still there.
			versionTimeout = 250 * time.Millisecond
			t.Cleanup(func() { versionTimeout = old })

			h, _ := Lookup("codex")
			d := detectOne(context.Background(), h)
			if !strings.Contains(d.Error, "no answer to `codex --version`") {
				t.Errorf("Error = %q, want the timeout rather than the child's fate", d.Error)
			}
		})
	}
}
