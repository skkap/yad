package codex

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

// The shapes read from the app-server, as far as the adapter reads them. The
// schema the installed version generates says what else is there
// (schema.go); unknown fields and notifications are ignored, because a new
// Codex release adds both routinely.

type thread struct {
	ID string `json:"id"`
}

// threadResult is thread/start's and thread/resume's response.
type threadResult struct {
	Thread thread `json:"thread"`
	Model  string `json:"model"`
}

type turnInfo struct {
	ID     string     `json:"id"`
	Status string     `json:"status"` // completed | interrupted | failed | inProgress
	Error  *turnError `json:"error"`
}

type turnError struct {
	Message string `json:"message"`
	// A string for most kinds, an object for the HTTP ones; only the strings
	// decide anything here.
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo"`
	AdditionalDetails string          `json:"additionalDetails"`
}

func (e *turnError) kind() string {
	var s string
	if e == nil || json.Unmarshal(e.CodexErrorInfo, &s) != nil {
		return ""
	}
	return s
}

// text is the error's message. Codex passes an API error through as the
// JSON body it got; the message inside is the part a person can read.
func (e *turnError) text() string {
	msg := strings.TrimSpace(e.Message)
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(msg), &body) == nil && body.Error.Message != "" {
		msg = body.Error.Message
	}
	if d := strings.TrimSpace(e.AdditionalDetails); d != "" {
		msg += " (" + d + ")"
	}
	if msg == "" {
		msg = "codex reported the turn failed without saying why"
	}
	return msg
}

// turnParams is every turn-scoped notification's common part.
type turnParams struct {
	ThreadID string    `json:"threadId"`
	TurnID   string    `json:"turnId"`
	Turn     *turnInfo `json:"turn"`
}

func (p turnParams) turnID() string {
	if p.Turn != nil {
		return p.Turn.ID
	}
	return p.TurnID
}

type item struct {
	Type  string `json:"type"`
	ID    string `json:"id"`
	Text  string `json:"text"`
	Phase string `json:"phase"`

	// commandExecution
	Command          string  `json:"command"`
	AggregatedOutput *string `json:"aggregatedOutput"`
	ExitCode         *int    `json:"exitCode"`
	Status           string  `json:"status"`

	// fileChange
	Changes []struct {
		Path string `json:"path"`
		Kind struct {
			Type string `json:"type"`
		} `json:"kind"`
		Diff string `json:"diff"`
	} `json:"changes"`

	// mcpToolCall, dynamicToolCall, collabAgentToolCall
	Server    string          `json:"server"`
	Tool      json.RawMessage `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    json.RawMessage `json:"result"`
	Error     *struct {
		Message string `json:"message"`
	} `json:"error"`
	ContentItems []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"contentItems"`
	Prompt string `json:"prompt"`

	// webSearch, imageView
	Query string `json:"query"`
	Path  string `json:"path"`
}

type itemParams struct {
	Item item `json:"item"`
}

type deltaParams struct {
	ItemID string `json:"itemId"`
	Delta  string `json:"delta"`
}

type tokenUsage struct {
	Total breakdown `json:"total"`
	Last  breakdown `json:"last"`
}

type breakdown struct {
	InputTokens           int64 `json:"inputTokens"`
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
}

func (b breakdown) minus(o breakdown) breakdown {
	return breakdown{
		InputTokens:           b.InputTokens - o.InputTokens,
		CachedInputTokens:     b.CachedInputTokens - o.CachedInputTokens,
		CacheWriteInputTokens: b.CacheWriteInputTokens - o.CacheWriteInputTokens,
		OutputTokens:          b.OutputTokens - o.OutputTokens,
	}
}

func (b breakdown) plus(o breakdown) breakdown {
	return breakdown{
		InputTokens:           b.InputTokens + o.InputTokens,
		CachedInputTokens:     b.CachedInputTokens + o.CachedInputTokens,
		CacheWriteInputTokens: b.CacheWriteInputTokens + o.CacheWriteInputTokens,
		OutputTokens:          b.OutputTokens + o.OutputTokens,
	}
}

