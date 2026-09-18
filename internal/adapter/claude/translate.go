package claude

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

// frame is one line of `claude -p --output-format stream-json`, flattened: the
// stream has a dozen shapes that share a handful of fields, and decoding each
// into its own type would cost more than it would check. Unknown fields and
// unknown types are ignored — a new Claude release adds both routinely.
type frame struct {
	Type            string  `json:"type"`
	Subtype         string  `json:"subtype"`
	SessionID       string  `json:"session_id"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	IsReplay        bool    `json:"isReplay"`

	// system/status, system/compact_boundary, system/api_retry,
	// system/permission_denied
	Status   string `json:"status"`
	ToolName string `json:"tool_name"`

	// stream_event
	Event *streamEvent `json:"event"`

	// assistant and user frames carry a message object; system frames such as
	// permission_denied carry a string under the same key. Decoded per type,
	// so one shape never makes the other unreadable.
	Message json.RawMessage `json:"message"`

	// result
	IsError         bool                  `json:"is_error"`
	Result          string                `json:"result"`
	Errors          []string              `json:"errors"`
	TerminalReason  string                `json:"terminal_reason"`
	APIErrorStatus  *int                  `json:"api_error_status"`
	QueuedTurnCount int                   `json:"queued_turn_count"`
	ModelUsage      map[string]modelUsage `json:"modelUsage"`

	// rate_limit_event
	RateLimitInfo *rateLimitInfo `json:"rate_limit_info"`

	// control_request from Claude; control_response to ours
	RequestID string          `json:"request_id"`
	Request   *controlRequest `json:"request"`
}

type streamEvent struct {
	Type         string `json:"type"`
	ContentBlock *block `json:"content_block"`
	Delta        *struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"delta"`
	Message *struct {
		ID string `json:"id"`
	} `json:"message"`
}

type message struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	// A string for a plain user message, blocks for everything else.
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

type modelUsage struct {
	InputTokens              int64    `json:"inputTokens"`
	OutputTokens             int64    `json:"outputTokens"`
	CacheReadInputTokens     int64    `json:"cacheReadInputTokens"`
	CacheCreationInputTokens int64    `json:"cacheCreationInputTokens"`
	CostUSD                  *float64 `json:"costUSD"`
}

type rateLimitInfo struct {
	Status        string `json:"status"`
	ResetsAt      int64  `json:"resetsAt"`
	RateLimitType string `json:"rateLimitType"`
}

type controlRequest struct {
	Subtype  string `json:"subtype"`
	ToolName string `json:"tool_name"`
}

// textFlush bounds how long streamed text waits before it becomes an event.
// Claude sends a delta per few tokens; one event each would be thousands per
// answer, and one per finished block would leave a long answer invisible — and
// the inactivity watchdog blind — until it ends.
const textFlush = time.Second

// textFlushBytes flushes a fast stream sooner, keeping each event small.
const textFlushBytes = 4 << 10

// translator turns Claude's stream into protocol events and, from the result,
// an Outcome. It holds no process and does no I/O, so every rule in it is
// tested line by line against recorded streams.
type translator struct {
	emit    func(v1.Event)
	now     func() time.Time
	session string // the id Claude was told to use

	pending      strings.Builder
	pendingKind  v1.EventKind
	pendingSince time.Time
	// Messages whose text arrived as partial deltas. Their complete `assistant`
	// frame repeats that text, so it is skipped; a message with no deltas (an
	// older Claude, a synthetic error message) is emitted whole.
	streamed map[string]bool
	current  string // the top-level message now streaming

	result  *frame
	replays int
	// takenAfter: Claude took one of our frames after the last result, so that
	// result answered a turn Claude had more input for.
	takenAfter bool
	limit      *adapter.Limit
	apiRetries int
	mismatch   string
}

func newTranslator(session string, emit func(v1.Event)) *translator {
	return &translator{emit: emit, now: time.Now, session: session, streamed: map[string]bool{}}
}

// reaction is what the turn must do about a line besides emitting its events.
type reaction struct {
	result   bool   // a result arrived; the turn may be over
	replayed bool   // Claude consumed one of our user frames
	mismatch bool   // Claude is running a different session: stop it
	deny     string // Claude asked permission; answer this request id with a denial
}

