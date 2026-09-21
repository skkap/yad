package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

var _ adapter.Adapter = Adapter{}

// fixtures is the Claude version the replayed streams were recorded from.
const fixtures = "testdata/claude-2.1.276"

func TestMain(m *testing.M) {
	if os.Getenv("CLAUDE_TEST_FIXTURE") != "" {
		fakeClaude()
		return
	}
	os.Exit(m.Run())
}

// harness is one fake claude run: what it plays, how it ends, what it saw.
type harness struct {
	fixture string            // path to the stream it plays
	env     map[string]string // extra CLAUDE_TEST_* settings
	log     string
}

// fixture is absolute: the fake claude runs in the turn's workdir.
func fixture(name string) string {
	p, err := filepath.Abs(filepath.Join(fixtures, name+".jsonl"))
	if err != nil {
		panic(err)
	}
	return p
}

func (h *harness) spec(t *testing.T) adapter.Spec {
	t.Helper()
	h.log = filepath.Join(t.TempDir(), "log.jsonl")
	env := []string{
		"CLAUDE_TEST_FIXTURE=" + h.fixture,
		"CLAUDE_TEST_LOG=" + h.log,
		// A race-enabled child otherwise sleeps a second at exit.
		"GORACE=atexit_sleep_ms=0",
	}
	for k, v := range h.env {
		env = append(env, k+"="+v)
	}
	return adapter.Spec{
		RunID:    "r1",
		Binary:   os.Args[0],
		Workdir:  t.TempDir(),
		Model:    "haiku",
		Env:      env,
		Brief:    v1.Brief{Instruction: "do the thing"},
		Settings: map[string]string{"permission_mode": "bypassPermissions"},
	}
}

// seen is what the fake claude logged: its argv, the context file, stdin.
type seen struct {
	argv    []string
	context string
	stdin   []map[string]any
}

