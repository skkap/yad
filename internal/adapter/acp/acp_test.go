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
	"github.com/skkap/yad/internal/adapter/jsonrpc"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
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
	o := tr.Wait()
	if o.State != v1.RunCancelled {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
	if o.LeftRunning != nil {
		t.Errorf("an interrupted turn was measured: %v", *o.LeftRunning)
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

// A permission asked for after the turn is interrupted is answered cancelled,
// whatever the owner's setting: ACP v1 asks it of a client that has sent
// session/cancel, and nothing more of an ending turn is approved.
func TestAPermissionAfterAnInterruptIsCancelled(t *testing.T) {
	fixture, _ := filepath.Abs(filepath.Join("testdata", "agent", "permission-after-cancel.jsonl"))
	log := filepath.Join(t.TempDir(), "log.jsonl")
	spec := adapter.Spec{
		RunID: "r1", Binary: os.Args[0], Workdir: t.TempDir(), Model: "a",
		Env:   []string{acptest.EnvFixture + "=" + fixture, acptest.EnvLog + "=" + log, "GORACE=atexit_sleep_ms=0"},
		Brief: v1.Brief{Instruction: "do it"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tr, err := Start(ctx, testAgent, spec)
	if err != nil {
		t.Fatal(err)
	}
	for e := range tr.Events() {
		if e.Kind == v1.EventText {
			tr.Interrupt()
		}
	}
	if o := tr.Wait(); o.State != v1.RunCancelled {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `\"outcome\":{\"outcome\":\"cancelled\"}`) || strings.Contains(string(b), `\"optionId\":\"once\"`) {
		t.Errorf("the permission after the cancel was not answered cancelled:\n%s", b)
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

// An error answered with a null id — JSON-RPC's answer to a request whose id
// the agent could not read — ties to no request by its id. With one request
// outstanding it is that request's: the run fails at once with the agent's
// words, not on the inactivity watchdog a prompt with no deadline of its own
// would wait for.
func TestANullIDErrorFailsTheOneRequestOutstanding(t *testing.T) {
	for _, tc := range []struct {
		fixture, method, says string
	}{
		{"prompt-null-id", "session/prompt", "Invalid request: prompt[0] is not a content block"},
		{"new-null-id", "session/new", "Parse error: unexpected token"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			start := time.Now()
			events, o := play(t, tc.fixture, nil)
			if took := time.Since(start); took > 10*time.Second {
				t.Errorf("the run took %s to end", took)
			}
			if o.State != v1.RunFailed || o.Error.Class != adapter.ClassHarness ||
				!strings.Contains(o.Error.Message, tc.says) || !strings.Contains(o.Error.Message, tc.method) {
				t.Fatalf("outcome %s %+v", o.State, o.Error)
			}
			checkNext(t, o.Error.Message, testAgent.ID)
			if !logged(events, tc.says) {
				t.Errorf("the agent's answer was not logged as a stream event: %+v", events)
			}
		})
	}
}

// With several requests outstanding the answer names none of them, and none
// is guessed: the turn fails with the agent's error. With none outstanding it
// is the agent's to explain, logged, and the run goes on. Either way its
// words are masked of the run's environment and file grants, as its stderr
// is: an agent that could not read a request may quote it back.
func TestANullIDErrorWithSeveralOrNoneOutstanding(t *testing.T) {
	const password, grant = "pw-0123456789abcdef", "grant-file-value-42"
	for _, tc := range []struct {
		name   string
		wait   map[int64]string
		failed bool
	}{
		{"several", map[int64]string{3: "session/prompt", 4: "session/set_config_option"}, true},
		{"none", map[int64]string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []v1.Event
			tu := &turn{
				agent: testAgent, wait: tc.wait, finished: make(chan struct{}),
				in:   newInput(nopWriter{}, testAgent.Name),
				env:  []string{"OPENCODE_SERVER_PASSWORD=" + password},
				spec: adapter.Spec{Secrets: []string{grant}},
			}
			defer tu.in.Close()
			tu.tr = newTranslator(func(e v1.Event) { events = append(events, e) }, testAgent, tu.spec)
			tu.handle(jsonrpc.Line{Msg: &jsonrpc.Message{
				ID:    json.RawMessage("null"),
				Error: &jsonrpc.RPCError{Code: jsonrpc.CodeInternal, Message: "something broke reading " + password + " and " + grant},
			}})
			if !logged(events, "something broke reading "+adapter.Redacted+" and "+adapter.Redacted) {
				t.Errorf("not logged, masked, as a stream event: %+v", events)
			}
			var said string
			for _, e := range events {
				if e.Error != nil {
					said += e.Error.Message + "\n"
				}
			}
			if tu.tr.fault != nil {
				said += tu.tr.fault.Message
			}
			if strings.Contains(said, password) || strings.Contains(said, grant) {
				t.Errorf("a secret went out unmasked: %s", said)
			}
			if tu.over != tc.failed {
				t.Errorf("turn over %v, want %v", tu.over, tc.failed)
			}
			if !tc.failed {
				if tu.tr.fault != nil || len(tu.wait) != 0 {
					t.Errorf("fault %+v, wait %v", tu.tr.fault, tu.wait)
				}
				return
			}
			if tu.tr.fault == nil || tu.tr.fault.Class != adapter.ClassHarness || !strings.Contains(tu.tr.fault.Message, "something broke") {
				t.Fatalf("fault %+v", tu.tr.fault)
			}
			checkNext(t, tu.tr.fault.Message, testAgent.ID)
			for _, method := range tc.wait {
				if strings.Contains(tu.tr.fault.Message, method) {
					t.Errorf("the error guesses %s: %q", method, tu.tr.fault.Message)
				}
			}
		})
	}
}

// An answer the core cannot use — a session opened with no id, a request
// refused where no retry applies, a fork answered with the session it was
// to leave alone — fails the run naming the agent's version command, which
// runs as printed.
func TestAnAnswerTheCoreCannotUseNamesTheAgentsVersion(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		edit    func(*adapter.Spec)
		class   string
		says    string
		doctor  bool // the message names yad doctor after the version
	}{
		{"new-no-session", nil, adapter.ClassHarness, "answered session/new without a session", false},
		{"new-refused", nil, adapter.ClassHarness, "refused session/new: Invalid params: mcpServers", true},
		{"fork-same-session", func(s *adapter.Spec) { s.ForkFrom = "s0" }, adapter.ClassSessionMismatch, "answered session/fork with session s0", false},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			_, o := play(t, tc.fixture, tc.edit)
			if o.State != v1.RunFailed || o.Error.Class != tc.class || !strings.Contains(o.Error.Message, tc.says) {
				t.Fatalf("outcome %s %+v", o.State, o.Error)
			}
			if tc.doctor {
				checkNext(t, o.Error.Message, testAgent.ID)
				return
			}
			parts := strings.Split(o.Error.Message, "`")
			if len(parts) != 3 {
				t.Fatalf("want one command set apart in backticks in %q", o.Error.Message)
			}
			shellwordtest.Check(t, parts[1], testAgent.ID, "--version")
		})
	}
}

// checkNext runs the two commands a null-id failure's next action names
// through a real sh: the agent's version, then yad doctor.
func checkNext(t *testing.T, msg, id string) {
	t.Helper()
	parts := strings.Split(msg, "`")
	if len(parts) != 5 {
		t.Fatalf("want two commands set apart in backticks in %q", msg)
	}
	shellwordtest.Check(t, parts[1], id, "--version")
	shellwordtest.Check(t, parts[3], "yad", "doctor")
}

// logged reports whether a stream event quotes says.
func logged(events []v1.Event, says string) bool {
	for _, e := range events {
		if e.Kind == v1.EventError && e.Error.Class == adapter.ClassStream && strings.Contains(e.Error.Message, says) {
			return true
		}
	}
	return false
}

type nopWriter struct{}

func (nopWriter) Write(b []byte) (int, error) { return len(b), nil }
func (nopWriter) Close() error                { return nil }
