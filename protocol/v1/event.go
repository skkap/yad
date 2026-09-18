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
	Seq    int64      `json:"seq"`
	At     time.Time  `json:"at"`
	Kind   EventKind  `json:"kind" enum:"text,thinking,tool_call,tool_result,status,usage,error"`
	Text   string     `json:"text,omitempty"`
	Tool   *ToolEvent `json:"tool,omitempty"`
	Status string     `json:"status,omitempty"`
	Usage  *Usage     `json:"usage,omitempty"`
	Error  *RunError  `json:"error,omitempty"`
}

// ToolEvent is a tool call or its result, joined by ID.
type ToolEvent struct {
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	Input     string `json:"input,omitempty"`
	Output    string `json:"output,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Usage is tokens for one model. Cost is the harness's own estimate where it
// gives one (Claude does, Codex does not) — never computed by YAD.
type Usage struct {
	Model      string   `json:"model,omitempty"`
	Input      int64    `json:"input"`
	Output     int64    `json:"output"`
	CacheRead  int64    `json:"cache_read,omitempty"`
	CacheWrite int64    `json:"cache_write,omitempty"`
	CostUSD    *float64 `json:"cost_usd,omitempty"`
}

// RunError classifies a failure so a hub can act on it without parsing text.
type RunError struct {
	Class   string `json:"class"`
	Message string `json:"message"`
}

// EventBatch is one upload from the spool.
type EventBatch struct {
	Events []Event `json:"events"`
}

// EventAck is authoritative: the runner resends everything after AckedThrough.
type EventAck struct {
	AckedThrough int64 `json:"acked_through"`
}