func (h *harness) seen(t *testing.T) seen {
	t.Helper()
	f, err := os.Open(h.log)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var s seen
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		var e struct {
			Argv    []string `json:"argv"`
			Context *string  `json:"context"`
			Stdin   *string  `json:"stdin"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		switch {
		case e.Argv != nil:
			s.argv = e.Argv
		case e.Context != nil:
			s.context = *e.Context
		case e.Stdin != nil:
			var m map[string]any
			json.Unmarshal([]byte(*e.Stdin), &m)
			s.stdin = append(s.stdin, m)
		}
	}
	return s
}

func (s seen) frames(typ string) []map[string]any {
	var out []map[string]any
	for _, m := range s.stdin {
		if m["type"] == typ {
			out = append(out, m)
		}
	}
	return out
}

func (s seen) flag(name string) (string, bool) {
	i := slices.Index(s.argv, name)
	if i < 0 {
		return "", false
	}
	if i+1 < len(s.argv) {
		return s.argv[i+1], true
	}
	return "", true
}

// drive starts a turn, collects every event, lets act steer or interrupt, and
// returns the events and outcome — failing the test rather than hanging.
func drive(t *testing.T, ctx context.Context, spec adapter.Spec, act func(adapter.Turn, v1.Event) bool) ([]v1.Event, adapter.Outcome, adapter.Turn) {
	t.Helper()
	tr, err := Adapter{}.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		events []v1.Event
		out    adapter.Outcome
	}
	done := make(chan result, 1)
	go func() {
		var evs []v1.Event
		acted := act == nil
		for e := range tr.Events() {
			evs = append(evs, e)
			if !acted {
				acted = act(tr, e)
			}
		}
		done <- result{evs, tr.Wait()}
	}()
	select {
	case r := <-done:
		return r.events, r.out, tr
	case <-time.After(20 * time.Second):
		t.Fatal("the turn did not end")
		return nil, adapter.Outcome{}, nil
	}
}

func text(evs []v1.Event) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Kind == v1.EventText {
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

func kinds(evs []v1.Event, k v1.EventKind) []v1.Event {
	var out []v1.Event
	for _, e := range evs {
		if e.Kind == k {
			out = append(out, e)
		}
	}
	return out
}

func hasStatus(evs []v1.Event, s string) bool {
	return slices.ContainsFunc(evs, func(e v1.Event) bool { return e.Kind == v1.EventStatus && e.Status == s })
}

func errorClasses(evs []v1.Event) []string {
	var out []string
	for _, e := range kinds(evs, v1.EventError) {
		out = append(out, e.Error.Class)
	}
	return out
}

func interruptOn(k v1.EventKind) func(adapter.Turn, v1.Event) bool {
	return func(tr adapter.Turn, e v1.Event) bool {
		if e.Kind != k {
			return false
		}
		tr.Interrupt()
		return true
	}
}

func steerOn(k v1.EventKind, msg string) func(adapter.Turn, v1.Event) bool {
	return func(tr adapter.Turn, e v1.Event) bool {
		if e.Kind != k {
			return false
		}
		if err := tr.Steer(msg); err != nil {
			panic(err)
		}
		return true
	}
}

// Every recorded stream, replayed through the adapter and a real process.
func TestRecordedStreams(t *testing.T) {
	cases := []struct {
		name      string
		resume    bool
		act       func(adapter.Turn, v1.Event) bool
		state     v1.RunState
		class     string
		final     string
		textHas   string
		steers    int
		interrupt bool
		// diesBeforeReading marks a harness that exits without ever reading
		// its instruction, so how many user frames it logged is undefined:
		// the fake reads and logs stdin from a goroutine, and os.Exit does
		// not wait for it. Asserting a count here fails 299 times in 300
		// under GOMAXPROCS=1, and intermittently on a runner with more cores.
		diesBeforeReading bool
	}{
		{name: "plain", state: v1.RunSucceeded, final: "pong", textHas: "pong"},
		{name: "tool", state: v1.RunSucceeded, final: "hello from a small file", textHas: "hello from a small file"},
		{name: "error", state: v1.RunFailed, class: adapter.ClassHarness, textHas: "issue with the selected model"},
		{name: "prompt-too-long", state: v1.RunFailed, class: adapter.ClassPromptTooLong},
		{name: "resume-missing", resume: true, state: v1.RunFailed, class: adapter.ClassSessionNotFound, diesBeforeReading: true},
		{name: "interrupt", act: interruptOn(v1.EventText), state: v1.RunCancelled, interrupt: true},
		{name: "steer-tool", act: steerOn(v1.EventToolCall, "end with STEERED"), state: v1.RunSucceeded, textHas: "STEERED", steers: 1},
		{name: "steer-followup", act: steerOn(v1.EventText, "reply: steered"), state: v1.RunSucceeded, final: "steered", steers: 1},
		{name: "permission-denied", state: v1.RunSucceeded},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &harness{fixture: fixture(c.name)}
			spec := h.spec(t)
			if c.resume {
				spec.NativeSessionID = "6f1c2b1e-9d3a-4c55-8e7f-0a1b2c3d4e5f"
			}
			evs, out, tr := drive(t, context.Background(), spec, c.act)
			if out.State != c.state {
				t.Fatalf("state %s, want %s (error %+v)", out.State, c.state, out.Error)
			}
			if c.class != "" {
				if out.Error == nil || out.Error.Class != c.class {
					t.Errorf("error %+v, want class %s", out.Error, c.class)
				}
				if !slices.Contains(errorClasses(evs), c.class) {
					t.Errorf("no %s error event in %v", c.class, errorClasses(evs))
				}
			} else if out.Error != nil {
				t.Errorf("unexpected error %+v", out.Error)
			}
			if c.final != "" && strings.TrimSpace(out.FinalText) != c.final {
				t.Errorf("final text %q, want %q", out.FinalText, c.final)
			}
			if c.textHas != "" && !strings.Contains(text(evs), c.textHas) {
				t.Errorf("text %q lacks %q", text(evs), c.textHas)
			}
			if tr.NativeSessionID() == "" || out.NativeSessionID != tr.NativeSessionID() {
				t.Errorf("native session %q, outcome %q", tr.NativeSessionID(), out.NativeSessionID)
			}
			if c.resume && out.NativeSessionID != spec.NativeSessionID {
				t.Errorf("resumed %s, outcome names %s", spec.NativeSessionID, out.NativeSessionID)
			}
			s := h.seen(t)
			if got := len(s.frames("user")); !c.diesBeforeReading && got != 1+c.steers {
				t.Errorf("%d user frames on stdin, want %d", got, 1+c.steers)
			}
			if got := len(s.frames("control_request")); (got > 0) != c.interrupt {
				t.Errorf("%d control requests; interrupt expected: %v", got, c.interrupt)
			}
			if c.interrupt && out.FinalText != "" {
				t.Errorf("an interrupted run reported final text %q", out.FinalText)
			}
			// Text arrives in coalesced chunks, never one event per delta.
			if n := len(kinds(evs, v1.EventText)); n > 20 {
				t.Errorf("%d text events: deltas are not being coalesced", n)
			}
		})
	}
}

// A resume that takes, recorded from claude 2.1.278: the second turn of a
// session is started with --resume and the session's own id, never
// --session-id, and answers from the first turn's context ("plum") in the
// session it was told to continue.
func TestRecordedResume(t *testing.T) {
	p, err := filepath.Abs("testdata/claude-2.1.278/resume.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{fixture: p}
	spec := h.spec(t)
	spec.NativeSessionID = "6f1c2b1e-9d3a-4c55-8e7f-0a1b2c3d4e5f"
	evs, out, tr := drive(t, context.Background(), spec, nil)
	if out.State != v1.RunSucceeded || strings.TrimSpace(out.FinalText) != "plum" {
		t.Fatalf("outcome %s %q (error %+v)", out.State, out.FinalText, out.Error)
	}
	if slices.Contains(errorClasses(evs), adapter.ClassSessionMismatch) {
		t.Error("a resume that took was reported as a mismatch")
	}
	if tr.NativeSessionID() != spec.NativeSessionID || out.NativeSessionID != spec.NativeSessionID {
		t.Errorf("native session %q, outcome %q; resumed %q", tr.NativeSessionID(), out.NativeSessionID, spec.NativeSessionID)
	}
	s := h.seen(t)
	if id, ok := s.flag("--resume"); !ok || id != spec.NativeSessionID {
		t.Errorf("--resume %q, %v", id, ok)
	}
	if _, ok := s.flag("--session-id"); ok {
		t.Error("a resume also passed --session-id, which would start a new conversation")
	}
}

func TestPlainTurn(t *testing.T) {
	h := &harness{fixture: fixture("plain")}
	spec := h.spec(t)
	spec.Brief.Context = "You are terse."
	evs, out, _ := drive(t, context.Background(), spec, nil)

	if !hasStatus(evs, "started") || !hasStatus(evs, "thinking") {
		t.Errorf("status events missing: %+v", kinds(evs, v1.EventStatus))
	}
	// The complete assistant frame repeats the streamed text; it must not be
	// emitted twice.
	if got := text(evs); got != "pong" {
		t.Errorf("text %q", got)
	}
	u, ok := out.Usage["claude-haiku-4-5-20251001"]
	if len(out.Usage) != 1 || !ok || u.Input == 0 || u.Output == 0 || u.CacheRead == 0 || u.CostUSD == nil || *u.CostUSD <= 0 {
		t.Errorf("usage %+v", out.Usage)
	}
	ue := kinds(evs, v1.EventUsage)
	if len(ue) != 1 || ue[0].Usage.Model != "claude-haiku-4-5-20251001" || ue[0].Usage.Output != u.Output {
		t.Errorf("usage events %+v", ue)
	}
	for _, e := range evs {
		if e.At.IsZero() || e.Seq != 0 {
			t.Errorf("event %+v: At must be set and Seq left to the runner", e)
		}
	}

	s := h.seen(t)
	for flag, want := range map[string]string{
		"--input-format":     "stream-json",
		"--output-format":    "stream-json",
		"--permission-mode":  "bypassPermissions",
		"--model":            "haiku",
		"--disallowed-tools": "AskUserQuestion",
	} {
		if got, _ := s.flag(flag); got != want {
			t.Errorf("%s %q, want %q (argv %q)", flag, got, want, s.argv)
		}
	}
	for _, flag := range []string{"-p", "--verbose", "--include-partial-messages", "--replay-user-messages"} {
		if _, ok := s.flag(flag); !ok {
			t.Errorf("argv lacks %s: %q", flag, s.argv)
		}
	}
	if id, _ := s.flag("--session-id"); id != out.NativeSessionID || !uuidPattern.MatchString(id) {
		t.Errorf("--session-id %q, outcome %q", id, out.NativeSessionID)
	}
	if _, ok := s.flag("--resume"); ok {
		t.Error("a new session was started with --resume")
	}
	if s.context != "You are terse." {
		t.Errorf("context file held %q", s.context)
	}
	file, _ := s.flag("--append-system-prompt-file")
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("context file %s outlived the turn: %v", file, err)
	}
	users := s.frames("user")
	if len(users) != 1 {
		t.Fatalf("user frames %v", users)
	}
	msg, _ := users[0]["message"].(map[string]any)
	if msg["role"] != "user" || msg["content"] != "do the thing" {
		t.Errorf("instruction frame %v", users[0])
	}
}

// The context file is gone by the time the turn says it is over (DEV-92). A
// runner stopping after its last turn exits once that turn is waited for, and
// anything still to be cleaned up then is left behind in the shared temp
// directory with the brief's context in it. The removal is made slow so that
// cleanup running after Wait fails here every time: TestPlainTurn checks the
// same thing at full speed, and caught it only on a loaded machine.
func TestTheContextFileIsGoneWhenTheTurnIs(t *testing.T) {
	old := remove
	remove = func(path string) error {
		time.Sleep(200 * time.Millisecond)
		return old(path)
	}
	t.Cleanup(func() { remove = old })
	h := &harness{fixture: fixture("plain")}
	spec := h.spec(t)
	spec.Brief.Context = "You are terse."
	drive(t, context.Background(), spec, nil)
	file, ok := h.seen(t).flag("--append-system-prompt-file")
	if !ok {
		t.Fatal("no context file was passed, so this test proved nothing")
	}
	if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("context file %s outlived the turn: %v", file, err)
	}
}

func TestToolEvents(t *testing.T) {
	h := &harness{fixture: fixture("tool")}
	evs, _, _ := drive(t, context.Background(), h.spec(t), nil)
	calls, results := kinds(evs, v1.EventToolCall), kinds(evs, v1.EventToolResult)
	if len(calls) != 1 || len(results) != 1 {
		t.Fatalf("calls %+v results %+v", calls, results)
	}
	c, r := calls[0].Tool, results[0].Tool
	if c.Name != "Read" || !strings.Contains(c.Input, "note.txt") || c.ID == "" {
		t.Errorf("call %+v", c)
	}
	if r.ID != c.ID || !strings.Contains(r.Output, "hello from a small file") {
		t.Errorf("result %+v for call %s", r, c.ID)
	}
	// Order is the stream's: the call, its result, then the answer.
	ci := slices.IndexFunc(evs, func(e v1.Event) bool { return e.Kind == v1.EventToolCall })
	ri := slices.IndexFunc(evs, func(e v1.Event) bool { return e.Kind == v1.EventToolResult })
	ti := slices.IndexFunc(evs, func(e v1.Event) bool { return e.Kind == v1.EventText })
	if ci >= ri || ri >= ti {
		t.Errorf("order call %d, result %d, text %d", ci, ri, ti)
	}
}

func TestPermissionDeniedIsReported(t *testing.T) {
	h := &harness{fixture: fixture("permission-denied")}
	evs, _, _ := drive(t, context.Background(), h.spec(t), nil)
	if !hasStatus(evs, "permission denied: Bash") {
		t.Errorf("no permission status in %+v", kinds(evs, v1.EventStatus))
	}
}

// The derived streams: shapes a real run produces but a recording cannot be
// asked for — a usage limit, a crash, a corrupt line. Each is built from a
// recorded stream so it stays in step with the version the fixtures are from.

func derive(t *testing.T, from string, edit func([]string) []string) string {
	t.Helper()
	raw, err := os.ReadFile(fixture(from))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	lines = edit(lines)
	path := filepath.Join(t.TempDir(), from+"-derived.jsonl")
	// A last line ending in cutMark is written without its newline, as a
	// process killed mid-write leaves it.
	body := strings.Join(lines, "\n") + "\n"
	body = strings.TrimSuffix(body, cutMark+"\n")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

const cutMark = "\x00"

func indexOf(lines []string, typ string) int {
	return slices.IndexFunc(lines, func(l string) bool {
		return strings.HasPrefix(l, "{") && strings.Contains(l, `"type":"`+typ+`"`) && isType(l, typ)
	})
}

func isType(line, typ string) bool {
	var f struct {
		Type string `json:"type"`
	}
	return json.Unmarshal([]byte(line), &f) == nil && f.Type == typ
}

// editResult rewrites the result frame's fields.
func editResult(t *testing.T, lines []string, set map[string]any) []string {
	i := indexOf(lines, "result")
	if i < 0 {
		t.Fatal("fixture has no result")
	}
	var m map[string]any
	json.Unmarshal([]byte(lines[i]), &m)
	for k, v := range set {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	lines[i] = string(b)
	return lines
}

func TestUsageLimit(t *testing.T) {
	reset := time.Date(2026, 9, 19, 3, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		event  bool   // a rate_limit_event says rejected, with a reset
		status any    // api_error_status
		result string // result text
		window string
		reset  time.Time
	}{
		{"rate limit event", true, 429, "You've hit your limit · resets 3am", "five_hour", reset},
		{"429 alone", false, 429, "You've hit your limit", "", time.Time{}},
		{"legacy text", false, nil, "Claude AI usage limit reached|1789786800", "", reset},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := derive(t, "error", func(lines []string) []string {
				lines = editResult(t, lines, map[string]any{"api_error_status": c.status, "result": c.result, "terminal_reason": "api_error"})
				if c.event {
					ev := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789786800,"rateLimitType":"five_hour"},"session_id":"x"}`
					i := indexOf(lines, "result")
					lines = slices.Insert(lines, i, ev)
				}
				return lines
			})
			h := &harness{fixture: path}
			evs, out, _ := drive(t, context.Background(), h.spec(t), nil)
			if out.State != v1.RunFailed || out.Error == nil || out.Error.Class != adapter.ClassUsageLimit {
				t.Fatalf("outcome %+v", out)
			}
			if out.Limit == nil || out.Limit.Window != c.window || !out.Limit.ResetAt.Equal(c.reset) {
				t.Errorf("limit %+v, want %s until %s", out.Limit, c.window, c.reset)
			}
			if c.event && !hasStatus(evs, "usage limit reached") {
				t.Error("no status event for the limit")
			}
		})
	}
}

