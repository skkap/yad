package codex

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
	"github.com/skkap/yad/internal/adapter/codex/codextest"
)

var _ adapter.Adapter = Adapter{}

// fixtures is the Codex version the replayed conversations were recorded from.
const fixtures = "testdata/codex-0.147.0"

func TestMain(m *testing.M) {
	// This package's children are all the fake, whatever they are named.
	if os.Getenv("CODEX_TEST_FIXTURE") != "" || os.Getenv("CODEX_TEST_SCHEMA") != "" {
		codextest.Main()
		return
	}
	os.Exit(m.Run())
}

// harness is one fake codex run: what it plays, how it ends, what it saw.
type harness struct {
	fixture string
	env     map[string]string
	log     string
}

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
		"CODEX_TEST_FIXTURE=" + h.fixture,
		"CODEX_TEST_LOG=" + h.log,
		// A race-enabled child otherwise sleeps a second at exit.
		"GORACE=atexit_sleep_ms=0",
	}
	for k, v := range h.env {
		env = append(env, k+"="+v)
	}
	return adapter.Spec{
		RunID:   "r1",
		Binary:  os.Args[0],
		Workdir: t.TempDir(),
		Model:   "gpt-5.6-luna",
		Env:     env,
		Brief:   v1.Brief{Instruction: "do the thing"},
	}
}

