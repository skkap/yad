//go:build unix

package supervise

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as every misbehaving child the supervisor has to
// handle: re-executed with SUPERVISE_TEST_CHILD set, it becomes that child instead of
// running tests. No shell scripts, no real harness, the same on macOS and Linux.
func TestMain(m *testing.M) {
	if mode := os.Getenv("SUPERVISE_TEST_CHILD"); mode != "" {
		child(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func child(mode string) {
	switch mode {
	case "echo":
		fmt.Println("one")
		fmt.Println("two")
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
	case "stderr":
		os.Stderr.WriteString(strings.Repeat("x", 10_000) + "THE END")
		os.Exit(3)
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM, syscall.SIGINT)
		fmt.Println("ready")
		time.Sleep(time.Hour)
	case "polite":
		fmt.Println("ready")
		time.Sleep(time.Hour) // default SIGTERM disposition: dies
	case "grandchild":
		// Leave a descendant behind in our process group, holding stdout, and
		// exit cleanly — the npm-outlives-its-parent case.
		gc := exec.Command(os.Args[0])
		gc.Env = append(os.Environ(), "SUPERVISE_TEST_CHILD=sleep")
		gc.Stdout = os.Stdout
		if err := gc.Start(); err != nil {
			os.Exit(9)
		}
		fmt.Println(gc.Process.Pid)
	case "sleep":
		time.Sleep(time.Hour)
	}
}

func spawn(t *testing.T, mode string, extra ...string) *Process {
	t.Helper()
	p, err := Start(context.Background(), Spec{
		Path: os.Args[0],
		// A race-enabled child otherwise sleeps a second at exit to flush its
		// report, which is test time, not supervisor behaviour.
		Env: append([]string{"SUPERVISE_TEST_CHILD=" + mode, "GORACE=atexit_sleep_ms=0"}, extra...),
		// Proves KeepEnv works; the name would survive the scrub anyway.
		KeepEnv: []string{"SUPERVISE_TEST_CHILD"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Stop(Ladder{}) })
	return p
}

func TestOutputIsNotLostToWait(t *testing.T) {
	p := spawn(t, "echo")
	out, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("exit: %v", err)
	}
	if string(out) != "one\ntwo\n" {
		t.Errorf("stdout = %q", out)
	}
}

func TestEnvironmentIsScrubbed(t *testing.T) {
	t.Setenv("CLAUDECODE", "1")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "cli")
	t.Setenv("ANTHROPIC_API_KEY", "sk-secret")
	t.Setenv("YAD_DATA_DIR", "/x")
	t.Setenv("KEEP_ME", "yes")
	p := spawn(t, "env", "ADDED=1")
	out, _ := io.ReadAll(p.Stdout())
	p.Wait()
	env := string(out)
	for _, gone := range []string{"CLAUDECODE=", "CLAUDE_CODE_ENTRYPOINT=", "ANTHROPIC_API_KEY=", "YAD_DATA_DIR="} {
		if strings.Contains(env, gone) {
			t.Errorf("child inherited %s", gone)
		}
	}
	for _, kept := range []string{"KEEP_ME=yes", "ADDED=1"} {
		if !strings.Contains(env, kept) {
			t.Errorf("child is missing %s", kept)
		}
	}
}

func TestStderrTailIsBounded(t *testing.T) {
	p := spawn(t, "stderr")
	io.ReadAll(p.Stdout())
	err := p.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("exit = %v", err)
	}
	// The copy goroutine may finish just after Wait; give it a moment.
	deadline := time.Now().Add(time.Second)
	for !strings.HasSuffix(p.Stderr(), "THE END") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := p.Stderr(); len(got) != StderrTail || !strings.HasSuffix(got, "THE END") {
		t.Errorf("stderr tail is %d bytes ending %q", len(got), got[max(0, len(got)-10):])
	}
}

func waitReady(t *testing.T, p *Process) *bufio.Reader {
	t.Helper()
	r := bufio.NewReader(p.Stdout())
	line, err := r.ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("child not ready: %q, %v", line, err)
	}
	return r
}

func TestStopLadder(t *testing.T) {
	fast := Ladder{InterruptGrace: 50 * time.Millisecond, TermGrace: 200 * time.Millisecond}
	for _, tc := range []struct {
		mode      string
		interrupt bool
		want      Step
	}{
		{"polite", false, StepTerminated},
		{"ignore-term", false, StepKilled},
		{"ignore-term", true, StepKilled},
	} {
		t.Run(fmt.Sprintf("%s/interrupt=%v", tc.mode, tc.interrupt), func(t *testing.T) {
			p := spawn(t, tc.mode)
			waitReady(t, p)
			l := fast
			called := false
			if tc.interrupt {
				l.Interrupt = func() error { called = true; return nil }
			}
			if got := p.Stop(l); got != tc.want {
				t.Errorf("Stop = %s, want %s", got, tc.want)
			}
			if tc.interrupt && !called {
				t.Error("the interrupt step was skipped")
			}
		})
	}
}

func TestStopAfterExit(t *testing.T) {
	p := spawn(t, "echo")
	io.ReadAll(p.Stdout())
	p.Wait()
	if got := p.Stop(DefaultLadder); got != StepExited {
		t.Errorf("Stop on an exited process = %s", got)
	}
}

// A child that exits cleanly while its own child still runs must not leave that
// grandchild behind: it would hold our pipe open forever and keep whatever
// locks it took.
func TestDescendantsDieWithLeader(t *testing.T) {
	p := spawn(t, "grandchild")
	r := bufio.NewReader(p.Stdout())
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	gc, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatalf("grandchild pid %q", line)
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("leader exit: %v", err)
	}
	// EOF proves every holder of the pipe is gone.
	done := make(chan struct{})
	go func() { io.Copy(io.Discard, r); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stdout never reached EOF — the grandchild survived")
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(gc, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still alive", gc)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestContextCancelKillsGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, err := Start(ctx, Spec{Path: os.Args[0], Env: []string{"SUPERVISE_TEST_CHILD=ignore-term"}, KeepEnv: []string{"SUPERVISE_TEST_CHILD"}})
	if err != nil {
		t.Fatal(err)
	}
	waitReady(t, p)
	cancel()
	select {
	case <-p.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not kill the child")
	}
}

func TestScrubKeep(t *testing.T) {
	got := Scrub([]string{"ANTHROPIC_API_KEY=k", "PATH=/bin", "YAD_X=1"}, []string{"ANTHROPIC_API_KEY"})
	if strings.Join(got, ",") != "ANTHROPIC_API_KEY=k,PATH=/bin" {
		t.Errorf("Scrub = %v", got)
	}
}