// A limit Claude recovered from (overage, a retry) is not the run's outcome.
func TestRejectedLimitThenSuccessSucceeds(t *testing.T) {
	path := derive(t, "plain", func(lines []string) []string {
		ev := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1789786800,"rateLimitType":"five_hour"}}`
		return slices.Insert(lines, indexOf(lines, "result"), ev)
	})
	h := &harness{fixture: path}
	_, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded || out.Limit != nil {
		t.Errorf("outcome %+v", out)
	}
}

func TestSessionMismatch(t *testing.T) {
	// The recorded stream keeps its own session id: Claude ran a session other
	// than the one it was told to.
	h := &harness{fixture: fixture("plain"), env: map[string]string{"CLAUDE_TEST_KEEP_SESSION": "1"}}
	spec := h.spec(t)
	spec.NativeSessionID = "6f1c2b1e-9d3a-4c55-8e7f-0a1b2c3d4e5f"
	evs, out, _ := drive(t, context.Background(), spec, nil)
	if out.State != v1.RunFailed || out.Error == nil || out.Error.Class != adapter.ClassSessionMismatch {
		t.Fatalf("outcome %+v", out)
	}
	if out.NativeSessionID != spec.NativeSessionID {
		t.Errorf("outcome names session %s; the runner's pointer must stay %s", out.NativeSessionID, spec.NativeSessionID)
	}
	if !slices.Contains(errorClasses(evs), adapter.ClassSessionMismatch) {
		t.Error("no mismatch error event")
	}
	if len(h.seen(t).frames("control_request")) != 1 {
		t.Error("the mismatched turn was not interrupted")
	}
}

func TestBrokenStreams(t *testing.T) {
	cases := []struct {
		name  string
		from  string
		edit  func([]string) []string
		mode  string
		exit  string
		state v1.RunState
		class string // the outcome's
		event string // an error event that must appear
		msg   string // in the outcome's message
	}{
		{
			name: "garbled line mid-stream", from: "plain",
			edit: func(l []string) []string {
				return slices.Insert(l, 4, `{"type":"assistant","message":{`, "not json at all")
			},
			state: v1.RunSucceeded, event: adapter.ClassStream,
		},
		{
			name: "truncated before the result", from: "plain",
			edit: func(l []string) []string {
				i := indexOf(l, "result")
				return append(l[:i:i], l[i][:len(l[i])/2]+cutMark)
			},
			state: v1.RunFailed, class: adapter.ClassHarnessExited, event: adapter.ClassStream,
		},
		{
			name: "exit 0 without a result", from: "plain",
			edit:  func(l []string) []string { return l[:indexOf(l, "result")] },
			state: v1.RunFailed, class: adapter.ClassHarnessExited,
		},
		{
			name: "died mid-tool", from: "tool",
			edit: func(l []string) []string {
				i := slices.IndexFunc(l, func(s string) bool { return strings.Contains(s, `"tool_use"`) && isType(s, "assistant") })
				return l[:i+1]
			},
			mode: "died", state: v1.RunFailed, class: adapter.ClassHarnessExited, msg: "something went badly wrong",
		},
		{
			name: "result with exit 1", from: "plain", exit: "1",
			edit:  func(l []string) []string { return l },
			state: v1.RunSucceeded,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := map[string]string{"CLAUDE_TEST_MODE": c.mode, "CLAUDE_TEST_EXIT": c.exit}
			h := &harness{fixture: derive(t, c.from, c.edit), env: env}
			evs, out, _ := drive(t, context.Background(), h.spec(t), nil)
			if out.State != c.state {
				t.Fatalf("state %s, want %s (%+v)", out.State, c.state, out.Error)
			}
			if c.class != "" && (out.Error == nil || out.Error.Class != c.class) {
				t.Errorf("error %+v, want %s", out.Error, c.class)
			}
			if c.event != "" && !slices.Contains(errorClasses(evs), c.event) {
				t.Errorf("error events %v lack %s", errorClasses(evs), c.event)
			}
			if c.msg != "" && (out.Error == nil || !strings.Contains(out.Error.Message, c.msg)) {
				t.Errorf("message %+v lacks %q", out.Error, c.msg)
			}
			if out.State == v1.RunFailed && out.Error != nil && !strings.Contains(out.Error.Message, "claude -p") && c.class == adapter.ClassHarnessExited {
				t.Errorf("message %q carries no next action", out.Error.Message)
			}
		})
	}
}

// A line over the cap is skipped and reported; the run goes on to its result.
func TestOversizedLineIsSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("writes 32 MiB through a pipe")
	}
	path := derive(t, "plain", func(l []string) []string {
		big := `{"type":"user","message":{"content":"` + strings.Repeat("x", adapter.MaxLine) + `"}}`
		return slices.Insert(l, 4, big)
	})
	h := &harness{fixture: path}
	evs, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded || out.FinalText != "pong" {
		t.Fatalf("outcome %+v", out)
	}
	if !slices.Contains(errorClasses(evs), adapter.ClassStream) {
		t.Error("the skipped line was not reported")
	}
}

// Claude asks permission over the control protocol when a tool needs it and
// something could answer. Nothing can; the adapter denies at once so the turn
// is not left waiting.
func TestPermissionRequestIsDenied(t *testing.T) {
	path := derive(t, "plain", func(l []string) []string {
		req := `{"type":"control_request","request_id":"perm-7","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"rm -rf /"}}}`
		return slices.Insert(l, 3, req)
	})
	h := &harness{fixture: path}
	_, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded {
		t.Fatalf("outcome %+v", out)
	}
	resp := h.seen(t).frames("control_response")
	if len(resp) != 1 {
		t.Fatalf("control responses %v", resp)
	}
	r, _ := resp[0]["response"].(map[string]any)
	inner, _ := r["response"].(map[string]any)
	if r["request_id"] != "perm-7" || inner["behavior"] != "deny" {
		t.Errorf("response %v", resp[0])
	}
}

func TestSteerAndInterruptAfterTheEnd(t *testing.T) {
	h := &harness{fixture: fixture("plain")}
	_, _, tr := drive(t, context.Background(), h.spec(t), nil)
	if err := tr.Steer("more"); err == nil || !strings.Contains(err.Error(), "new run") {
		t.Errorf("steer after the end: %v", err)
	}
	if err := tr.Interrupt(); err != nil {
		t.Errorf("interrupt after the end: %v", err)
	}
}

// Interrupting twice sends one request: cancel paths race.
func TestInterruptTwice(t *testing.T) {
	h := &harness{fixture: fixture("interrupt")}
	_, out, _ := drive(t, context.Background(), h.spec(t), func(tr adapter.Turn, e v1.Event) bool {
		if e.Kind != v1.EventText {
			return false
		}
		tr.Interrupt()
		tr.Interrupt()
		return true
	})
	if out.State != v1.RunCancelled {
		t.Errorf("state %s", out.State)
	}
	if n := len(h.seen(t).frames("control_request")); n != 1 {
		t.Errorf("%d interrupt requests", n)
	}
}

// The cancel ladder's second rung: a claude deaf to the interrupt is ended
// by SIGTERM to its group, and the run is cancelled, not a crash.
func TestTerminateEndsADeafClaude(t *testing.T) {
	h := &harness{fixture: fixture("interrupt"), env: map[string]string{"CLAUDE_TEST_MODE": "deaf"}}
	_, out, _ := drive(t, context.Background(), h.spec(t), func(tr adapter.Turn, e v1.Event) bool {
		if e.Kind != v1.EventText {
			return false
		}
		tr.Interrupt()
		go func() {
			// Terminated once the fake has read the interrupt, not a fixed
			// while after sending it: on a loaded machine the SIGTERM used
			// to land first, and the log held no interrupt (DEV-92).
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if b, _ := os.ReadFile(h.log); strings.Contains(string(b), "control_request") {
					break
				}
			}
			tr.Terminate()
		}()
		return true
	})
	if out.State != v1.RunCancelled || out.Error != nil {
		t.Errorf("state %s (%+v)", out.State, out.Error)
	}
	if n := len(h.seen(t).frames("control_request")); n != 1 {
		t.Errorf("%d interrupt requests", n)
	}
}