// rateLimits is a RateLimitSnapshot: the account's windows as Codex last
// reported them.
type rateLimits struct {
	Primary              *window `json:"primary"`
	Secondary            *window `json:"secondary"`
	RateLimitReachedType *string `json:"rateLimitReachedType"`
}

type window struct {
	UsedPercent int    `json:"usedPercent"`
	ResetsAt    *int64 `json:"resetsAt"`
}

type errorParams struct {
	Error     turnError `json:"error"`
	WillRetry bool      `json:"willRetry"`
}

// translator turns the notifications of the run's own turn into protocol
// events and, once the turn is over, an Outcome. It holds no process and does
// no I/O; the turn decides which notifications are the run's (codex.go).
type translator struct {
	emit func(v1.Event)
	now  func() time.Time
	text adapter.Text

	thread string
	model  string

	// streamed: messages and reasoning whose text arrived as deltas; the
	// finished item repeats it, so it is not emitted twice.
	streamed map[string]bool
	// called: tool items announced by item/started, so item/completed only
	// adds the result.
	called map[string]bool

	final      string // the turn's answer: its last agent message
	finalPhase bool   // final was marked final_answer, which a later commentary message does not replace

	// Usage: the first snapshot of the turn counts its `last`, every later
	// one the growth of `total`. A resume replays the thread's earlier total
	// before the turn starts, and that is not this run's to report.
	used     breakdown
	total    breakdown
	counted  bool
	limits   *rateLimits
	retries  int
	done     *turnInfo // turn/completed, for the run's turn
	mismatch string    // the thread codex resumed, when it was not the one asked for
	fault    *v1.RunError
}

func newTranslator(emit func(v1.Event)) *translator {
	t := &translator{emit: emit, now: time.Now, streamed: map[string]bool{}, called: map[string]bool{}}
	t.text = adapter.Text{Emit: emit, Now: func() time.Time { return t.now() }}
	return t
}

// notification handles one notification the turn has decided is this run's.
func (t *translator) notification(method string, params json.RawMessage) {
	switch method {
	case "item/agentMessage/delta":
		var p deltaParams
		if json.Unmarshal(params, &p) == nil {
			t.streamed[p.ItemID] = true
			t.text.Add(v1.EventText, p.Delta)
		}
	case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		var p deltaParams
		if json.Unmarshal(params, &p) == nil {
			t.streamed[p.ItemID] = true
			t.text.Add(v1.EventThinking, p.Delta)
		}
	case "item/started":
		var p itemParams
		if json.Unmarshal(params, &p) == nil {
			t.started(p.Item)
		}
	case "item/completed":
		var p itemParams
		if json.Unmarshal(params, &p) == nil {
			t.completed(p.Item)
		}
	case "thread/tokenUsage/updated":
		var p struct {
			TokenUsage tokenUsage `json:"tokenUsage"`
		}
		if json.Unmarshal(params, &p) == nil {
			t.usage(p.TokenUsage)
		}
	case "error":
		var p errorParams
		if json.Unmarshal(params, &p) == nil && p.WillRetry {
			// Transient: Codex retries by itself, and the run never waits on
			// it (DOMAIN.md, Usage limit).
			t.retries++
			t.text.Flush()
			t.status("api_retry")
		}
	case "thread/compacted":
		t.text.Flush()
		t.status("compacted")
	case "model/rerouted":
		var p struct {
			ToModel string `json:"toModel"`
		}
		if json.Unmarshal(params, &p) == nil && p.ToModel != "" {
			t.model = p.ToModel
			t.status("model rerouted to " + p.ToModel)
		}
	case "turn/completed":
		var p turnParams
		if json.Unmarshal(params, &p) == nil && p.Turn != nil {
			t.text.Flush()
			t.done = p.Turn
		}
	}
}

