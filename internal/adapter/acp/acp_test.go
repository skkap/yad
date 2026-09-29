package acp

import (
	"context"
	"encoding/json"
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
	log := filepath.Join(t.TempDir(), "log.jsonl")
	lastLog = log
	spec := adapter.Spec{
		RunID: "r1", Binary: os.Args[0], Workdir: t.TempDir(), Model: "a",
		Env:   []string{acptest.EnvFixture + "=" + fixture, acptest.EnvLog + "=" + log, "GORACE=atexit_sleep_ms=0"},
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

// lastLog is the fake's log of the last play: what the adapter wrote.
var lastLog string

// sent is every request the adapter sent with this method, in order: its
// params.
func sent(t *testing.T, method string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(lastLog)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e struct {
			Stdin *string `json:"stdin"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.Stdin == nil {
			continue
		}
		var m struct {
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal([]byte(*e.Stdin), &m) == nil && m.Method == method {
			out = append(out, m.Params)
		}
	}
	return out
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

// An interrupt never reaches the agent ahead of the prompt it is for: a
// cancel with no turn to end is dropped, and the turn after it would run to
// the end. Interrupted at the moment the turn is marked prompted — before its
// prompt is queued — the run still ends cancelled, and the cancel reaches
// the agent after the prompt.
func TestAnInterruptNeverOvertakesThePrompt(t *testing.T) {
	fixture, _ := filepath.Abs(filepath.Join("testdata", "agent", "turn.jsonl"))
	log := filepath.Join(t.TempDir(), "log.jsonl")
	spec := adapter.Spec{
		RunID: "r1", Binary: os.Args[0], Workdir: t.TempDir(), Model: "a",
		Env: []string{acptest.EnvFixture + "=" + fixture, acptest.EnvLog + "=" + log,
			acptest.EnvGate + "=" + filepath.Join(t.TempDir(), "never"), "GORACE=atexit_sleep_ms=0"},
		Brief: v1.Brief{Instruction: "do it"},
	}
	var tr adapter.Turn
	ready := make(chan struct{})
	promptHook = func() {
		<-ready
		// Given the chance to run before the prompt is queued: it blocks
		// until it is, or this wait runs out and it is sent after anyway.
		done := make(chan struct{})
		go func() {
			tr.Interrupt()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Cleanup(func() { promptHook = nil })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var err error
	tr, err = Start(ctx, testAgent, spec)
	if err != nil {
		t.Fatal(err)
	}
	close(ready)
	for range tr.Events() {
	}
	if o := tr.Wait(); o.State != v1.RunCancelled {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	p, c := strings.Index(string(b), "session/prompt"), strings.Index(string(b), "session/cancel")
	if p < 0 || c < p {
		t.Errorf("prompt at %d, cancel at %d in what the agent read", p, c)
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
			// Every session, not the workdir's — a fork's source is in
			// another — and each page after the first by its cursor.
			lists := sent(t, "session/list")
			if len(lists) != 2 || len(lists[0]) != 0 || lists[1]["cursor"] != "c1" || lists[1]["cwd"] != nil {
				t.Errorf("session/list sent %v", lists)
			}
		})
	}
}