func TestContextCancelEndsTheTurn(t *testing.T) {
	// No result ever comes: the fake blocks waiting for an interrupt that is
	// never sent, as a wedged claude would.
	h := &harness{fixture: fixture("interrupt")}
	ctx, cancel := context.WithCancel(context.Background())
	_, out, _ := drive(t, ctx, h.spec(t), func(_ adapter.Turn, e v1.Event) bool {
		if e.Kind != v1.EventText {
			return false
		}
		cancel()
		return true
	})
	if out.State != v1.RunCancelled {
		t.Errorf("state %s (%+v)", out.State, out.Error)
	}
}

// A runner that has stopped reading events must still get its Outcome, or
// every such run leaks a claude process and a goroutine.
func TestWaitWithoutReadingEvents(t *testing.T) {
	h := &harness{fixture: fixture("tool")}
	tr, err := Adapter{}.Start(context.Background(), h.spec(t))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan adapter.Outcome, 1)
	go func() { done <- tr.Wait() }()
	select {
	case out := <-done:
		if out.State != v1.RunSucceeded {
			t.Errorf("state %s", out.State)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Wait hung on events nobody read")
	}
}

// A claude that will not exit after its last result is stopped: the result
// already decided the run, and its process must not outlive it.
func TestLingeringClaudeIsStopped(t *testing.T) {
	old := exitGrace
	exitGrace, termGrace = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { exitGrace, termGrace = old, 5*time.Second })
	h := &harness{fixture: fixture("plain"), env: map[string]string{"CLAUDE_TEST_MODE": "linger"}}
	_, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded || out.FinalText != "pong" {
		t.Errorf("outcome %+v", out)
	}
}