// rateLimits records the account's windows. Not scoped to a thread or a turn:
// it is the account's, and the latest word on it is what a limit is read
// from. The schema calls an update sparse — clients merge what it carries
// into what they have — so a window it leaves out keeps its last value.
func (t *translator) rateLimits(params json.RawMessage) {
	var p struct {
		RateLimits *rateLimits `json:"rateLimits"`
	}
	if json.Unmarshal(params, &p) != nil || p.RateLimits == nil {
		return
	}
	if t.limits == nil {
		t.limits = &rateLimits{}
	}
	u := p.RateLimits
	if u.Primary != nil {
		t.limits.Primary = u.Primary
	}
	if u.Secondary != nil {
		t.limits.Secondary = u.Secondary
	}
	if u.RateLimitReachedType != nil {
		t.limits.RateLimitReachedType = u.RateLimitReachedType
	}
}

func (t *translator) started(it item) {
	switch it.Type {
	case "reasoning":
		// Codex does not stream reasoning unless it summarises; that it is
		// thinking is what the inactivity watchdog needs to see.
		t.text.Flush()
		t.status("thinking")
	case "agentMessage", "userMessage":
	default:
		if name, input, ok := toolCall(it); ok {
			t.text.Flush()
			t.called[it.ID] = true
			in, cut := adapter.CapTool(input)
			t.emit(v1.Event{At: t.now(), Kind: v1.EventToolCall, Tool: &v1.ToolEvent{ID: it.ID, Name: name, Input: in, Truncated: cut}})
		}
	}
}

func (t *translator) completed(it item) {
	switch it.Type {
	case "agentMessage":
		t.text.Flush()
		if !t.streamed[it.ID] && it.Text != "" {
			t.emit(v1.Event{At: t.now(), Kind: v1.EventText, Text: it.Text})
		}
		// The answer is the last message, unless one marked final came
		// before it: commentary after the answer is not the answer.
		if it.Phase == "final_answer" || !t.finalPhase {
			t.final, t.finalPhase = it.Text, it.Phase == "final_answer"
		}
	case "reasoning":
		t.text.Flush()
	case "userMessage":
	default:
		name, input, ok := toolCall(it)
		if !ok {
			return
		}
		t.text.Flush()
		if !t.called[it.ID] {
			in, cut := adapter.CapTool(input)
			t.emit(v1.Event{At: t.now(), Kind: v1.EventToolCall, Tool: &v1.ToolEvent{ID: it.ID, Name: name, Input: in, Truncated: cut}})
		}
		delete(t.called, it.ID)
		out, cut := adapter.CapTool(toolOutput(it))
		t.emit(v1.Event{At: t.now(), Kind: v1.EventToolResult, Tool: &v1.ToolEvent{ID: it.ID, Output: out, Truncated: cut}})
	}
}

// toolCall names a tool item and its input the way a hub reads Claude's: the
// command a shell ran, the files a patch touched, an MCP tool's arguments.
func toolCall(it item) (name, input string, ok bool) {
	switch it.Type {
	case "commandExecution":
		return "shell", it.Command, true
	case "fileChange":
		lines := make([]string, 0, len(it.Changes))
		for _, c := range it.Changes {
			lines = append(lines, c.Kind.Type+" "+c.Path)
		}
		return "apply_patch", strings.Join(lines, "\n"), true
	case "mcpToolCall":
		return "mcp__" + it.Server + "__" + rawString(it.Tool), string(it.Arguments), true
	case "dynamicToolCall":
		return rawString(it.Tool), string(it.Arguments), true
	case "collabAgentToolCall":
		return "agent:" + rawString(it.Tool), it.Prompt, true
	case "webSearch":
		return "web_search", it.Query, true
	case "imageView":
		return "view_image", it.Path, true
	}
	return "", "", false
}