// seen is what the fake codex logged: its argv and what the adapter wrote.
type seen struct {
	argv  []string
	stdin []map[string]any
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
			Argv  []string `json:"argv"`
			Stdin *string  `json:"stdin"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		switch {
		case e.Argv != nil:
			s.argv = e.Argv
		case e.Stdin != nil:
			var m map[string]any
			json.Unmarshal([]byte(*e.Stdin), &m)
			s.stdin = append(s.stdin, m)
		}
	}
	return s
}

// sent is every request or notification the adapter sent with this method.
func (s seen) sent(method string) []map[string]any {
	var out []map[string]any
	for _, m := range s.stdin {
		if m["method"] == method {
			out = append(out, m)
		}
	}
	return out
}

func (s seen) params(t *testing.T, method string) map[string]any {
	t.Helper()
	ms := s.sent(method)
	if len(ms) != 1 {
		t.Fatalf("the adapter sent %s %d times, want once: %v", method, len(ms), s.stdin)
	}
	p, _ := ms[0]["params"].(map[string]any)
	return p
}

// drive starts a turn, collects every event, lets act steer or interrupt, and
// returns the events and outcome — failing the test rather than hanging.
func drive(t *testing.T, ctx context.Context, spec adapter.Spec, act func(adapter.Turn, v1.Event) bool) ([]v1.Event, adapter.Outcome, adapter.Turn) {
	t.Helper()
	tr, err := Adapter{}.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	return collect(t, tr, act)
}

func collect(t *testing.T, tr adapter.Turn, act func(adapter.Turn, v1.Event) bool) ([]v1.Event, adapter.Outcome, adapter.Turn) {
	t.Helper()
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

func statuses(evs []v1.Event) []string {
	var out []string
	for _, e := range kinds(evs, v1.EventStatus) {
		out = append(out, e.Status)
	}
	return out
}

func errorClasses(evs []v1.Event) []string {
	var out []string
	for _, e := range kinds(evs, v1.EventError) {
		out = append(out, e.Error.Class)
	}
	return out
}

// threadIn is the thread id a recorded conversation ran in.
func threadIn(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(fixture(name))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		var m struct {
			Result struct {
				Thread thread `json:"thread"`
			} `json:"result"`
		}
		if json.Unmarshal([]byte(line), &m) == nil && m.Result.Thread.ID != "" {
			return m.Result.Thread.ID
		}
	}
	t.Fatalf("%s has no thread", name)
	return ""
}

// A new session: the app-server is started as the adapter documents, the
// thread is started with the owner's defaults and the brief's context, the
// answer streams as text, and usage and the thread id come back.
func TestPlainRun(t *testing.T) {
	h := &harness{fixture: fixture("plain")}
	spec := h.spec(t)
	spec.Brief.Context = "You are terse."
	evs, out, tr := drive(t, context.Background(), spec, nil)

	if out.State != v1.RunSucceeded || out.FinalText != "pong" || out.Error != nil {
		t.Fatalf("outcome = %+v", out)
	}
	if got := text(evs); got != "pong" {
		t.Errorf("text = %q", got)
	}
	thread := threadIn(t, "plain")
	if out.NativeSessionID != thread || tr.NativeSessionID() != thread {
		t.Errorf("native session = %q / %q, want %q", out.NativeSessionID, tr.NativeSessionID(), thread)
	}
	if st := statuses(evs); len(st) == 0 || st[0] != "started" {
		t.Errorf("statuses = %v, want started first", st)
	}
	u, ok := out.Usage["gpt-5.6-luna"]
	if !ok || u.Output != 5 || u.CacheRead != 9984 || u.Input != 11863-9984 || u.CostUSD != nil {
		t.Errorf("usage = %+v", out.Usage)
	}
	if len(kinds(evs, v1.EventUsage)) != 1 {
		t.Errorf("usage events = %v", kinds(evs, v1.EventUsage))
	}

	s := h.seen(t)
	if !slices.Equal(s.argv, []string{"app-server", "--listen", "stdio://"}) {
		t.Errorf("argv = %v", s.argv)
	}
	p := s.params(t, "thread/start")
	want := map[string]any{"approvalPolicy": "never", "sandbox": "danger-full-access", "model": "gpt-5.6-luna",
		"developerInstructions": "You are terse.", "cwd": spec.Workdir}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("thread/start %s = %v, want %v", k, p[k], v)
		}
	}
	if len(s.sent("initialized")) != 1 || len(s.sent("thread/resume")) != 0 {
		t.Errorf("handshake = %v", s.stdin)
	}
	tp := s.params(t, "turn/start")
	if tp["threadId"] != thread || !strings.Contains(mustJSON(tp["input"]), "do the thing") {
		t.Errorf("turn/start = %v", tp)
	}
	if n := len(s.sent("thread/inject_items")); n != 0 {
		t.Errorf("a new thread, whose developer instructions open it, injected %d items", n)
	}
	if _, ok := tp["effort"]; ok {
		t.Errorf("a run with no effort named one, where Codex's default was asked for: %v", tp)
	}
}

// A run's effort is its turn's, as the hub sent it: Codex decides which
// levels a model takes, and the runner checks none. Neither thread/start nor
// thread/resume carries it, so a continued session is not left at an effort
// an earlier run chose.
func TestEffortGoesOnTheTurn(t *testing.T) {
	h := &harness{fixture: fixture("effort")}
	spec := h.spec(t)
	spec.Effort = "low"
	if _, out, _ := drive(t, context.Background(), spec, nil); out.State != v1.RunSucceeded || out.FinalText != "pong" {
		t.Fatalf("outcome = %+v", out)
	}
	s := h.seen(t)
	if got := s.params(t, "turn/start")["effort"]; got != "low" {
		t.Errorf("turn/start effort = %v, want low", got)
	}
	if _, ok := s.params(t, "thread/start")["effort"]; ok {
		t.Errorf("thread/start carries the effort: %v", s.params(t, "thread/start"))
	}
	if !(Adapter{}).AppliesEffort() {
		t.Error("the codex adapter does not say it applies an effort, and the runner would refuse every run carrying one")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// The owner's approval policy and sandbox are sent as they are; nothing else
// sets them (decision 0015).
func TestOwnerSettingsWin(t *testing.T) {
	h := &harness{fixture: fixture("plain")}
	spec := h.spec(t)
	spec.Settings = map[string]string{"approval": "untrusted", "sandbox": "workspace-write"}
	spec.Model = ""
	if _, out, _ := drive(t, context.Background(), spec, nil); out.State != v1.RunSucceeded {
		t.Fatalf("outcome = %+v", out)
	}
	p := h.seen(t).params(t, "thread/start")
	if p["approvalPolicy"] != "untrusted" || p["sandbox"] != "workspace-write" {
		t.Errorf("thread/start = %v", p)
	}
	if _, ok := p["model"]; ok {
		t.Errorf("a run with no model named one: %v", p)
	}
	if _, ok := p["developerInstructions"]; ok {
		t.Errorf("a brief with no context sent developer instructions: %v", p)
	}
}

// A shell command becomes a tool call and its result.
func TestToolRun(t *testing.T) {
	h := &harness{fixture: fixture("tool")}
	evs, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded || out.FinalText != "hello from a small file" {
		t.Fatalf("outcome = %+v", out)
	}
	calls, results := kinds(evs, v1.EventToolCall), kinds(evs, v1.EventToolResult)
	if len(calls) != 1 || len(results) != 1 {
		t.Fatalf("calls %v, results %v", calls, results)
	}
	c, r := calls[0].Tool, results[0].Tool
	if c.Name != "shell" || !strings.Contains(c.Input, "cat note.txt") || r.ID != c.ID || r.Output != "hello from a small file\n" {
		t.Errorf("call %+v, result %+v", c, r)
	}
}

func TestFailures(t *testing.T) {
	for _, tc := range []struct {
		name, fixture string
		env           map[string]string
		class         string
		inMessage     string
		limit         *adapter.Limit
		// asked is the adapter's extra question after the turn.
		asked string
	}{
		{name: "an unknown model", fixture: "error", class: adapter.ClassHarness,
			inMessage: "The 'gpt-nonexistent-9' model is not supported"},
		// Codex takes the turn and its API refuses the level: the run fails
		// with Codex's words, which name the levels it would take.
		{name: "an effort the model does not take", fixture: "effort-rejected", class: adapter.ClassHarness,
			inMessage: "Invalid value: 'bogus'"},
		{name: "a context window exceeded", fixture: "prompt-too-long", class: adapter.ClassPromptTooLong,
			inMessage: "ran out of room"},
		{name: "a usage limit, with its window", fixture: "usage-limit", class: adapter.ClassUsageLimit,
			inMessage: "usage limit", limit: &adapter.Limit{Window: "primary", ResetAt: time.Unix(1789803060, 0).UTC()}},
		// Both windows full: the run waits for the later reset.
		{name: "a usage limit, window asked for", fixture: "usage-limit-unnamed", class: adapter.ClassUsageLimit,
			inMessage: "usage limit", limit: &adapter.Limit{Window: "secondary", ResetAt: time.Unix(1790300000, 0).UTC()},
			asked: "account/rateLimits/read"},
		{name: "a process that died mid-turn", fixture: "interrupt", env: map[string]string{"CODEX_TEST_MODE": "died"},
			class: adapter.ClassHarnessExited, inMessage: "something went badly wrong"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &harness{fixture: fixture(tc.fixture), env: tc.env}
			if tc.fixture == "interrupt" {
				h.fixture = cutBefore(t, "interrupt", `{">":{"jsonrpc":"2.0","id":4,"method":"turn/interrupt"`)
			}
			evs, out, _ := drive(t, context.Background(), h.spec(t), nil)
			if out.State != v1.RunFailed || out.Error == nil || out.Error.Class != tc.class || !strings.Contains(out.Error.Message, tc.inMessage) {
				t.Fatalf("outcome = %+v (%+v)", out, out.Error)
			}
			if out.FinalText != "" {
				t.Errorf("a failed run carries final text %q", out.FinalText)
			}
			if cls := errorClasses(evs); len(cls) == 0 || cls[len(cls)-1] != tc.class {
				t.Errorf("error events = %v, want the stream to end saying %s", cls, tc.class)
			}
			if tc.limit != nil && (out.Limit == nil || *out.Limit != *tc.limit) {
				t.Errorf("limit = %+v, want %+v", out.Limit, tc.limit)
			}
			if tc.limit == nil && out.Limit != nil {
				t.Errorf("limit = %+v on a run that hit none", out.Limit)
			}
			if tc.asked != "" && len(h.seen(t).sent(tc.asked)) != 1 {
				t.Errorf("the adapter did not ask %s", tc.asked)
			}
		})
	}
}

// cutBefore writes a copy of a fixture that ends just before the line with
// this prefix — a Codex that stopped there.
func cutBefore(t *testing.T, name, prefix string) string {
	t.Helper()
	b, err := os.ReadFile(fixture(name))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, prefix)
	if i < 0 {
		t.Fatalf("%s has no %s", name, prefix)
	}
	return writeFixture(t, s[:i])
}

func writeFixture(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fixture.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The app-server exits 0 without completing the turn: not a success.
func TestExitWithoutCompletion(t *testing.T) {
	h := &harness{fixture: cutBefore(t, "plain", `{"method":"turn/completed"`), env: map[string]string{"CODEX_TEST_MODE": "exit"}}
	_, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunFailed || out.Error == nil || out.Error.Class != adapter.ClassHarnessExited {
		t.Fatalf("outcome = %+v (%+v)", out, out.Error)
	}
}

// A resume hands Codex the stored thread, counts only this turn's usage — not
// what Codex replays of the thread's earlier turns — and answers from them.
func TestResume(t *testing.T) {
	h := &harness{fixture: fixture("resume")}
	spec := h.spec(t)
	spec.NativeSessionID = "01a0b879-aaaa-7050-84ee-d1a30d4b696d"
	evs, out, tr := drive(t, context.Background(), spec, nil)
	if out.State != v1.RunSucceeded || out.FinalText != "plum" {
		t.Fatalf("outcome = %+v", out)
	}
	if out.NativeSessionID != spec.NativeSessionID || tr.NativeSessionID() != spec.NativeSessionID {
		t.Errorf("native = %q, want the resumed %q", out.NativeSessionID, spec.NativeSessionID)
	}
	p := h.seen(t).params(t, "thread/resume")
	if p["threadId"] != spec.NativeSessionID || p["approvalPolicy"] != "never" {
		t.Errorf("thread/resume = %v", p)
	}
	if len(h.seen(t).sent("thread/start")) != 0 {
		t.Error("a resume started a new thread")
	}
	u := out.Usage["gpt-5.6-luna"]
	if total := u.Input + u.CacheRead; total == 0 || total > 13000 {
		// The replayed first turn alone is 11 871 tokens.
		t.Errorf("usage = %+v: counted more than the resumed turn", u)
	}
	if got := text(evs); got != "plum" {
		t.Errorf("text = %q: something replayed reached the run", got)
	}
	if n := len(h.seen(t).sent("thread/inject_items")); n != 0 {
		t.Errorf("a resumed run with no context injected %d items", n)
	}
}

// A continuing run's context reaches the model. Codex takes thread/resume's
// developerInstructions as the thread's but reads them only once a
// compaction rebuilds the thread's opening, so the adapter also injects the
// context as a developer message between the resume and the turn (decision
// 0050). Recorded: the first turn had no context, and the answer is the one
// only this run's context holds.
func TestAResumedRunCarriesItsContext(t *testing.T) {
	h := &harness{fixture: fixture("resume-context")}
	spec := h.spec(t)
	spec.NativeSessionID = threadIn(t, "resume-context")
	spec.Brief.Context = "The codeword is BLUE. If asked, give the codeword."
	_, out, _ := drive(t, context.Background(), spec, nil)
	if out.State != v1.RunSucceeded || out.FinalText != "BLUE" {
		t.Fatalf("outcome = %+v", out)
	}
	s := h.seen(t)
	if got := s.params(t, "thread/resume")["developerInstructions"]; got != spec.Brief.Context {
		t.Errorf("thread/resume developerInstructions = %v: after a compaction the thread would lose the context", got)
	}
	inj := mustJSON(s.params(t, "thread/inject_items"))
	for _, want := range []string{`"role":"developer"`, `"type":"input_text"`, "The codeword is BLUE."} {
		if !strings.Contains(inj, want) {
			t.Errorf("thread/inject_items %s lacks %s", inj, want)
		}
	}
	var order []string
	for _, m := range s.stdin {
		if method, _ := m["method"].(string); method == "thread/resume" || method == "thread/inject_items" || method == "turn/start" {
			order = append(order, method)
		}
	}
	if !slices.Equal(order, []string{"thread/resume", "thread/inject_items", "turn/start"}) {
		t.Errorf("sent %v, want the context injected between the resume and the turn", order)
	}
}

// A resume of a thread Codex has no rollout for says so, for the executor
// to report as resume_rejected (decision 0031); no turn is started.
func TestResumeMissing(t *testing.T) {
	h := &harness{fixture: fixture("resume-missing")}
	spec := h.spec(t)
	spec.NativeSessionID = "01a0b86e-0000-7000-8000-000000000000"
	evs, out, tr := drive(t, context.Background(), spec, nil)
	if out.State != v1.RunFailed || out.Error == nil || out.Error.Class != adapter.ClassSessionNotFound {
		t.Fatalf("outcome = %+v (%+v)", out, out.Error)
	}
	if !strings.Contains(out.Error.Message, "start a new session") {
		t.Errorf("message = %q, want the next action", out.Error.Message)
	}
	if slices.Contains(errorClasses(evs), adapter.ClassSessionNotFound) != true {
		t.Errorf("error events = %v", errorClasses(evs))
	}
	if tr.NativeSessionID() != spec.NativeSessionID {
		t.Errorf("native = %q", tr.NativeSessionID())
	}
	if len(h.seen(t).sent("turn/start")) != 0 {
		t.Error("a turn was started on a thread that was not resumed")
	}
}

// Codex resumed another thread than the one asked for: the run stops before
// its turn, as session_mismatch (decision 0031).
func TestResumeMismatch(t *testing.T) {
	h := &harness{fixture: fixture("resume"), env: map[string]string{"CODEX_TEST_KEEP_THREAD": "1"}}
	spec := h.spec(t)
	spec.NativeSessionID = "01a0b879-aaaa-7050-84ee-d1a30d4b696d"
	evs, out, _ := drive(t, context.Background(), spec, nil)
	if out.State != v1.RunFailed || out.Error == nil || out.Error.Class != adapter.ClassSessionMismatch {
		t.Fatalf("outcome = %+v (%+v)", out, out.Error)
	}
	if out.NativeSessionID != "" && out.NativeSessionID != spec.NativeSessionID {
		t.Errorf("the outcome adopted the echoed thread %q", out.NativeSessionID)
	}
	if !slices.Contains(errorClasses(evs), adapter.ClassSessionMismatch) {
		t.Errorf("error events = %v", errorClasses(evs))
	}
	if len(h.seen(t).sent("turn/start")) != 0 {
		t.Error("a turn was started in the wrong thread")
	}
}

// A malformed stored id is refused before anything starts.
func TestDamagedThreadID(t *testing.T) {
	h := &harness{fixture: fixture("plain")}
	spec := h.spec(t)
	spec.NativeSessionID = "--config=evil"
	if _, err := (Adapter{}).Start(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "close the session") {
		t.Fatalf("err = %v", err)
	}
}

func TestInterrupt(t *testing.T) {
	h := &harness{fixture: fixture("interrupt")}
	evs, out, _ := drive(t, context.Background(), h.spec(t), func(tr adapter.Turn, e v1.Event) bool {
		if e.Kind != v1.EventText {
			return false
		}
		if err := tr.Interrupt(); err != nil {
			t.Error(err)
		}
		return true
	})
	if out.State != v1.RunCancelled || out.Error != nil || out.FinalText != "" {
		t.Fatalf("outcome = %+v", out)
	}
	if text(evs) == "" {
		t.Error("the text before the interrupt was lost")
	}
	p := h.seen(t).params(t, "turn/interrupt")
	if p["threadId"] != threadIn(t, "interrupt") || p["turnId"] == "" {
		t.Errorf("turn/interrupt = %v", p)
	}
}

// An interrupt before Codex has answered anything ends the run with nothing
// run: the session is left exactly as an interrupt would leave it.
func TestInterruptBeforeTheTurn(t *testing.T) {
	hold := filepath.Join(t.TempDir(), "go")
	h := &harness{fixture: fixture("plain"), env: map[string]string{"CODEX_TEST_HOLD": hold}}
	tr, err := Adapter{}.Start(context.Background(), h.spec(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Interrupt(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(hold, nil, 0o600)
	_, out, _ := collect(t, tr, nil)
	if out.State != v1.RunCancelled {
		t.Fatalf("outcome = %+v", out)
	}
	if len(h.seen(t).sent("turn/start")) != 0 {
		t.Error("a turn was started after an interrupt")
	}
	if tr.Interrupt() != nil || tr.Steer("late") == nil {
		t.Error("a finished turn must accept an interrupt quietly and refuse a steer")
	}
}

// A steer at the first tool call is taken into the same turn, whose one
// answer carries it.
func TestSteer(t *testing.T) {
	h := &harness{fixture: fixture("steer")}
	_, out, tr := drive(t, context.Background(), h.spec(t), func(tr adapter.Turn, e v1.Event) bool {
		if e.Kind != v1.EventToolCall {
			return false
		}
		if err := tr.Steer("Also: end your final reply with the word STEERED."); err != nil {
			t.Error(err)
		}
		return true
	})
	if out.State != v1.RunSucceeded || !strings.HasSuffix(out.FinalText, "STEERED") {
		t.Fatalf("outcome = %+v", out)
	}
	p := h.seen(t).params(t, "turn/steer")
	if p["expectedTurnId"] == "" || !strings.Contains(mustJSON(p["input"]), "STEERED") {
		t.Errorf("turn/steer = %v", p)
	}
	if err := tr.Steer("too late"); err == nil || !strings.Contains(err.Error(), "new run in the same session") {
		t.Errorf("steer after the end: %v", err)
	}
}

// With an approval policy that asks, the request is declined — nobody is
// there — and the run goes on to its own answer.
func TestApprovalDeclined(t *testing.T) {
	h := &harness{fixture: fixture("approval")}
	spec := h.spec(t)
	spec.Settings = map[string]string{"approval": "untrusted", "sandbox": "read-only"}
	evs, out, _ := drive(t, context.Background(), spec, nil)
	if out.State != v1.RunSucceeded {
		t.Fatalf("outcome = %+v", out)
	}
	if !slices.ContainsFunc(statuses(evs), func(s string) bool {
		return strings.HasPrefix(s, "approval declined: ") && strings.Contains(s, "touch out.txt")
	}) {
		t.Errorf("statuses = %v", statuses(evs))
	}
	var replies []any
	for _, m := range h.seen(t).stdin {
		if _, ok := m["result"]; ok {
			replies = append(replies, m["result"])
		}
	}
	if len(replies) != 1 || mustJSON(replies[0]) != `{"decision":"decline"}` {
		t.Errorf("replies = %v", replies)
	}
}

// Codex runs subagents as threads on the same pipe, and a resume replays
// earlier turns: nothing from another thread or another turn is the run's.
// A server request of a kind YAD does not serve is refused, a line that is
// not JSON-RPC is reported and skipped, and the run carries on.
func TestOnlyTheRunsTurn(t *testing.T) {
	b, err := os.ReadFile(fixture("plain"))
	if err != nil {
		t.Fatal(err)
	}
	thread := threadIn(t, "plain")
	var turnID string
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.HasPrefix(line, `{"method":"item/agentMessage/delta"`) {
			var m struct {
				Params struct {
					TurnID string `json:"turnId"`
				} `json:"params"`
			}
			json.Unmarshal([]byte(line), &m)
			turnID = m.Params.TurnID
			out = append(out,
				delta("01a0b878-ffff-7783-b1a6-65eeb944100e", turnID, "SUBAGENT"),
				delta(thread, "01a0b878-0000-7562-880a-548f542e449d", "REPLAYED"),
				`{"method":"item/tool/requestUserInput","id":"q1","params":{"threadId":"`+thread+`","turnId":"`+turnID+`","itemId":"x","questions":[],"isBlocking":true}}`,
				`{">":{"jsonrpc":"2.0","id":"q1","error":{}}}`,
				`not json at all`,
			)
		}
		out = append(out, line)
	}
	h := &harness{fixture: writeFixture(t, strings.Join(out, "\n")+"\n")}
	evs, o, _ := drive(t, context.Background(), h.spec(t), nil)
	if o.State != v1.RunSucceeded || text(evs) != "pong" {
		t.Fatalf("outcome = %+v, text %q", o, text(evs))
	}
	if cls := errorClasses(evs); !slices.Equal(cls, []string{adapter.ClassStream}) {
		t.Errorf("error events = %v, want the one skipped line", cls)
	}
	var refused bool
	for _, m := range h.seen(t).stdin {
		if m["id"] == "q1" && m["error"] != nil {
			refused = true
		}
	}
	if !refused {
		t.Error("the question nobody can answer was not refused")
	}
}

func delta(thread, turn, s string) string {
	return `{"method":"item/agentMessage/delta","params":{"threadId":"` + thread + `","turnId":"` + turn + `","itemId":"m","delta":"` + s + `"}}`
}

// The ways an app-server fails to go away, and the one that never answers.
func TestStubbornProcesses(t *testing.T) {
	defer func(a, b, c, d time.Duration) {
		handshakeTimeout, exitGrace, drainGrace, termGrace = a, b, c, d
	}(handshakeTimeout, exitGrace, drainGrace, termGrace)
	exitGrace, drainGrace, termGrace = 100*time.Millisecond, 200*time.Millisecond, 200*time.Millisecond
	shipped := handshakeTimeout

	for _, tc := range []struct {
		name, mode string
		state      v1.RunState
		class      string
		// handshake is short only where it is meant to run out. The server
		// that answers must be given the shipped bound: its first answer comes
		// from a race-instrumented test binary still starting, which takes
		// longer than any short bound on a loaded machine (DEV-100).
		handshake time.Duration
	}{
		// The turn is over; the server ignores its closed input and SIGTERM.
		{name: "linger", mode: "linger", state: v1.RunSucceeded, handshake: shipped},
		// It closes its output and lingers.
		{name: "mute", mode: "mute", state: v1.RunSucceeded, handshake: shipped},
		// It never answers initialize.
		{name: "silent", mode: "silent", state: v1.RunFailed, class: adapter.ClassHarness, handshake: 300 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handshakeTimeout = tc.handshake
			h := &harness{fixture: fixture("plain"), env: map[string]string{"CODEX_TEST_MODE": tc.mode}}
			// A server that answers is timed from its first event, not from
			// the spawn: a slow start is the machine's, and it is what the
			// shipped handshake above is there to absorb. The first, because
			// any later one may come from the outcome, which is decided after
			// the reap — usage, an error, text still buffered — and would leave
			// nothing to time. For the same reason silent, whose only event is
			// its error, is timed from the start, which its short handshake
			// bounds whether the child has finished starting or not.
			from := time.Now()
			_, out, _ := drive(t, context.Background(), h.spec(t), func(adapter.Turn, v1.Event) bool {
				if tc.class == "" {
					from = time.Now()
				}
				return true
			})
			if out.State != tc.state || (tc.class != "") != (out.Error != nil) || out.Error != nil && out.Error.Class != tc.class {
				t.Fatalf("outcome = %+v (%+v)", out, out.Error)
			}
			if tc.mode == "silent" && !strings.Contains(out.Error.Message, "initialize") {
				t.Errorf("message = %q, want the request that went unanswered", out.Error.Message)
			}
			if d := time.Since(from); d > 5*time.Second {
				t.Errorf("took %s to reap", d)
			}
		})
	}
}

// The cancel ladder from the runner's side: an interrupt Codex never takes,
// then SIGTERM through Terminate, and the run is cancelled.
func TestTerminateAfterADeafInterrupt(t *testing.T) {
	h := &harness{fixture: fixture("interrupt"), env: map[string]string{"CODEX_TEST_MODE": "deaf"}}
	_, out, _ := drive(t, context.Background(), h.spec(t), func(tr adapter.Turn, e v1.Event) bool {
		if e.Kind != v1.EventText {
			return false
		}
		tr.Interrupt()
		go func() {
			time.Sleep(100 * time.Millisecond)
			tr.Terminate()
		}()
		return true
	})
	if out.State != v1.RunCancelled {
		t.Fatalf("outcome = %+v (%+v)", out, out.Error)
	}
}

// The ladder's last rung is the run's context: a Codex killed mid-turn is
// cancelled, not failed.
func TestContextCancelled(t *testing.T) {
	h := &harness{fixture: fixture("interrupt"), env: map[string]string{"CODEX_TEST_MODE": "deaf"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, out, _ := drive(t, ctx, h.spec(t), func(tr adapter.Turn, e v1.Event) bool {
		if e.Kind != v1.EventText {
			return false
		}
		tr.Interrupt()
		cancel()
		return true
	})
	if out.State != v1.RunCancelled {
		t.Fatalf("outcome = %+v (%+v)", out, out.Error)
	}
}

func TestStartRefusals(t *testing.T) {
	if _, err := (Adapter{}).Start(context.Background(), adapter.Spec{}); err == nil || !strings.Contains(err.Error(), "yad doctor") {
		t.Errorf("no binary: %v", err)
	}
	// The exec error names the binary under the owner's home; the run's error
	// goes to a hub, so the path travels only as the cause (DEV-67).
	const bin = "/Users/someone/bin/codex"
	_, err := (Adapter{}).Start(context.Background(), adapter.Spec{Binary: bin, Workdir: t.TempDir()})
	le, ok := errors.AsType[*adapter.LocalError](err)
	if !ok || strings.Contains(err.Error(), bin) || !strings.Contains(err.Error(), "would not start") {
		t.Errorf("a binary that will not exec: %v", err)
	}
	if ok && !strings.Contains(le.Err.Error(), bin) {
		t.Errorf("the cause lost what the owner needs: %v", le.Err)
	}
}

// inject writes a copy of a recorded conversation with extra lines after the
// first line starting with after.
func inject(t *testing.T, name, after string, lines ...string) string {
	t.Helper()
	b, err := os.ReadFile(fixture(name))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	done := false
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		out = append(out, line)
		if !done && strings.HasPrefix(line, after) {
			out = append(out, lines...)
			done = true
		}
	}
	if !done {
		t.Fatalf("%s has no line starting %s", name, after)
	}
	return writeFixture(t, strings.Join(out, "\n")+"\n")
}

// A turn/started that names no turn opens nothing, and the run goes on to its
// own turn — rather than closing the gate twice and taking the runner down.
func TestTurnStartedWithoutAnID(t *testing.T) {
	thread := threadIn(t, "plain")
	h := &harness{fixture: inject(t, "plain", `{">":{"jsonrpc":"2.0","id":3,"method":"turn/start"`,
		`{"method":"turn/started","params":{"threadId":"`+thread+`","turn":{"id":"","items":[],"status":"inProgress"}}}`)}
	evs, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded || text(evs) != "pong" {
		t.Fatalf("outcome = %+v, text %q", out, text(evs))
	}
}

// A steer waits for the turn to start and for Codex to take it within one
// bound, not one bound each: the runner's event loop and its cancel ladder
// wait on it.
func TestSteerHasOneBound(t *testing.T) {
	defer func(d time.Duration) { steerTimeout = d }(steerTimeout)
	steerTimeout = time.Second
	hold := filepath.Join(t.TempDir(), "go")
	// Codex takes the steer and never answers it.
	h := &harness{fixture: cutBefore(t, "steer", `{"id":4,"result"`), env: map[string]string{"CODEX_TEST_HOLD": hold}}
	tr, err := Adapter{}.Start(context.Background(), h.spec(t))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(600 * time.Millisecond)
		os.WriteFile(hold, nil, 0o600)
	}()
	start := time.Now()
	err = tr.Steer("Also: end your final reply with the word STEERED.")
	took := time.Since(start)
	if err == nil {
		t.Fatal("a steer Codex never answered was reported taken")
	}
	if took > steerTimeout+300*time.Millisecond {
		t.Errorf("steer took %s, past its %s bound", took, steerTimeout)
	}
	tr.Terminate()
	collect(t, tr, nil)
}

// Codex's rate-limit updates are sparse: a window an update leaves out keeps
// what the last one said, so the limit names the window that is really full.
func TestRateLimitUpdatesMerge(t *testing.T) {
	tr := newTranslator(func(v1.Event) {})
	tr.rateLimits([]byte(`{"rateLimits":{"primary":{"usedPercent":100,"resetsAt":1789803060},"secondary":{"usedPercent":40,"resetsAt":1790300000}}}`))
	tr.rateLimits([]byte(`{"rateLimits":{"secondary":{"usedPercent":41,"resetsAt":1790300000}}}`))
	l := tr.limit()
	if l.Window != "primary" || !l.ResetAt.Equal(time.Unix(1789803060, 0)) {
		t.Errorf("limit = %+v", l)
	}
}

// The older approval requests name their thread conversationId; the run's
// own are declined and said to be, as the newer ones are.
func TestLegacyApprovalDeclined(t *testing.T) {
	thread := threadIn(t, "plain")
	h := &harness{fixture: inject(t, "plain", `{"method":"turn/started"`,
		`{"method":"execCommandApproval","id":"a1","params":{"conversationId":"`+thread+`","callId":"c","command":["touch","x"],"cwd":"/work","parsedCmd":[]}}`,
		`{">":{"jsonrpc":"2.0","id":"a1","result":{}}}`)}
	evs, out, _ := drive(t, context.Background(), h.spec(t), nil)
	if out.State != v1.RunSucceeded {
		t.Fatalf("outcome = %+v", out)
	}
	if !slices.Contains(statuses(evs), "approval declined: execCommand") {
		t.Errorf("statuses = %v", statuses(evs))
	}
	var reply string
	for _, m := range h.seen(t).stdin {
		if m["id"] == "a1" {
			reply = mustJSON(m["result"])
		}
	}
	if !strings.Contains(reply, `"denied"`) {
		t.Errorf("reply = %s", reply)
	}
}