// A claude whose output ends with no result but which never exits is stopped
// too: nothing it could still do would reach the run.
func TestMuteClaudeIsStopped(t *testing.T) {
	old := exitGrace
	exitGrace, termGrace = 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { exitGrace, termGrace = old, 5*time.Second })
	path := derive(t, "plain", func(l []string) []string { return l[:indexOf(l, "result")] })
	h := &harness{fixture: path, env: map[string]string{"CLAUDE_TEST_MODE": "mute"}}
	_, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunFailed || out.Error == nil || out.Error.Class != adapter.ClassHarnessExited {
		t.Errorf("outcome %+v", out)
	}
}

// Claude took a steer, answered the turn before it, and died: the earlier
// result is not the run's answer.
func TestDeathAfterATakenSteerFails(t *testing.T) {
	path := derive(t, "steer-followup", func(l []string) []string {
		var replays []int
		for i, s := range l {
			if strings.Contains(s, `"isReplay":true`) {
				replays = append(replays, i)
			}
		}
		return l[:replays[1]+1]
	})
	h := &harness{fixture: path, env: map[string]string{"CLAUDE_TEST_MODE": "died"}}
	_, out, _ := drive(t, context.Background(), h.spec(t), steerOn(v1.EventText, "reply: steered"))
	if out.State != v1.RunFailed || out.Error == nil || out.Error.Class != adapter.ClassHarnessExited || out.FinalText != "" {
		t.Fatalf("outcome %+v", out)
	}
	if !strings.Contains(out.Error.Message, "steer") {
		t.Errorf("message %q does not say a steer went unanswered", out.Error.Message)
	}
}

