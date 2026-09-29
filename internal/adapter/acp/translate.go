package acp

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/jsonrpc"
)

// The shapes read from the agent, as far as the core reads them. Unknown
// fields and update kinds are ignored: the schema grows them within v1
// (plan, available_commands_update, usage_update and the rest), and none of
// them is a run's event.

// promptResult is session/prompt's answer: why the turn stopped, and — an
// unstable part of v1 — what it used.
type promptResult struct {
	StopReason string  `json:"stopReason"`
	Usage      *tokens `json:"usage"`
}

// tokens is a turn's usage as ACP reports it. OpenCode 1.18.33 reports the
// turn's last model call, not the sum of its calls (decision 0072).
type tokens struct {
	InputTokens       int64 `json:"inputTokens"`
	OutputTokens      int64 `json:"outputTokens"`
	ThoughtTokens     int64 `json:"thoughtTokens"`
	CachedReadTokens  int64 `json:"cachedReadTokens"`
	CachedWriteTokens int64 `json:"cachedWriteTokens"`
}

// doneUsage is the prompt's usage, or nil for none: no answer, no usage, or
// all of it zero.
func (t *translator) doneUsage() *tokens {
	if t.done == nil || t.done.Usage == nil || *t.done.Usage == (tokens{}) {
		return nil
	}
	return t.done.Usage
}