// line handles one line of the stream.
func (t *translator) line(raw []byte) reaction {
	var f frame
	if err := json.Unmarshal(raw, &f); err != nil {
		t.flush()
		t.streamError(fmt.Sprintf("an unreadable line from claude was skipped: %v", err))
		return reaction{}
	}
	var r reaction
	// Only init and result name the conversation itself; checking every frame
	// would trust that no other frame ever carries a subsidiary id.
	sessionFrame := (f.Type == "system" && f.Subtype == "init") || f.Type == "result"
	if sessionFrame && f.SessionID != "" && f.SessionID != t.session && t.mismatch == "" {
		t.mismatch = f.SessionID
		t.flush()
		t.emitErr(adapter.ClassSessionMismatch, t.mismatchMessage())
		r.mismatch = true
	}
	switch f.Type {
	case "stream_event":
		t.streamEvent(&f)
	case "assistant":
		t.assistant(&f)
	case "user":
		if f.IsReplay {
			r.replayed = true
			t.replays++
			t.takenAfter = t.result != nil
			return r
		}
		t.toolResults(&f)
	case "system":
		t.system(&f)
	case "rate_limit_event":
		if i := f.RateLimitInfo; i != nil && i.Status == "allowed" {
			t.limit = nil
		}
		if i := f.RateLimitInfo; i != nil && i.Status == "rejected" {
			t.limit = &adapter.Limit{Window: i.RateLimitType}
			if i.ResetsAt > 0 {
				t.limit.ResetAt = time.Unix(i.ResetsAt, 0).UTC()
			}
			t.status("usage limit reached")
		}
	case "control_request":
		// Only a permission prompt reaches here: nobody is there to answer it,
		// and the owner's permission mode already decided what may run (0015).
		if f.Request != nil && f.Request.Subtype == "can_use_tool" {
			r.deny = f.RequestID
		}
	case "result":
		t.flush()
		t.result = &f
		t.takenAfter = false
		if !f.IsError {
			// A limit Claude got past (overage, a retry) is not this result's.
			t.limit = nil
		}
		r.result = true
	}
	return r
}

func (t *translator) streamEvent(f *frame) {
	e := f.Event
	if e == nil || f.ParentToolUseID != nil {
		// A subagent's partial output is its own conversation; its finished
		// messages still arrive as assistant frames.
		return
	}
	switch e.Type {
	case "message_start":
		if e.Message != nil {
			t.current = e.Message.ID
		}
	case "content_block_start":
		if e.ContentBlock != nil && e.ContentBlock.Type == "thinking" {
			// Claude does not stream thinking text by default, only that it is
			// thinking — which is exactly what the watchdog needs to see.
			t.flush()
			t.status("thinking")
		}
	case "content_block_delta":
		if e.Delta == nil {
			return
		}
		switch e.Delta.Type {
		case "text_delta":
			t.streamed[t.current] = true
			t.add(v1.EventText, e.Delta.Text)
		case "thinking_delta":
			if e.Delta.Thinking != "" {
				t.streamed[t.current] = true
				t.add(v1.EventThinking, e.Delta.Thinking)
			}
		}
	case "content_block_stop", "message_stop":
		t.flush()
	}
}

func (f *frame) message() *message {
	var m message
	if len(f.Message) == 0 || json.Unmarshal(f.Message, &m) != nil {
		return nil
	}
	return &m
}

func (t *translator) assistant(f *frame) {
	m := f.message()
	if m == nil {
		return
	}
	t.flush()
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	streamed := t.streamed[m.ID] && f.ParentToolUseID == nil
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if !streamed && b.Text != "" {
				t.emit(v1.Event{At: t.now(), Kind: v1.EventText, Text: b.Text})
			}
		case "thinking":
			if !streamed && b.Thinking != "" {
				t.emit(v1.Event{At: t.now(), Kind: v1.EventThinking, Text: b.Thinking})
			}
		case "tool_use", "server_tool_use":
			in, cut := capText(string(b.Input))
			t.emit(v1.Event{At: t.now(), Kind: v1.EventToolCall, Tool: &v1.ToolEvent{ID: b.ID, Name: b.Name, Input: in, Truncated: cut}})
		}
	}
}

func (t *translator) toolResults(f *frame) {
	m := f.message()
	if m == nil {
		return
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return // a plain-text user message: an echo of the brief, nothing new
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		out, cut := capText(toolOutput(b.Content))
		t.emit(v1.Event{At: t.now(), Kind: v1.EventToolResult, Tool: &v1.ToolEvent{ID: b.ToolUseID, Output: out, Truncated: cut}})
	}
}