// A result followed by a steer Claude took is not final, and it never decides
// the run — not when Claude dies, not on an interrupt, not on a cancel.
func TestNonFinalResultNeverSucceeds(t *testing.T) {
	// Claude answered the first turn, took the steer and started thinking
	// about it; the stream ends there.
	path := derive(t, "steer-followup", func(l []string) []string {
		replays := 0
		for i, s := range l {
			if strings.Contains(s, `"isReplay":true`) {
				replays++
			}
			if replays == 2 && strings.Contains(s, `"content_block_start"`) {
				return l[:i+1]
			}
		}
		t.Fatal("fixture has no second turn")
		return nil
	})
	cases := []struct {
		name  string
		mode  string
		stop  func(tr adapter.Turn, cancel context.CancelFunc) // on the second turn's first sign of life
		state v1.RunState
	}{
		{"interrupted, then died unanswered", "die-on-interrupt", func(tr adapter.Turn, _ context.CancelFunc) { tr.Interrupt() }, v1.RunCancelled},
		{"cancelled", "linger", func(_ adapter.Turn, cancel context.CancelFunc) { cancel() }, v1.RunCancelled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			h := &harness{fixture: path, env: map[string]string{"CLAUDE_TEST_MODE": c.mode}}
			steered, stopped := false, false
			_, out, _ := drive(t, ctx, h.spec(t), func(tr adapter.Turn, e v1.Event) bool {
				switch {
				case !steered && e.Kind == v1.EventText:
					steered = true
					if err := tr.Steer("reply: steered"); err != nil {
						t.Error(err)
					}
				case steered && !stopped && hasStatus([]v1.Event{e}, "thinking"):
					stopped = true
					c.stop(tr, cancel)
				}
				return stopped
			})
			if !stopped {
				t.Fatal("the second turn never started")
			}
			if out.State != c.state || out.FinalText != "" {
				t.Errorf("outcome %+v, want %s with no final text", out, c.state)
			}
		})
	}
}

