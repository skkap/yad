//go:build unix

package supervise

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runChild(ctx context.Context, t *testing.T, mode string) Capture {
	t.Helper()
	c, err := Run(ctx, Spec{
		Path:    os.Args[0],
		Env:     []string{"SUPERVISE_TEST_CHILD=" + mode, "GORACE=atexit_sleep_ms=0"},
		KeepEnv: []string{"SUPERVISE_TEST_CHILD"},
	}, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TimedOut is a fact about the leader, not a guess (DEV-70). Before, Run
// decided it in the select that waits for EOF, the leader's exit or ctx. A
// child that closes stdout and then hangs settles that select on EOF before the
// deadline exists, so its timeout came back as an ordinary kill every time —
// the reliable reproduction, and the half of DEV-69's subtests that went red.
// A child that hangs holding stdout leaves all three ready once the deadline's
// kill lands and Go picks at random; that only loses when Run is descheduled at
// the wrong moment, rare on an idle machine and not rare in CI, so it runs many
// times rather than once.
func TestRunTimedOutIsTheLeadersFate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		timeout time.Duration // zero: no deadline at all
		ended   bool          // ctx already ended when Run is called
		runs    int
		want    bool
	}{
		{name: "hangs holding stdout", mode: "sleep", timeout: 50 * time.Millisecond, runs: 30, want: true},
		{name: "closes stdout, then hangs", mode: "close-stdout-sleep", timeout: 50 * time.Millisecond, runs: 10, want: true},
		{name: "ctx ended before it started", mode: "sleep", ended: true, runs: 10, want: true},
		// Not the deadline's doing: the leader ended itself, by exit status or
		// by a signal of its own, and no ctx ended.
		{name: "exits 0", mode: "echo", runs: 1, want: false},
		{name: "exits non-zero", mode: "stderr", runs: 1, want: false},
		{name: "killed by a signal ctx did not send", mode: "self-kill", runs: 1, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.runs {
				timeout := time.Hour
				if tc.timeout > 0 {
					timeout = tc.timeout
				}
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				if tc.ended {
					cancel()
				}
				c := runChild(ctx, t, tc.mode)
				cancel()
				if c.TimedOut != tc.want {
					t.Fatalf("run %d: TimedOut = %v, want %v (Err = %v)", i, c.TimedOut, tc.want, c.Err)
				}
				// The kill is the leader's exit error; a timeout without one
				// would mean the leader was never ended.
				if tc.want && c.Err == nil {
					t.Fatalf("run %d: TimedOut with a nil Err", i)
				}
			}
		})
	}
}

// The rule at the reap, over real exit statuses. The orderings that matter —
// a leader that crashes on its own in the instant the deadline fires — cannot
// be arranged through Run, since ctx's kill follows at once; here ctx's end is
// simply given, and only the leader's fate decides.
func TestEndedByContext(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		ctxErr error
		want   bool
	}{
		{"self-kill", context.DeadlineExceeded, true}, // indistinguishable from ours, and so ours
		{"self-kill", context.Canceled, true},
		{"self-kill", nil, false},
		{"self-term", context.DeadlineExceeded, false}, // its own crash, not our kill
		{"echo", context.DeadlineExceeded, false},      // it answered, however close
		{"stderr", context.DeadlineExceeded, false},    // it failed, by itself
	} {
		t.Run(fmt.Sprintf("%s/%v", tc.mode, tc.ctxErr), func(t *testing.T) {
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), "SUPERVISE_TEST_CHILD="+tc.mode, "GORACE=atexit_sleep_ms=0")
			_ = cmd.Run()
			// Else a self-term that merely exited would pass for the wrong reason.
			if strings.HasPrefix(tc.mode, "self-") && cmd.ProcessState.Exited() {
				t.Fatalf("%s exited with %v, want death by signal", tc.mode, cmd.ProcessState)
			}
			if got := endedByContext(tc.ctxErr, cmd.ProcessState); got != tc.want {
				t.Errorf("endedByContext(%v, %v) = %v, want %v", tc.ctxErr, cmd.ProcessState, got, tc.want)
			}
		})
	}
	if !endedByContext(context.DeadlineExceeded, nil) {
		t.Error("a Wait with no status after the deadline must still be the deadline's")
	}
}

// An answer is kept when ctx ends after it: the leader exited on its own, and
// what it printed is the result.
func TestRunKeepsAnAnswerFromBeforeTheDeadline(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := runChild(ctx, t, "echo")
	cancel()
	if c.TimedOut || c.Err != nil || string(c.Stdout) != "one\ntwo\n" {
		t.Errorf("Capture = %+v, want the answer and no timeout", c)
	}
}

// A descendant that left the group holding the pipe must neither stall Run nor
// turn a leader that answered and exited into a timeout.
func TestRunReturnsPastADetachedDescendant(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	c := runChild(ctx, t, "detached")
	if pid, err := strconv.Atoi(strings.TrimSpace(string(c.Stdout))); err == nil {
		// It left our group, so nothing of ours will kill it.
		syscall.Kill(pid, syscall.SIGKILL)
	} else {
		t.Errorf("Stdout = %q, want the descendant's pid", c.Stdout)
	}
	if c.TimedOut || c.Err != nil {
		t.Errorf("TimedOut = %v, Err = %v, want an ordinary exit", c.TimedOut, c.Err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Run took %s, want it bounded by the drain, not the descendant", d)
	}
}