func (t *translator) system(f *frame) {
	switch f.Subtype {
	case "init":
		t.status("started")
	case "compact_boundary":
		t.flush()
		t.status("compacted")
	case "status":
		if f.Status == "compacting" {
			t.flush()
			t.status("compacting")
		}
	case "permission_denied":
		// Claude answers its own permission prompts in -p mode; the tool result
		// carries the reason, and this says plainly that the mode was the cause.
		t.flush()
		t.status("permission denied: " + f.ToolName)
	case "api_retry":
		// A rate limit, not a usage limit: Claude retries by itself and the run
		// never waits on it (DOMAIN.md, Usage limit).
		t.apiRetries++
		t.flush()
		t.status("api_retry")
	}
}

// toolOutput flattens a tool_result's content, which is a string or a list of
// blocks. Non-text blocks are named rather than carried: an image in an event
// would dwarf the cap and tell a hub nothing.
func toolOutput(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		} else {
			parts = append(parts, "["+b.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}

// capText holds a tool payload to MaxToolOutputBytes without splitting a rune.
func capText(s string) (string, bool) {
	if len(s) <= v1.MaxToolOutputBytes {
		return s, false
	}
	cut := v1.MaxToolOutputBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

func (t *translator) add(kind v1.EventKind, s string) {
	if s == "" {
		return
	}
	if t.pending.Len() > 0 && t.pendingKind != kind {
		t.flush()
	}
	if t.pending.Len() == 0 {
		t.pendingKind, t.pendingSince = kind, t.now()
	}
	t.pending.WriteString(s)
	if t.pending.Len() >= textFlushBytes {
		t.flush()
	}
}

// tick flushes text that has waited textFlush. It runs on a timer, not on the
// next delta: a Claude that stalls mid-sentence must not hold back what it has
// already said.
func (t *translator) tick() {
	if t.pending.Len() > 0 && t.now().Sub(t.pendingSince) >= textFlush {
		t.flush()
	}
}

func (t *translator) flush() {
	if t.pending.Len() == 0 {
		return
	}
	t.emit(v1.Event{At: t.now(), Kind: t.pendingKind, Text: t.pending.String()})
	t.pending.Reset()
}

func (t *translator) status(s string) {
	t.emit(v1.Event{At: t.now(), Kind: v1.EventStatus, Status: s})
}

func (t *translator) emitErr(class, msg string) {
	t.emit(v1.Event{At: t.now(), Kind: v1.EventError, Error: &v1.RunError{Class: class, Message: msg}})
}

func (t *translator) streamError(msg string) { t.emitErr(adapter.ClassStream, msg) }

func (t *translator) mismatchMessage() string {
	return fmt.Sprintf("claude ran session %s instead of %s — the resume did not take and the conversation's context is gone; start a new session or check the transcript is still on this runner", t.mismatch, t.session)
}

// ended is how the turn stopped, as the process side saw it.
type ended struct {
	interrupted bool   // we sent an interrupt
	cancelled   bool   // the run's context ended
	exitErr     error  // the process's exit status
	stderr      string // its last words
	// final: the result in hand was the run's last — every frame we sent had
	// been taken and answered, or an interrupt ended the turn.
	final bool
}

// outcome decides how the turn ended. Only a result decides success: exit 0
// with no result is a failure, and a result that says `success` may carry
// `is_error` — prompt_too_long arrives exactly that way.
func (t *translator) outcome(e ended) adapter.Outcome {
	t.flush()
	o := adapter.Outcome{NativeSessionID: t.session, APIRetries: t.apiRetries}
	r := t.result
	if r != nil {
		o.FinalText = r.Result
		o.Usage = usage(r.ModelUsage)
		for _, u := range sortedUsage(o.Usage) {
			t.emit(v1.Event{At: t.now(), Kind: v1.EventUsage, Usage: &u})
		}
	}
	fail := func(class, msg string) adapter.Outcome {
		o.State = v1.RunFailed
		o.FinalText = ""
		o.Error = &v1.RunError{Class: class, Message: msg}
		t.emitErr(class, msg)
		return o
	}
	cancelled := func() adapter.Outcome {
		o.State = v1.RunCancelled
		o.FinalText = ""
		return o
	}
	success := r != nil && !r.IsError && r.Subtype == "success"
	// The order is the rule. Only a final result decides the run by what it
	// says; a result that was not final — Claude had taken more input, or was
	// still to — decides nothing, whether it says success or error.
	switch {
	case t.mismatch != "":
		// First: an interrupted or even successful turn in the wrong session
		// did its work without the conversation it was meant to continue.
		o.State = v1.RunFailed
		o.FinalText = ""
		o.Error = &v1.RunError{Class: adapter.ClassSessionMismatch, Message: t.mismatchMessage()}
		return o
	case r == nil && (e.interrupted || e.cancelled):
		return cancelled()
	case r == nil:
		return fail(adapter.ClassHarnessExited, exitedMessage(e))
	case e.final && success:
		// Even after an interrupt: a turn that finished first really
		// succeeded, and reporting it cancelled would throw its answer away.
		o.State = v1.RunSucceeded
		return o
	case e.interrupted:
		return cancelled()
	case !e.final && e.cancelled:
		return cancelled()
	case !e.final && (t.takenAfter || r.QueuedTurnCount > 0):
		return fail(adapter.ClassHarnessExited, "claude exited after taking a steer and before answering it"+exitDetail(e)+" — send the steer again as a new run in the same session")
	case !e.final && success:
		// Claude answered and left without reading a steer written to it.
		return fail(adapter.ClassHarnessExited, "claude exited before reading a steer"+exitDetail(e)+" — send the steer again as a new run in the same session")
	case success:
		o.State = v1.RunSucceeded
		return o
	}
	// An error result that was final, or one Claude gave before it read any
	// input at all (a resume with no transcript): what it says is the outcome.
	msg := resultMessage(r)
	switch {
	case r.TerminalReason == "prompt_too_long" || strings.HasPrefix(r.Result, "Prompt is too long"):
		return fail(adapter.ClassPromptTooLong, msg)
	case t.limit != nil || isUsageLimit(r):
		o.Limit = t.limit
		if o.Limit == nil {
			o.Limit = &adapter.Limit{}
		}
		if o.Limit.ResetAt.IsZero() {
			o.Limit.ResetAt = legacyReset(r.Result)
		}
		return fail(adapter.ClassUsageLimit, msg)
	case slices.ContainsFunc(r.Errors, func(s string) bool { return strings.HasPrefix(s, "No conversation found") }):
		return fail(adapter.ClassSessionNotFound, msg+" — the session's transcript is not on this runner; start a new session")
	}
	return fail(adapter.ClassHarness, msg)
}

func exitedMessage(e ended) string {
	return "claude exited without reporting a result" + exitDetail(e) + " — check that it runs and is logged in: `claude -p hello`"
}

func exitDetail(e ended) string {
	var msg string
	if e.exitErr != nil {
		msg += " (" + e.exitErr.Error() + ")"
	}
	if s := strings.TrimSpace(e.stderr); s != "" {
		msg += ": " + lastLine(s)
	}
	return msg
}

func lastLine(s string) string {
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func resultMessage(r *frame) string {
	if r.Result != "" {
		return r.Result
	}
	if len(r.Errors) > 0 {
		return strings.Join(r.Errors, "; ")
	}
	return "claude reported " + r.Subtype
}

// Claude has reported a usage limit three ways across releases: a
// rate_limit_event (handled by the caller), a 429, and before either of those
// the text "Claude AI usage limit reached|<unix reset>".
var legacyLimit = regexp.MustCompile(`usage limit reached\|(\d+)`)

func isUsageLimit(r *frame) bool {
	return (r.APIErrorStatus != nil && *r.APIErrorStatus == 429) || legacyLimit.MatchString(r.Result)
}

func legacyReset(s string) time.Time {
	m := legacyLimit.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0).UTC()
}

// usage maps modelUsage, which is cumulative over the process — so the last
// result's is the whole run's, steered follow-up turns included.
func usage(m map[string]modelUsage) map[string]v1.Usage {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]v1.Usage, len(m))
	for model, u := range m {
		out[model] = v1.Usage{
			Model:      model,
			Input:      u.InputTokens,
			Output:     u.OutputTokens,
			CacheRead:  u.CacheReadInputTokens,
			CacheWrite: u.CacheCreationInputTokens,
			CostUSD:    u.CostUSD,
		}
	}
	return out
}

func sortedUsage(m map[string]v1.Usage) []v1.Usage {
	out := make([]v1.Usage, 0, len(m))
	for _, u := range m {
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b v1.Usage) int { return strings.Compare(a.Model, b.Model) })
	return out
}
