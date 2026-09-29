package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/acp/acptest"
)

var _ adapter.Adapter = Adapter{}

// fixtures is the OpenCode release the replayed conversations were recorded
// from, which is the pinned one.
var fixtures = filepath.Join("testdata", "opencode-"+PinnedVersion)

// missingSession is shaped like an OpenCode session id, and no session has
// it: what resume-missing and fork-missing were recorded asking for.
const missingSession = "ses_0000000000000000000000000000"

func TestMain(m *testing.M) {
	// This package's children are all the fake, whatever they are named.
	if os.Getenv(acptest.EnvFixture) != "" {
		acptest.Main()
		return
	}
	os.Exit(m.Run())
}

// play is one fake OpenCode run: what it plays and what it saw.
type play struct {
	fixture string
	env     map[string]string
	log     string
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(fixtures, name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *play) spec(t *testing.T) adapter.Spec {
	t.Helper()
	p.log = filepath.Join(t.TempDir(), "log.jsonl")
	env := []string{
		acptest.EnvFixture + "=" + p.fixture,
		acptest.EnvLog + "=" + p.log,
		acptest.EnvWatch + "=" + envPassword + "," + envConfig,
		// A race-enabled child otherwise sleeps a second at exit.
		"GORACE=atexit_sleep_ms=0",
	}
	for k, v := range p.env {
		env = append(env, k+"="+v)
	}
	return adapter.Spec{
		RunID: "r1", Binary: os.Args[0], Workdir: t.TempDir(),
		Model: "opencode/nemotron-3.5-lightning-free", Env: env,
		Brief: v1.Brief{Instruction: "do the thing"},
	}
}

// seen is what the fake logged: its argv, the variables it was watching, and
// what the adapter wrote.
type seen struct {
	argv  []string
	env   map[string]string
	stdin []map[string]any
}

func (p *play) seen(t *testing.T) seen {
	t.Helper()
	f, err := os.Open(p.log)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := seen{env: map[string]string{}}
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		var e struct {
			Argv  []string          `json:"argv"`
			Env   map[string]string `json:"env"`
			Stdin *string           `json:"stdin"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		switch {
		case e.Argv != nil:
			s.argv = e.Argv
		case e.Env != nil:
			s.env = e.Env
		case e.Stdin != nil:
			var m map[string]any
			json.Unmarshal([]byte(*e.Stdin), &m)
			s.stdin = append(s.stdin, m)
		}
	}
	return s
}

func (s seen) sent(method string) []map[string]any {
	var out []map[string]any
	for _, m := range s.stdin {
		if m["method"] == method {
			out = append(out, m)
		}
	}
	return out
}

// run plays spec to its end and returns what the runner would see.
func run(t *testing.T, spec adapter.Spec, act func(tr adapter.Turn, e v1.Event)) ([]v1.Event, adapter.Outcome) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tr, err := Adapter{}.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	var events []v1.Event
	for e := range tr.Events() {
		events = append(events, e)
		if act != nil {
			act(tr, e)
		}
	}
	return events, tr.Wait()
}

func kinds(events []v1.Event, kind v1.EventKind) []v1.Event {
	var out []v1.Event
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func text(events []v1.Event, kind v1.EventKind) string {
	var b strings.Builder
	for _, e := range kinds(events, kind) {
		b.WriteString(e.Text)
	}
	return b.String()
}

// A new session's turn: the answer is the last message, its reasoning is
// thinking, its usage is the prompt's, and its context reaches OpenCode as an
// instruction file, never in argv — beside a password for OpenCode's own
// server that is new on every run (decision 0072).
func TestPlainRun(t *testing.T) {
	p := &play{fixture: fixture(t, "plain")}
	spec := p.spec(t)
	spec.Brief.Context = "You are terse."
	events, o := run(t, spec, nil)
	if o.State != v1.RunSucceeded || o.FinalText != "pong" {
		t.Fatalf("outcome %s %q %+v", o.State, o.FinalText, o.Error)
	}
	if !strings.HasPrefix(o.NativeSessionID, "ses_") {
		t.Errorf("native session %q", o.NativeSessionID)
	}
	if text(events, v1.EventThinking) == "" {
		t.Error("no thinking event")
	}
	u, ok := o.Usage[spec.Model]
	if !ok || u.Input == 0 || u.Output == 0 {
		t.Errorf("usage %+v", o.Usage)
	}
	if st := kinds(events, v1.EventStatus); len(st) == 0 || st[0].Status != "started" {
		t.Errorf("the first status is not started: %+v", st)
	}
	s := p.seen(t)
	if !slices.Equal(s.argv, []string{"acp"}) {
		t.Errorf("argv %v", s.argv)
	}
	pw := s.env[envPassword]
	if len(pw) != 2*passwordBytes {
		t.Errorf("OpenCode's server password is %d characters, want %d", len(pw), 2*passwordBytes)
	}
	for _, a := range s.argv {
		if strings.Contains(a, pw) {
			t.Error("the password is in argv")
		}
	}
	var config struct {
		Instructions []string `json:"instructions"`
	}
	if err := json.Unmarshal([]byte(s.env[envConfig]), &config); err != nil || len(config.Instructions) != 1 {
		t.Fatalf("%s = %q: %v", envConfig, s.env[envConfig], err)
	}
	if _, err := os.Stat(config.Instructions[0]); !os.IsNotExist(err) {
		t.Errorf("the context file %s outlived the run: %v", config.Instructions[0], err)
	}
	set := s.sent("session/set_config_option")
	if len(set) != 1 || set[0]["params"].(map[string]any)["value"] != spec.Model {
		t.Errorf("set_config_option %v", set)
	}
	if prompt := s.sent("session/prompt"); len(prompt) != 1 {
		t.Errorf("prompts %v", prompt)
	}
}

// Each run's password is its own.
func TestEachRunHasItsOwnPassword(t *testing.T) {
	var got []string
	for range 2 {
		p := &play{fixture: fixture(t, "plain")}
		run(t, p.spec(t), nil)
		got = append(got, p.seen(t).env[envPassword])
	}
	if got[0] == got[1] || got[0] == "" {
		t.Errorf("passwords %q", got)
	}
}

// A context the owner's environment already configures OpenCode inline for
// is added to that configuration, not put in its place.
func TestTheContextJoinsTheOwnersInlineConfig(t *testing.T) {
	t.Setenv(envConfig, `{"instructions":["/etc/team.md"],"share":"disabled"}`)
	p := &play{fixture: fixture(t, "plain")}
	spec := p.spec(t)
	spec.Brief.Context = "You are terse."
	run(t, spec, nil)
	var config map[string]any
	if err := json.Unmarshal([]byte(p.seen(t).env[envConfig]), &config); err != nil {
		t.Fatal(err)
	}
	in, _ := config["instructions"].([]any)
	if len(in) != 2 || in[0] != "/etc/team.md" || config["share"] != "disabled" {
		t.Errorf("config %v", config)
	}

	t.Setenv(envConfig, `{ // not JSON`)
	bad := &play{fixture: fixture(t, "plain")}
	spec = bad.spec(t)
	spec.Brief.Context = "You are terse."
	if _, err := (Adapter{}).Start(context.Background(), spec); err == nil || !strings.Contains(err.Error(), envConfig) {
		t.Errorf("start with unreadable inline config: %v", err)
	}
}

// OpenCode's own variables are the owner's to set: a hub's grant of one —
// permissions, inline config naming a provider and its key, credentials, the
// server's password — never reaches OpenCode, while the owner's own does.
func TestAGrantCannotConfigureOpenCode(t *testing.T) {
	t.Setenv("OPENCODE_DISABLE_AUTOUPDATE", "1")
	p := &play{fixture: fixture(t, "plain"), env: map[string]string{
		"OPENCODE_PERMISSION":   `"allow"`,
		envConfig:               `{"permission":"allow"}`,
		"OPENCODE_AUTH_CONTENT": `{"anthropic":{"type":"api","key":"sk-hub"}}`,
		envPassword:             "the-hubs-password",
		"PROJECT_TOKEN":         "kept",
	}}
	spec := p.spec(t)
	for i, kv := range spec.Env {
		if strings.HasPrefix(kv, acptest.EnvWatch+"=") {
			spec.Env[i] += ",OPENCODE_PERMISSION,OPENCODE_AUTH_CONTENT,OPENCODE_DISABLE_AUTOUPDATE,PROJECT_TOKEN"
		}
	}
	run(t, spec, nil)
	env := p.seen(t).env
	for _, name := range []string{"OPENCODE_PERMISSION", envConfig, "OPENCODE_AUTH_CONTENT"} {
		if v, ok := env[name]; ok {
			t.Errorf("the grant %s reached OpenCode: %q", name, v)
		}
	}
	if env[envPassword] == "the-hubs-password" || len(env[envPassword]) != 2*passwordBytes {
		t.Errorf("the server's password is not the runner's own")
	}
	if env["OPENCODE_DISABLE_AUTOUPDATE"] != "1" || env["PROJECT_TOKEN"] != "kept" {
		t.Errorf("the owner's variable or the project's grant did not reach OpenCode: %v", env)
	}
}

// A tool call is announced once its input is whole, and its result joins it
// by id; a command's exit status is OpenCode's own.
func TestToolCalls(t *testing.T) {
	p := &play{fixture: fixture(t, "tool")}
	events, o := run(t, p.spec(t), nil)
	if o.State != v1.RunSucceeded || !strings.Contains(o.FinalText, "hello from a small file") {
		t.Fatalf("outcome %s %q %+v", o.State, o.FinalText, o.Error)
	}
	calls, results := kinds(events, v1.EventToolCall), kinds(events, v1.EventToolResult)
	if len(calls) != 1 || len(results) != 1 {
		t.Fatalf("calls %+v results %+v", calls, results)
	}
	c, r := calls[0].Tool, results[0].Tool
	if c.Name != "bash" || !strings.Contains(c.Input, "cat note.txt") || r.ID != c.ID {
		t.Errorf("call %+v result %+v", c, r)
	}
	if !strings.Contains(r.Output, "hello from a small file") || r.IsError == nil || *r.IsError || r.ExitCode == nil || *r.ExitCode != 0 {
		t.Errorf("result %+v", r)
	}

	p = &play{fixture: fixture(t, "tool-outcomes")}
	events, _ = run(t, p.spec(t), nil)
	results = kinds(events, v1.EventToolResult)
	if len(results) != 2 {
		t.Fatalf("results %+v", results)
	}
	if f := results[0].Tool; f.IsError == nil || !*f.IsError || f.ExitCode == nil || *f.ExitCode != 3 {
		t.Errorf("the failing command %+v", f)
	}
	if ok := results[1].Tool; ok.IsError == nil || *ok.IsError {
		t.Errorf("the succeeding command %+v", ok)
	}
}

// A model OpenCode does not have fails the run with its own words, before any
// prompt is sent.
func TestAnUnknownModelIsOpenCodesToRefuse(t *testing.T) {
	p := &play{fixture: fixture(t, "error")}
	spec := p.spec(t)
	spec.Model = "opencode/no-such-model"
	_, o := run(t, spec, nil)
	if o.State != v1.RunFailed || o.Error.Class != adapter.ClassHarness || !strings.Contains(o.Error.Message, "model not found") {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
	if n := len(p.seen(t).sent("session/prompt")); n != 0 {
		t.Errorf("%d prompts sent", n)
	}
}

// An effort is the session's thought_level option; one the model does not
// have is OpenCode's to refuse, and a model with no levels at all is refused
// before anything is asked.
func TestEffort(t *testing.T) {
	p := &play{fixture: fixture(t, "effort")}
	spec := p.spec(t)
	spec.Model, spec.Effort = effortModelOf(t, p.fixture), "high"
	_, o := run(t, spec, nil)
	if o.State != v1.RunSucceeded {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
	var efforts []any
	for _, m := range p.seen(t).sent("session/set_config_option") {
		if params := m["params"].(map[string]any); params["configId"] == "effort" {
			efforts = append(efforts, params["value"])
		}
	}
	if len(efforts) != 1 || efforts[0] != "high" {
		t.Errorf("effort set %v", efforts)
	}

	p = &play{fixture: fixture(t, "effort-rejected")}
	spec = p.spec(t)
	spec.Model, spec.Effort = effortModelOf(t, p.fixture), "bogus"
	_, o = run(t, spec, nil)
	if o.State != v1.RunFailed || o.Error.Class != adapter.ClassHarness || !strings.Contains(o.Error.Message, "effort not found") {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}

	p = &play{fixture: fixture(t, "plain")}
	spec = p.spec(t)
	spec.Effort = "low"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tr, err := Adapter{}.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	for range tr.Events() {
	}
	o = tr.Wait()
	if o.State != v1.RunFailed || !strings.Contains(o.Error.Message, "no effort levels") {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
}

// effortModelOf is the model a recorded effort run set.
func effortModelOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		var ours struct {
			Msg struct {
				Method string `json:"method"`
				Params struct {
					ConfigID string `json:"configId"`
					Value    string `json:"value"`
				} `json:"params"`
			} `json:">"`
		}
		if json.Unmarshal([]byte(line), &ours) == nil && ours.Msg.Params.ConfigID == "model" {
			return ours.Msg.Params.Value
		}
	}
	t.Fatalf("%s sets no model", path)
	return ""
}

// A resume continues the session asked for, without a replay; one OpenCode
// does not have is session_not_found, told apart from any other refusal by
// OpenCode's own list of the workdir's sessions.
func TestResume(t *testing.T) {
	p := &play{fixture: fixture(t, "resume")}
	spec := p.spec(t)
	spec.NativeSessionID = recordedSession(t, p.fixture, "session/resume")
	_, o := run(t, spec, nil)
	if o.State != v1.RunSucceeded || !strings.Contains(strings.ToLower(o.FinalText), "plum") || o.NativeSessionID != spec.NativeSessionID {
		t.Fatalf("outcome %s %q %s %+v", o.State, o.FinalText, o.NativeSessionID, o.Error)
	}

	p = &play{fixture: fixture(t, "resume-missing")}
	spec = p.spec(t)
	spec.NativeSessionID = missingSession
	_, o = run(t, spec, nil)
	if o.State != v1.RunFailed || o.Error.Class != adapter.ClassSessionNotFound {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
	if n := len(p.seen(t).sent("session/list")); n != 1 {
		t.Errorf("%d lists", n)
	}
}

// recordedSession is the session a recorded request named.
func recordedSession(t *testing.T, path, method string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		var ours struct {
			Msg struct {
				Method string `json:"method"`
				Params struct {
					SessionID string `json:"sessionId"`
				} `json:"params"`
			} `json:">"`
		}
		if json.Unmarshal([]byte(line), &ours) == nil && ours.Msg.Method == method {
			return ours.Msg.Params.SessionID
		}
	}
	t.Fatalf("%s sends no %s", path, method)
	return ""
}

// A fork is a new session: OpenCode replays the conversation it copies before
// answering, and none of that replay is the run's (decision 0065).
func TestFork(t *testing.T) {
	p := &play{fixture: fixture(t, "fork")}
	spec := p.spec(t)
	spec.Brief.Context = "You are terse."
	spec.ForkFrom = recordedSession(t, p.fixture, "session/fork")
	events, o := run(t, spec, nil)
	if o.State != v1.RunSucceeded || !strings.Contains(strings.ToLower(o.FinalText), "plum") {
		t.Fatalf("outcome %s %q %+v", o.State, o.FinalText, o.Error)
	}
	if o.NativeSessionID == "" || o.NativeSessionID == spec.ForkFrom {
		t.Errorf("the fork's session %q, forked %q", o.NativeSessionID, spec.ForkFrom)
	}
	if strings.Contains(text(events, v1.EventText), "ok") && strings.HasPrefix(text(events, v1.EventText), "ok") {
		t.Errorf("the replay reached the run's events: %q", text(events, v1.EventText))
	}

	p = &play{fixture: fixture(t, "fork-missing")}
	spec = p.spec(t)
	spec.ForkFrom = missingSession
	_, o = run(t, spec, nil)
	if o.State != v1.RunFailed || o.Error.Class != adapter.ClassSessionNotFound {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
}

// An interrupt is session/cancel, and the turn it ends is cancelled; one that
// comes before the prompt stops the run without sending it.
func TestInterrupt(t *testing.T) {
	p := &play{fixture: fixture(t, "interrupt")}
	_, o := run(t, p.spec(t), func(tr adapter.Turn, e v1.Event) {
		if e.Kind == v1.EventText {
			tr.Interrupt()
		}
	})
	if o.State != v1.RunCancelled {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
	if n := len(p.seen(t).sent("session/cancel")); n != 1 {
		t.Errorf("%d cancels", n)
	}

	at := filepath.Join(t.TempDir(), "at-gate")
	p = &play{fixture: fixture(t, "plain"), env: map[string]string{acptest.EnvGate: filepath.Join(t.TempDir(), "never"), acptest.EnvAtGate: at}}
	_, o = run(t, p.spec(t), func(tr adapter.Turn, e v1.Event) {
		if e.Kind == v1.EventText {
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if _, err := os.Stat(at); err == nil {
					break
				}
			}
			tr.Interrupt()
		}
	})
	if o.State != v1.RunCancelled {
		t.Fatalf("at the gate: outcome %s %+v", o.State, o.Error)
	}
}

// ACP v1 has no steer.
func TestSteerIsRefused(t *testing.T) {
	p := &play{fixture: fixture(t, "plain")}
	var steerErr error
	run(t, p.spec(t), func(tr adapter.Turn, e v1.Event) {
		if steerErr == nil {
			steerErr = tr.Steer("more")
		}
	})
	if steerErr == nil || !strings.Contains(steerErr.Error(), "new run") {
		t.Errorf("steer: %v", steerErr)
	}
}

// A permission request is answered from the owner's permission_mode, never
// from anything a hub sent: allow by default, reject when the owner says so.
func TestPermissionRequests(t *testing.T) {
	for _, tc := range []struct {
		fixture, mode, want string
		declined            bool
	}{
		{"permission", "", "once", false},
		{"permission-rejected", "reject", "reject", true},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			p := &play{fixture: fixture(t, tc.fixture)}
			spec := p.spec(t)
			spec.Settings = map[string]string{"permission_mode": tc.mode}
			events, _ := run(t, spec, nil)
			var answers []string
			for _, m := range p.seen(t).stdin {
				if r, ok := m["result"].(map[string]any); ok {
					if out, ok := r["outcome"].(map[string]any); ok {
						answers = append(answers, out["optionId"].(string))
					}
				}
			}
			if len(answers) == 0 || answers[0] != tc.want {
				t.Errorf("answers %v, want %s", answers, tc.want)
			}
			declined := false
			for _, e := range kinds(events, v1.EventStatus) {
				declined = declined || strings.HasPrefix(e.Status, "permission declined")
			}
			if declined != tc.declined {
				t.Errorf("declined status %v, want %v", declined, tc.declined)
			}
		})
	}

	p := &play{fixture: fixture(t, "plain")}
	spec := p.spec(t)
	spec.Settings = map[string]string{"permission_mode": "bypassPermissions"}
	if _, err := (Adapter{}).Start(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "permission_mode") {
		t.Errorf("an unknown permission_mode: %v", err)
	}
}

// An OpenCode that dies before answering the prompt is harness_exited, with
// its last words.
func TestDiedBeforeTheAnswer(t *testing.T) {
	p := &play{fixture: fixture(t, "plain"), env: map[string]string{acptest.EnvMode: "died"}}
	_, o := run(t, p.spec(t), nil)
	if o.State != v1.RunFailed || o.Error.Class != adapter.ClassHarnessExited || !strings.Contains(o.Error.Message, "something went badly wrong") {
		t.Fatalf("outcome %s %+v", o.State, o.Error)
	}
}

// A failed prompt reaches the run classified: provider-error was recorded
// (Zen's upstream down); the rest are plain with its answer replaced by the
// error OpenCode 1.18.33 sends for each.
func TestFailedPrompts(t *testing.T) {
	for _, tc := range []struct {
		fixture, class string
		limit, auth    bool
	}{
		{"provider-error", adapter.ClassHarness, false, false},
		{"usage-limit", adapter.ClassUsageLimit, true, false},
		{"auth-rejected", adapter.ClassHarness, false, true},
		{"prompt-too-long", adapter.ClassPromptTooLong, false, false},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			p := &play{fixture: fixture(t, tc.fixture)}
			spec := p.spec(t)
			if m := effortModelOf(t, p.fixture); m != spec.Model {
				spec.Model = m
			}
			if strings.Contains(recordedLine(t, p.fixture, "session/set_config_option", "effort"), "effort") {
				spec.Effort = "low"
			}
			events, o := run(t, spec, nil)
			if o.State != v1.RunFailed || o.Error.Class != tc.class {
				t.Fatalf("outcome %s %+v", o.State, o.Error)
			}
			if (o.Limit != nil) != tc.limit || o.AuthRejected != tc.auth {
				t.Errorf("limit %+v, auth %v", o.Limit, o.AuthRejected)
			}
			if errs := kinds(events, v1.EventError); len(errs) == 0 || errs[len(errs)-1].Error.Class != tc.class {
				t.Errorf("error events %+v", errs)
			}
		})
	}
}

// recordedLine is the first line of ours in a fixture sending method whose
// text holds want, or "".
func recordedLine(t *testing.T, path, method, want string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, `{">":`) && strings.Contains(line, `"method":"`+method+`"`) && strings.Contains(line, want) {
			return line
		}
	}
	return ""
}
