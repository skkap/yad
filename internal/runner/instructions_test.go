package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/config"
)

// The owner's standing instructions reach every run after the hub's context,
// under the runner's own heading — a run continuing the session included, and
// a run the hub gave no context of its own (decision 0089).
func TestStandingInstructionsFollowTheHubsContext(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	if err := os.MkdirAll(e.paths.Config, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(e.paths.Config, "INSTRUCTIONS.md")
	if err := os.WriteFile(file, []byte("Never pkill by name.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &fake.Adapter{ID: "claude", Next: func(adapter.Spec) fake.Script {
		return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"}}
	}}
	x := e.executor(h)
	x.Paths = e.paths
	x.Config.Instructions = config.InstructionsConfig{Text: "Docker is the colima context.", File: "INSTRUCTIONS.md"}

	first := testRun("a", "s1")
	first.Brief.Context = "You work for ZUM-128."
	runOne(t, e, l, x, first)
	// Edited between runs, with no restart: the next run reads the file anew.
	if err := os.WriteFile(file, []byte("Never pkill by name; CI shares this box.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := continued("b", "s1")
	second.Brief.Context = "You work for ZUM-128."
	runOne(t, e, l, x, second)
	runOne(t, e, l, x, continued("c", "s1")) // no context from the hub

	want := []string{
		"You work for ZUM-128.\n\n## This machine\n\nDocker is the colima context.\n\nNever pkill by name.",
		"You work for ZUM-128.\n\n## This machine\n\nDocker is the colima context.\n\nNever pkill by name; CI shares this box.",
		"## This machine\n\nDocker is the colima context.\n\nNever pkill by name; CI shares this box.",
	}
	if len(h.Starts) != len(want) {
		t.Fatalf("%d starts, want %d", len(h.Starts), len(want))
	}
	for i, w := range want {
		if got := h.Starts[i].Brief.Context; got != w {
			t.Errorf("run %d's context = %q, want %q", i+1, got, w)
		}
		if h.Starts[i].Brief.Instruction != "do "+[]string{"a", "b", "c"}[i] {
			t.Errorf("run %d's instruction changed: %q", i+1, h.Starts[i].Brief.Instruction)
		}
	}
	if h.Starts[1].NativeSessionID != "native-1" {
		t.Errorf("the second run did not continue the session: %+v", h.Starts[1])
	}
}

// With none configured, a run's brief reaches the harness exactly as the hub
// sent it.
func TestNoStandingInstructionsLeaveTheBriefAlone(t *testing.T) {
	for _, hubContext := range []string{"", "You work for ZUM-128.\n"} {
		e := newEnv(t)
		l := e.loop(t, 1)
		h := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}})
		x := e.executor(h)
		x.Paths = e.paths
		run := testRun("a", "s1")
		run.Brief.Context = hubContext
		runOne(t, e, l, x, run)
		if len(h.Starts) != 1 || h.Starts[0].Brief != run.Brief {
			t.Errorf("brief = %+v, want %+v unchanged", h.Starts, run.Brief)
		}
	}
}

// Instructions the runner cannot give whole refuse the run before anything is
// made for it, saying why: never a cut text, never a run without them.
func TestStandingInstructionsThatCannotBeGivenRefuseTheRun(t *testing.T) {
	for _, tc := range []struct {
		name, file, want string
		body             []byte
	}{
		{name: "over the cap", file: "INSTRUCTIONS.md", body: []byte(strings.Repeat("x", config.MaxInstructions+1)), want: "instructions.file is 16385 bytes, over the 16384"},
		{name: "a file that is not there", file: "GONE.md", want: "instructions.file could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			if tc.body != nil {
				if err := os.MkdirAll(e.paths.Config, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(e.paths.Config, tc.file), tc.body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			h := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}})
			x := e.executor(h)
			x.Paths = e.paths
			x.Config.Instructions = config.InstructionsConfig{File: tc.file}
			runOne(t, e, l, x, testRun("a", "s1"))
			if len(h.Starts) != 0 {
				t.Fatalf("the harness was started %d times", len(h.Starts))
			}
			res := hubResult(t, e, "a")
			if res.State != v1.RunFailed || res.Error == nil || res.Error.Class != ClassPrepare || !strings.Contains(res.Error.Message, tc.want) {
				t.Errorf("result = %+v, want prepare_failed saying %q", res, tc.want)
			}
		})
	}
}