// update is one session/update, every kind the core reads in one shape.
type update struct {
	Kind      string          `json:"sessionUpdate"`
	MessageID string          `json:"messageId"`
	Content   json.RawMessage `json:"content"`

	// tool_call and tool_call_update
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`
}

// contentBlock is a text block, the only kind a message chunk carries that
// the core reads; an image or a resource is named rather than carried.
type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolContent is one entry of a tool call's content: text the tool produced,
// a diff it made, a terminal it ran in.
type toolContent struct {
	Type    string       `json:"type"`
	Content contentBlock `json:"content"`
	Path    string       `json:"path"`
}

// tool is a tool call as far as the run has seen it.
type tool struct {
	name   string
	input  json.RawMessage
	called bool // its tool_call event is out
}

// translator turns the run's own updates into protocol events and, once the
// prompt is answered, an Outcome. It holds no process and does no I/O; the
// turn decides which updates are the run's (acp.go).
type translator struct {
	emit  func(v1.Event)
	now   func() time.Time
	text  adapter.Text
	agent Agent
	spec  adapter.Spec

	session string
	model   string

	tools map[string]*tool
	// final is the turn's answer: the text of its last agent message, which
	// a tool call between two messages starts over.
	final     strings.Builder
	finalID   string
	done      *promptResult
	promptErr *jsonrpc.RPCError
	mismatch  string
	fault     *v1.RunError
}

func newTranslator(emit func(v1.Event), agent Agent, spec adapter.Spec) *translator {
	t := &translator{emit: emit, now: time.Now, agent: agent, spec: spec, tools: map[string]*tool{}}
	t.text = adapter.Text{Emit: emit, Now: func() time.Time { return t.now() }}
	return t
}

func (t *translator) update(raw json.RawMessage) {
	var u update
	if json.Unmarshal(raw, &u) != nil {
		return
	}
	switch u.Kind {
	case "agent_message_chunk":
		var c contentBlock
		if json.Unmarshal(u.Content, &c) != nil || c.Type != "text" {
			return
		}
		if u.MessageID != t.finalID {
			t.final.Reset()
			t.finalID = u.MessageID
		}
		t.final.WriteString(c.Text)
		t.text.Add(v1.EventText, c.Text)
	case "agent_thought_chunk":
		var c contentBlock
		if json.Unmarshal(u.Content, &c) == nil && c.Type == "text" {
			t.text.Add(v1.EventThinking, c.Text)
		}
	case "tool_call", "tool_call_update":
		t.tool(u)
	}
}

// tool follows one tool call through its updates. The call's event goes out
// once the agent has said what the call is — at in_progress, when an agent
// like OpenCode first carries the whole input — or with its result, for one
// that finishes without that; its result goes out when it is completed or
// failed. The name is the call's first title, which OpenCode sets to the
// tool's own name before it rewrites it to describe the call.
func (t *translator) tool(u update) {
	if u.ToolCallID == "" {
		return
	}
	tc, ok := t.tools[u.ToolCallID]
	if !ok {
		tc = &tool{name: u.Title}
		t.tools[u.ToolCallID] = tc
		// A tool call between two messages: the answer is what comes after.
		t.final.Reset()
		t.finalID = ""
	}
	if len(u.RawInput) > 0 && string(u.RawInput) != "null" {
		tc.input = u.RawInput
	}
	switch u.Status {
	case "in_progress":
		t.call(u.ToolCallID, tc)
	case "completed", "failed":
		t.call(u.ToolCallID, tc)
		out, cut := adapter.CapTool(toolOutput(u))
		failed := u.Status == "failed"
		var exit *int
		if t.agent.ExitCode != nil && len(u.RawOutput) > 0 {
			exit = t.agent.ExitCode(u.RawOutput)
		}
		if exit != nil && *exit != 0 {
			failed = true
		}
		t.emit(v1.Event{At: t.now(), Kind: v1.EventToolResult, Tool: &v1.ToolEvent{ID: u.ToolCallID, Output: out, Truncated: cut, IsError: &failed, ExitCode: exit}})
		delete(t.tools, u.ToolCallID)
	}
}

func (t *translator) call(id string, tc *tool) {
	if tc.called {
		return
	}
	tc.called = true
	t.text.Flush()
	in, cut := adapter.CapTool(string(tc.input))
	t.emit(v1.Event{At: t.now(), Kind: v1.EventToolCall, Tool: &v1.ToolEvent{ID: id, Name: firstNonEmpty(tc.name, "tool"), Input: in, Truncated: cut}})
}

// toolOutput is a finished call's content as text: what the tool printed,
// the files a diff touched. Where the agent put nothing there, its rawOutput
// as it sent it.
func toolOutput(u update) string {
	var content []toolContent
	json.Unmarshal(u.Content, &content)
	parts := make([]string, 0, len(content))
	for _, c := range content {
		switch {
		case c.Type == "content" && c.Content.Type == "text":
			parts = append(parts, c.Content.Text)
		case c.Type == "diff":
			parts = append(parts, "edit "+c.Path)
		case c.Type == "content":
			parts = append(parts, "["+c.Content.Type+"]")
		default:
			parts = append(parts, "["+c.Type+"]")
		}
	}
	if len(parts) == 0 && len(u.RawOutput) > 0 && string(u.RawOutput) != "null" {
		return string(u.RawOutput)
	}
	return strings.Join(parts, "\n")
}

func (t *translator) status(s string) {
	t.emit(v1.Event{At: t.now(), Kind: v1.EventStatus, Status: s})
}

func (t *translator) emitErr(class, msg string) {
	t.emit(v1.Event{At: t.now(), Kind: v1.EventError, Error: &v1.RunError{Class: class, Message: msg}})
}

// fail records that the conversation failed before the prompt could decide
// the run. The first failure is the one reported.
func (t *translator) fail(class, msg string) {
	if t.fault == nil {
		t.fault = &v1.RunError{Class: class, Message: msg}
	}
}

// failed records a prompt the agent answered with an error, which the
// agent's Classify reads when the run's outcome is decided.
func (t *translator) failed(e *jsonrpc.RPCError) {
	t.text.Flush()
	t.promptErr = e
}

func (t *translator) mismatchMessage() string {
	return fmt.Sprintf("%s answered session/fork with session %s, the one to fork, rather than a new session — the fork did not take and the turn was not started; report this with `%s --version` as a yad bug", t.agent.Name, t.mismatch, t.agent.ID)
}

// ended is how the turn stopped, as the process side saw it.
type ended struct {
	interrupted bool   // we asked the agent to cancel the turn
	cancelled   bool   // the run's context ended
	exitErr     error  // the agent's exit status
	stderr      string // its last words
	asked       string // the session the run resumed, if it resumed one
	check       string // the agent's login check, for the owner
}

// outcome decides how the run ended. Only the prompt's answer decides it: an
// agent that exits 0 before answering has not succeeded.
func (t *translator) outcome(e ended) adapter.Outcome {
	t.text.Flush()
	o := adapter.Outcome{NativeSessionID: t.session}
	// A turn cancelled before the model answered carries a usage of zeros,
	// which is no usage: nothing was spent, and a usage event would say
	// something was.
	if u := t.doneUsage(); u != nil {
		model := firstNonEmpty(t.model, t.agent.ID)
		// Reasoning is output the model produced and was billed for; the
		// protocol has no field of its own for it, as Claude's report has
		// none.
		usage := v1.Usage{
			Model: model, Input: u.InputTokens, Output: u.OutputTokens + u.ThoughtTokens,
			CacheRead: u.CachedReadTokens, CacheWrite: u.CachedWriteTokens,
		}
		o.Usage = map[string]v1.Usage{model: usage}
		t.emit(v1.Event{At: t.now(), Kind: v1.EventUsage, Usage: &usage})
	}
	fail := func(class, msg string) adapter.Outcome {
		o.State = v1.RunFailed
		o.Error = &v1.RunError{Class: class, Message: msg}
		t.emitErr(class, msg)
		return o
	}
	switch {
	case t.mismatch != "":
		o.State = v1.RunFailed
		o.Error = &v1.RunError{Class: adapter.ClassSessionMismatch, Message: t.mismatchMessage()}
		return o
	case t.fault != nil:
		// An answer that landed before a cancel stands (decision 0025).
		return fail(t.fault.Class, t.fault.Message)
	case t.promptErr != nil:
		f := Failure{}
		if t.agent.Classify != nil {
			f = t.agent.Classify(t.promptErr, t.spec)
		}
		if f.Class == "" {
			f.Class = adapter.ClassHarness
		}
		if f.Message == "" {
			f.Message = fmt.Sprintf("%s failed the turn: %s", t.agent.Name, t.promptErr.Message)
		}
		o.Limit, o.AuthRejected = f.Limit, f.AuthRejected
		return fail(f.Class, f.Message)
	case t.done == nil && (e.interrupted || e.cancelled):
		o.State = v1.RunCancelled
		return o
	case t.done == nil:
		return fail(adapter.ClassHarnessExited, fmt.Sprintf("%s exited before it answered the prompt%s — check that it runs and has a working login: %s", t.agent.Name, exitDetail(e), e.check))
	}
	switch t.done.StopReason {
	case "end_turn":
		// Even after an interrupt: a turn that finished first really
		// succeeded, and reporting it cancelled would throw its answer away.
		o.State = v1.RunSucceeded
		o.FinalText = t.final.String()
		return o
	case "cancelled":
		if e.interrupted || e.cancelled {
			o.State = v1.RunCancelled
			return o
		}
		return fail(adapter.ClassHarness, t.agent.Name+" cancelled the turn by itself — look for the reason in the events before this one")
	case "max_tokens":
		return fail(adapter.ClassHarness, t.agent.Name+" stopped the turn at the model's output limit — the answer is cut short; ask for less in one run")
	case "max_turn_requests":
		return fail(adapter.ClassHarness, t.agent.Name+" stopped the turn at its limit of model requests in one turn — split the work into more runs")
	case "refusal":
		return fail(adapter.ClassHarness, "the model refused to continue the turn, and "+t.agent.Name+" leaves this prompt out of the session from here")
	}
	return fail(adapter.ClassHarness, fmt.Sprintf("%s ended the turn %q, which is not a way ACP v1 ends one", t.agent.Name, t.done.StopReason))
}

func exitDetail(e ended) string {
	var msg string
	if e.exitErr != nil {
		msg += " (" + e.exitErr.Error() + ")"
	}
	if s := strings.TrimSpace(e.stderr); s != "" {
		if i := strings.LastIndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		msg += ": " + s
	}
	return msg
}