func toolOutput(it item) string {
	switch it.Type {
	case "commandExecution":
		var out string
		if it.AggregatedOutput != nil {
			out = *it.AggregatedOutput
		}
		switch {
		case it.Status == "declined":
			out += "[declined: the owner's approval policy did not allow it]"
		case it.ExitCode != nil && *it.ExitCode != 0:
			out += fmt.Sprintf("[exit %d]", *it.ExitCode)
		}
		return out
	case "fileChange":
		parts := make([]string, 0, len(it.Changes)+1)
		for _, c := range it.Changes {
			parts = append(parts, c.Diff)
		}
		if it.Status != "completed" {
			parts = append(parts, "["+it.Status+"]")
		}
		return strings.Join(parts, "\n")
	case "mcpToolCall":
		if it.Error != nil {
			return it.Error.Message
		}
		return mcpText(it.Result)
	case "dynamicToolCall":
		parts := make([]string, 0, len(it.ContentItems))
		for _, c := range it.ContentItems {
			if c.Type == "inputText" {
				parts = append(parts, c.Text)
			} else {
				parts = append(parts, "["+c.Type+"]")
			}
		}
		return strings.Join(parts, "\n")
	}
	return it.Status
}

// mcpText flattens an MCP result's content. Non-text blocks are named rather
// than carried: an image in an event would dwarf the cap and tell a hub
// nothing.
func mcpText(raw json.RawMessage) string {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return ""
	}
	parts := make([]string, 0, len(r.Content))
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		} else {
			parts = append(parts, "["+c.Type+"]")
		}
	}
	return strings.Join(parts, "\n")
}

func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// usage follows Multica's lesson: the first snapshot of the run's turn counts
// its `last` — one model call — and every later one the growth of `total`.
// Neither a replayed total from before the turn nor a repeated snapshot is
// counted twice.
func (t *translator) usage(u tokenUsage) {
	if t.counted {
		t.used = t.used.plus(u.Total.minus(t.total))
	} else {
		t.used = u.Last
	}
	t.total, t.counted = u.Total, true
}

func (t *translator) status(s string) {
	t.emit(v1.Event{At: t.now(), Kind: v1.EventStatus, Status: s})
}

func (t *translator) emitErr(class, msg string) {
	t.emit(v1.Event{At: t.now(), Kind: v1.EventError, Error: &v1.RunError{Class: class, Message: msg}})
}

// fail records that the conversation with Codex failed before its turn could
// decide the run — a refused handshake, a resume of nothing, no answer. The
// first failure is the one reported.
func (t *translator) fail(class, msg string) {
	if t.fault == nil {
		t.fault = &v1.RunError{Class: class, Message: msg}
	}
}

func (t *translator) mismatchMessage(asked string) string {
	return fmt.Sprintf("codex resumed thread %s instead of %s — the conversation's context is not the session's; start a new session or check the thread is still on this runner", t.mismatch, asked)
}

// ended is how the turn stopped, as the process side saw it.
type ended struct {
	interrupted bool   // we asked codex to interrupt the turn
	cancelled   bool   // the run's context ended
	exitErr     error  // the app-server's exit status
	stderr      string // its last words
	asked       string // the thread id the run resumed, if it resumed one
	check       string // the login check for the owner, in the run's account home
}