func TestStartRefuses(t *testing.T) {
	good := adapter.Spec{Binary: os.Args[0], Workdir: t.TempDir()}
	cases := []struct {
		name string
		edit func(*adapter.Spec)
		want string
	}{
		{"no binary", func(s *adapter.Spec) { s.Binary = "" }, "yad doctor"},
		{"a session id that is not a uuid", func(s *adapter.Spec) { s.NativeSessionID = "--dangerously-skip-permissions" }, "not a UUID"},
		{"a model that is a flag", func(s *adapter.Spec) { s.Model = "--dangerously-skip-permissions" }, "not a model"},
		{"a permission mode that is a flag", func(s *adapter.Spec) {
			s.Settings = map[string]string{"permission_mode": "--dangerously-skip-permissions"}
		}, "config.toml"},
		// The exec error names the binary under the owner's home; the run's
		// error goes to a hub, so the path travels only as the cause (DEV-67).
		{"a binary that will not exec", func(s *adapter.Spec) { s.Binary = "/Users/someone/bin/claude" }, "would not start"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := good
			c.edit(&s)
			_, err := Adapter{}.Start(context.Background(), s)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err %v, want %q", err, c.want)
			}
			if err != nil && strings.Contains(err.Error(), "/Users/someone") {
				t.Errorf("err %v names the binary's path", err)
			}
		})
	}
}

