package v1

import "time"

// EventKind is the closed set of normalised events. Every adapter translates
// its harness's stream into these, so a hub never parses a harness format.
type EventKind string

const (
	EventText       EventKind = "text"
	EventThinking   EventKind = "thinking"
	EventToolCall   EventKind = "tool_call"
	EventToolResult EventKind = "tool_result"
	EventStatus     EventKind = "status"
	EventUsage      EventKind = "usage"
	EventError      EventKind = "error"
)

// EventKinds lists the closed set.
func EventKinds() []EventKind {
	return []EventKind{EventText, EventThinking, EventToolCall, EventToolResult, EventStatus, EventUsage, EventError}
}

// MaxToolOutputBytes caps tool input/output carried in one event. A single
// `cat` of a lockfile would otherwise dominate the stream a hub stores.
const MaxToolOutputBytes = 8 << 10

// Event is one normalised thing that happened in a run. Seq is per run,
// starts at 1, and is unique: (run, seq) is the idempotency key.
type Event struct {
	Seq    int64      `json:"seq" doc:"The event's place in its run: 1 for the first, then 2, 3, and so on with no gaps. (run, seq) identifies an event; a resent one keeps its seq."`
	At     time.Time  `json:"at" doc:"When it happened on the runner."`
	Kind   EventKind  `json:"kind" enum:"text,thinking,tool_call,tool_result,status,usage,error" doc:"text: the harness said something (text). thinking: its reasoning (text). tool_call: it called a tool (tool.id, tool.name, tool.input). tool_result: the tool answered (tool.id, tool.output). status: a phase of the run, for people (status, text). usage: tokens one model used (usage). error: something went wrong, whether or not the run carries on (error)."`
	Text   string     `json:"text,omitempty" doc:"The words of a text, thinking, status or error event. At most 1 MiB."`
	Tool   *ToolEvent `json:"tool,omitempty" doc:"For tool_call and tool_result."`
	Status string     `json:"status,omitempty" doc:"For a status event, a short label of the phase, such as account, waiting, cancelling or steered. For display: not a closed set, and not to be parsed."`
	Usage  *Usage     `json:"usage,omitempty" doc:"For a usage event."`
	Error  *RunError  `json:"error,omitempty" doc:"For an error event."`
}

// ToolEvent is a tool call or its result, joined by ID.
type ToolEvent struct {
	ID        string `json:"id" doc:"The harness's id for the call, which joins a tool_result to its tool_call."`
	Name      string `json:"name,omitempty" doc:"The tool, as the harness names it, such as Read or shell."`
	Input     string `json:"input,omitempty" doc:"The call's input, usually JSON, at most 8 KiB."`
	Output    string `json:"output,omitempty" doc:"The tool's output, at most 8 KiB."`
	Truncated bool   `json:"truncated,omitempty" doc:"Input or output was cut to fit 8 KiB."`
}

// Usage is tokens for one model. Cost is the harness's own estimate where it
// gives one (Claude does, Codex does not) — never computed by YAD.
type Usage struct {
	Model      string   `json:"model,omitempty" doc:"The model the tokens were spent on, as the harness reported it."`
	Input      int64    `json:"input" doc:"Input tokens not read from a cache."`
	Output     int64    `json:"output" doc:"Output tokens."`
	CacheRead  int64    `json:"cache_read,omitempty" doc:"Input tokens read from the prompt cache."`
	CacheWrite int64    `json:"cache_write,omitempty" doc:"Input tokens written to the prompt cache."`
	CostUSD    *float64 `json:"cost_usd,omitempty" doc:"The harness's own cost estimate in US dollars, where it gives one (Claude Code does, Codex does not). Never computed by yad, and absent rather than zero when unknown."`
}

// RunError classifies a failure so a hub can act on it without parsing text.
type RunError struct {
	Class   string `json:"class" doc:"What kind of failure, for a program to act on: refused, session_closed, resume_rejected, usage_limit, prompt_too_long, source_refused, setup_failed, inactivity_timeout and others. Not a closed set: a class you do not know is a failure to show, not to parse."`
	Message string `json:"message" doc:"What happened, for a person. At most 1 MiB."`
}

// EventBatch is one upload from the spool.
type EventBatch struct {
	Events []Event `json:"events" doc:"Events of this run in seq order, at most 100 from a yad runner. A batch may repeat events already sent."`
}

// EventAck is authoritative: the runner resends everything after AckedThrough.
type EventAck struct {
	AckedThrough int64 `json:"acked_through" doc:"The highest seq up to which the hub holds every event of the run with no gap: 0 when it holds none, or none from 1 on. The runner resends everything after it, so never answer a seq you have not stored contiguously."`
}