// outcome decides how the run ended. Only the turn's own completion decides
// it (as decision 0021 has it for Claude): an app-server that exits 0 before
// turn/completed has not succeeded.
func (t *translator) outcome(e ended) adapter.Outcome {
	t.text.Flush()
	o := adapter.Outcome{NativeSessionID: t.thread, APIRetries: t.retries, Windows: t.windows()}
	if t.counted {
		model := t.model
		if model == "" {
			model = "codex"
		}
		u := v1.Usage{
			Model: model,
			// Codex counts cached input inside input; the protocol, as
			// Claude reports it, keeps them apart.
			Input:      t.used.InputTokens - t.used.CachedInputTokens,
			Output:     t.used.OutputTokens,
			CacheRead:  t.used.CachedInputTokens,
			CacheWrite: t.used.CacheWriteInputTokens,
		}
		o.Usage = map[string]v1.Usage{model: u}
		t.emit(v1.Event{At: t.now(), Kind: v1.EventUsage, Usage: &u})
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
		o.Error = &v1.RunError{Class: adapter.ClassSessionMismatch, Message: t.mismatchMessage(e.asked)}
		return o
	case t.fault != nil:
		// An answer that landed before a cancel stands (decision 0025): a
		// resume of nothing reported as cancelled would hide what the hub
		// must act on.
		return fail(t.fault.Class, t.fault.Message)
	case t.done == nil && (e.interrupted || e.cancelled):
		o.State = v1.RunCancelled
		return o
	case t.done == nil:
		return fail(adapter.ClassHarnessExited, "codex app-server exited before the turn completed"+exitDetail(e)+" — check that it runs and is logged in: "+e.loginCheck())
	}
	switch t.done.Status {
	case "completed":
		// Even after an interrupt: a turn that finished first really
		// succeeded, and reporting it cancelled would throw its answer away.
		o.State = v1.RunSucceeded
		o.FinalText = t.final
		return o
	case "interrupted":
		if e.interrupted || e.cancelled {
			o.State = v1.RunCancelled
			return o
		}
		return fail(adapter.ClassHarness, "codex interrupted the turn by itself — look for the reason in the events before this one")
	}
	te := t.done.Error
	if te == nil {
		te = &turnError{}
	}
	msg := te.text()
	switch te.kind() {
	case "contextWindowExceeded":
		return fail(adapter.ClassPromptTooLong, msg)
	case "usageLimitExceeded":
		o.Limit = t.limit()
		return fail(adapter.ClassUsageLimit, msg)
	}
	if t.done.Status != "failed" {
		return fail(adapter.ClassHarness, fmt.Sprintf("codex ended the turn %q, which is not a way a turn ends — %s", t.done.Status, msg))
	}
	return fail(adapter.ClassHarness, msg)
}

// limit reads the exhausted window from the account's latest snapshot: the
// one at 100%, or failing that the fullest. Codex names them primary and
// secondary; their durations vary by plan, so the names are kept.
func (t *translator) limit() *adapter.Limit {
	l := &adapter.Limit{}
	if t.limits == nil {
		return l
	}
	type named struct {
		name string
		w    *window
	}
	ws := []named{{"primary", t.limits.Primary}, {"secondary", t.limits.Secondary}}
	ws = slices.DeleteFunc(ws, func(n named) bool { return n.w == nil })
	if len(ws) == 0 {
		return l
	}
	best := slices.MaxFunc(ws, func(a, b named) int {
		if (a.w.UsedPercent >= 100) != (b.w.UsedPercent >= 100) {
			if a.w.UsedPercent >= 100 {
				return 1
			}
			return -1
		}
		if a.w.UsedPercent != b.w.UsedPercent {
			return a.w.UsedPercent - b.w.UsedPercent
		}
		// Both full: the run waits for the later reset, since the earlier
		// one alone would not free the account.
		return resetOf(a.w).Compare(resetOf(b.w))
	})
	l.Window = best.name
	l.ResetAt = resetOf(best.w)
	return l
}

// windows is every window Codex named during the turn, with its latest use
// and reset — reported whether or not the turn hit a limit, so a hub can see
// an account's headroom before it runs out rather than only once it has.
//
// Codex names its windows primary and secondary and their durations vary by
// plan, so the names are kept as they are, exactly as limit keeps them.
func (t *translator) windows() []adapter.Window {
	if t.limits == nil {
		return nil
	}
	var out []adapter.Window
	for _, n := range []struct {
		name string
		w    *window
	}{{"primary", t.limits.Primary}, {"secondary", t.limits.Secondary}} {
		if n.w == nil {
			continue
		}
		// Codex already reports a percentage, so nothing is scaled here.
		out = append(out, adapter.Window{Name: n.name, UsedPercent: float64(n.w.UsedPercent), ResetAt: resetOf(n.w)})
	}
	return out
}

func resetOf(w *window) time.Time {
	if w == nil || w.ResetsAt == nil || *w.ResetsAt <= 0 {
		return time.Time{}
	}
	return time.Unix(*w.ResetsAt, 0).UTC()
}

// loginCheck is the check the turn was started with, or a bare one for an
// ended built without a turn.
func (e ended) loginCheck() string {
	if e.check == "" {
		return adapter.Spec{}.HarnessCheck("codex", "login", "status")
	}
	return e.check
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