// The permission mode is the owner's; unset, it is unattended auto-approve
// (0015), never Claude's interactive default, which denies every tool that
// needs a prompt.
func TestPermissionMode(t *testing.T) {
	cases := []struct {
		name     string
		settings map[string]string
		want     string
	}{
		{"unset means bypass", nil, "bypassPermissions"},
		{"empty means bypass", map[string]string{"permission_mode": ""}, "bypassPermissions"},
		{"the owner's value wins", map[string]string{"permission_mode": "acceptEdits"}, "acceptEdits"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := &harness{fixture: fixture("plain")}
			spec := h.spec(t)
			spec.Settings = c.settings
			spec.Model = ""
			drive(t, context.Background(), spec, nil)
			s := h.seen(t)
			if got, _ := s.flag("--permission-mode"); got != c.want {
				t.Errorf("--permission-mode %q, want %q (argv %q)", got, c.want, s.argv)
			}
			for _, flag := range []string{"--model", "--append-system-prompt-file"} {
				if _, ok := s.flag(flag); ok {
					t.Errorf("argv has %s with nothing configured: %q", flag, s.argv)
				}
			}
		})
	}
}

// A grant named IS_SANDBOX never reaches Claude: it would switch off Claude's
// own refusal to bypass permissions as root.
func TestRunCannotDeclareTheSandbox(t *testing.T) {
	got := runEnv([]string{"A=1", "IS_SANDBOX=1", "IS_SANDBOX_X=2", "B=2", "IS_SANDBOX"})
	want := []string{"A=1", "IS_SANDBOX_X=2", "B=2"}
	if !slices.Equal(got, want) {
		t.Errorf("runEnv = %q, want %q", got, want)
	}
}

// Claude exits at once if bypassPermissions is used as root outside a
// declared sandbox. The adapter refuses first, with the way out, and never
// declares the sandbox itself.
func TestBypassAsRoot(t *testing.T) {
	cases := []struct {
		name    string
		euid    int
		mode    string
		env     []string // the run's
		osEnv   string   // the runner's IS_SANDBOX
		refused bool
	}{
		{"root, default mode", 0, "", nil, "", true},
		{"root, bypass set by the owner", 0, "bypassPermissions", nil, "", true},
		{"root, sandbox declared by the runner's environment", 0, "", nil, "1", false},
		// A run's environment carries the hub's grants: it cannot declare the
		// sandbox, and cannot take the owner's declaration away either.
		{"root, sandbox declared only by the run", 0, "", []string{"IS_SANDBOX=1"}, "", true},
		{"root, the run tries to undeclare it", 0, "", []string{"IS_SANDBOX=0"}, "1", false},
		{"root, a narrower mode", 0, "acceptEdits", nil, "", false},
		{"an ordinary user", 501, "", nil, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := geteuid
			geteuid = func() int { return c.euid }
			t.Cleanup(func() { geteuid = old })
			t.Setenv("IS_SANDBOX", c.osEnv)
			spec := adapter.Spec{Workdir: t.TempDir(), Env: c.env, Settings: map[string]string{"permission_mode": c.mode}}
			args, err := argv(spec, newUUID(), "")
			if c.refused {
				if err == nil || !strings.Contains(err.Error(), "IS_SANDBOX=1") || !strings.Contains(err.Error(), "ordinary user") {
					t.Errorf("err %v, want a refusal naming the ways out", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, "IS_SANDBOX") }) {
				t.Errorf("argv declares the sandbox: %q", args)
			}
		})
	}
}

func TestNewUUID(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := newUUID()
		if !uuidPattern.MatchString(id) || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) {
			t.Fatalf("%q is not a v4 uuid", id)
		}
		if seen[id] {
			t.Fatalf("%q repeated", id)
		}
		seen[id] = true
	}
}
