package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/acp/acptest"
)

// The core against agents that are not OpenCode: hand-written conversations
// (testdata/agent) for what OpenCode's recordings cannot show. OpenCode's
// own recorded runs are internal/adapter/opencode's tests.

func TestMain(m *testing.M) {
	if os.Getenv(acptest.EnvFixture) != "" {
		acptest.Main()
		return
	}
	os.Exit(m.Run())
}

var testAgent = Agent{ID: "agent", Name: "Agent", Args: []string{"acp"}, LoginCheck: []string{"login", "status"}}

func play(t *testing.T, name string, edit func(*adapter.Spec)) ([]v1.Event, adapter.Outcome) {
	t.Helper()
	fixture, err := filepath.Abs(filepath.Join("testdata", "agent", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	spec := adapter.Spec{
		RunID: "r1", Binary: os.Args[0], Workdir: t.TempDir(), Model: "a",
		Env:   []string{acptest.EnvFixture + "=" + fixture, "GORACE=atexit_sleep_ms=0"},
		Brief: v1.Brief{Instruction: "do it"},
	}
	if edit != nil {
		edit(&spec)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tr, err := Start(ctx, testAgent, spec)
	if err != nil {
		t.Fatal(err)
	}
	var events []v1.Event
	for e := range tr.Events() {
		events = append(events, e)
	}
	return events, tr.Wait()
}

// An agent that answers with another major version of the protocol is not
// driven: v2 answers a prompt when it accepts it, not when the turn ends.
func TestAnotherProtocolVersionIsNotDriven(t *testing.T) {
	_, o := play(t, "v2", nil)
	if o.State != v1.RunFailed || o.Error.Class != adapter.ClassHarness || !strings.Contains(o.Error.Message, "ACP version 2") {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
}

// What the agent says it cannot do is refused in words before anything is
// asked of it.
func TestAResumeOrForkTheAgentCannotDoIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*adapter.Spec)
		says string
	}{
		{"resume", func(s *adapter.Spec) { s.NativeSessionID = "s0" }, "does not resume"},
		{"fork", func(s *adapter.Spec) { s.ForkFrom = "s0" }, "does not fork"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, o := play(t, "bare", tc.edit)
			if o.State != v1.RunFailed || !strings.Contains(o.Error.Message, tc.says) {
				t.Fatalf("outcome %s %+v", o.State, o.Error)
			}
		})
	}
}

// Only the run's own session's updates are its events; a request for a
// capability the client never offered is refused; a turn stopped at the
// model's output limit is a failure that says so; and a usage of zeros is no
// usage.
func TestTheRunsOwnSessionAndItsStop(t *testing.T) {
	events, o := play(t, "max-tokens", nil)
	if o.State != v1.RunFailed || !strings.Contains(o.Error.Message, "output limit") {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
	var said string
	for _, e := range events {
		if e.Kind == v1.EventText {
			said += e.Text
		}
		if e.Kind == v1.EventUsage {
			t.Errorf("a usage event for a turn that used nothing: %+v", e.Usage)
		}
	}
	if said != "hi" {
		t.Errorf("text %q, want only the run's session's", said)
	}
	if o.Usage != nil {
		t.Errorf("usage %+v", o.Usage)
	}
}

// A permission_mode yad does not know refuses the run before anything
// starts.
func TestAnUnknownPermissionModeIsRefused(t *testing.T) {
	spec := adapter.Spec{Binary: os.Args[0], Settings: map[string]string{"permission_mode": "yolo"}}
	if _, err := Start(context.Background(), testAgent, spec); err == nil || !strings.Contains(err.Error(), "[harness.agent]") {
		t.Errorf("start: %v", err)
	}
}

// A refused resume is session_not_found only when no page of the agent's
// sessions names it: one on a later page was refused for another reason,
// and a hub told the conversation is gone would abandon one that is not.
func TestARefusedResumeReadsEveryPage(t *testing.T) {
	for _, tc := range []struct {
		fixture, class string
	}{
		{"resume-refused-second-page", adapter.ClassHarness},
		{"resume-refused-gone", adapter.ClassSessionNotFound},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			_, o := play(t, tc.fixture, func(s *adapter.Spec) { s.NativeSessionID = "s0" })
			if o.State != v1.RunFailed || o.Error.Class != tc.class {
				t.Fatalf("outcome %s %+v", o.State, o.Error)
			}
		})
	}
}
