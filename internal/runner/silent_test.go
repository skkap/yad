package runner

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/claude"
	"github.com/skkap/yad/internal/config"
)

// silentClaudeSecret is a grant's value, which the fake claude quotes back on
// stderr as a harness refusing a credential might.
const silentClaudeSecret = "sk-test-grant-value-0123456789"

// fakeClaudeRun is the test binary as claude for a whole run, re-executed
// with RUNNER_TEST_CLAUDE set to the mode: the real Claude adapter drives it
// as it would drive claude, through supervise, pipes and process group.
// "silent" says a few words on stderr and then nothing at all, ignoring its
// input, as the native Claude of DEV-170 did after loading its settings;
// SIGTERM ends it. "stderr" writes seven lines to stderr, quoting the grant
// it was given, and exits 1 before writing a frame.
func fakeClaudeRun(mode string) int {
	switch mode {
	case "silent":
		fmt.Fprintln(os.Stderr, "starting")
		fmt.Fprintln(os.Stderr, "loaded settings")
		time.Sleep(time.Hour)
		return 0
	default:
		for i := 1; i <= 7; i++ {
			if i == 6 {
				fmt.Fprintf(os.Stderr, "line %d: the credential %s was refused\n", i, os.Getenv("DEPLOY_TOKEN"))
				continue
			}
			fmt.Fprintf(os.Stderr, "line %d\n", i)
		}
		return 1
	}
}

// claudeExecutor is an executor whose harness is the real Claude adapter
// starting this test binary as claude in mode.
func claudeExecutor(t *testing.T, e *env, mode string) *Exec {
	t.Helper()
	t.Setenv("RUNNER_TEST_CLAUDE", mode)
	// A race-enabled child otherwise sleeps a second at exit.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	x := e.executor(claude.Adapter{})
	x.Binary = func(string) (string, bool) { return os.Args[0], true }
	return x
}

// A Claude that hangs before its first frame ends the run harness_silent at
// the owner's first-output timeout, with what it said on stderr and the next
// action, rather than sitting in running for the inactivity timeout's half
// hour (DEV-170).
func TestASilentClaudeIsStoppedAtItsFirstOutputTimeout(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x := claudeExecutor(t, e, "silent")
	// Long enough for a race-enabled child to have written its stderr.
	x.Config.Supervise.FirstOutput = config.Duration{Duration: 2 * time.Second}
	start := time.Now()
	claimAndRun(t, l, x)
	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunTimedOut || res.Error == nil || res.Error.Class != ClassHarnessSilent {
		t.Fatalf("result = %+v (error %+v), %v", res, res.Error, ok)
	}
	for _, s := range []string{
		"no event in the first 2s after it started",
		"its stderr ended: starting | loaded settings",
		"see whether it answers: `claude -p hello`",
		"account add claude",
	} {
		if !strings.Contains(res.Error.Message, s) {
			t.Errorf("message %q lacks %q", res.Error.Message, s)
		}
	}
	if took := time.Since(start); took > time.Minute {
		t.Errorf("the run took %s to stop", took)
	}
}

// The account a silent run is pointed to only for a harness that can have
// one: for OpenCode, which has none, the command would be refused as printed.
func TestASilentRunIsSentToAnAccountOnlyWhereOneCanBeAdded(t *testing.T) {
	x := &Exec{}
	for _, tc := range []struct {
		harness    string
		hasAccount bool
		want       bool
	}{
		{"claude", false, true},
		{"codex", false, true},
		{"claude", true, false},
		{"opencode", false, false},
	} {
		got := x.stoppedDetail(ClassHarnessSilent, v1.Run{Harness: tc.harness}, adapter.Outcome{}, tc.hasAccount)
		if strings.Contains(got, "account add") != tc.want {
			t.Errorf("%s, account %v: %q", tc.harness, tc.hasAccount, got)
		}
	}
}

// A Claude that writes to stderr and exits without a frame fails
// harness_exited with its last lines — not only the last one — and none of
// the values yad put in its environment.
func TestAClaudeThatDiesCarriesItsLastStderrLines(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	run := testRun("a", "s1")
	run.Grants = []v1.Grant{{Name: "DEPLOY_TOKEN", Value: silentClaudeSecret, As: v1.GrantEnv}}
	e.enqueue(t, run)
	claimAndRun(t, l, claudeExecutor(t, e, "stderr"))
	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunFailed || res.Error == nil || res.Error.Class != adapter.ClassHarnessExited {
		t.Fatalf("result = %+v (error %+v), %v", res, res.Error, ok)
	}
	msg := res.Error.Message
	want := "line 3 | line 4 | line 5 | line 6: the credential " + adapter.Redacted + " was refused | line 7"
	if !strings.Contains(msg, want) {
		t.Errorf("message %q lacks %q", msg, want)
	}
	if strings.Contains(msg, "line 2") || strings.Contains(msg, silentClaudeSecret) {
		t.Errorf("message %q carries more than the last %d lines, or the grant", msg, adapter.StderrLines)
	}
}
