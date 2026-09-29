// Package codex drives Codex through `codex app-server --listen stdio://`,
// JSON-RPC over its stdin and stdout: initialize, then thread/start for a new
// session or thread/resume for one Codex already has — followed, on a resume
// whose run has a context, by thread/inject_items putting that context in the
// thread (decision 0050) — then one turn/start per run, with turn/steer and
// turn/interrupt while it runs (decision 0006, ARCHITECTURE.md §3). The
// thread id is the session's native id.
//
// How the conversation becomes a run — which notifications are the run's,
// which answer decides it, what the owner's approval and sandbox settings
// mean — is decisions 0036 and 0037.
package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/supervise"
)

// Adapter is the Codex adapter.
type Adapter struct {
	// Raw, when set, receives the whole conversation with the app-server, one
	// JSON object per line: what Codex wrote as it wrote it, and what YAD wrote
	// wrapped as {">": …}. It is the local diagnostic copy that is never sent
	// to a hub (decision 0012), and what the fixture recorder keeps. Called
	// once per Start; written from more than one goroutine, one line at a time.
	Raw func(spec adapter.Spec) io.Writer
}

func (Adapter) Harness() string { return "codex" }

// AppliesEffort: a run's effort is the effort of its turn/start.
func (Adapter) AppliesEffort() bool { return true }

// Forks: a fork is thread/fork of the thread forked, which Codex copies into
// a new thread and leaves as it was; the new thread is the session's
// (decision 0065).
func (Adapter) Forks() bool { return true }

// Timings a test may shorten.
var (
	// handshakeTimeout bounds each step before the turn runs: initialize,
	// thread/start or thread/resume, thread/inject_items on a resume with a
	// context, turn/start. A resume loads the thread's
	// whole rollout, so it is generous; an app-server that answers none of
	// them in this long is wedged, and nothing else would notice until the
	// inactivity watchdog, half an hour later.
	handshakeTimeout = 30 * time.Second
	// exitGrace is how long the app-server gets to exit once the turn is over
	// and its input is closed. It exits at once; anything longer is a server
	// that will not, and its group is killed (DEV-21).
	exitGrace = 2 * time.Second
	// drainGrace is how long the reader keeps going after the app-server has
	// exited, for output already in the pipe. Anything holding the pipe open
	// longer is a descendant that escaped the process group.
	drainGrace = 2 * time.Second
	termGrace  = 5 * time.Second
	// steerTimeout bounds a steer, all of it: waiting for the turn to start,
	// then for Codex to take the input. The runner's event loop is waiting on
	// it, and so is the cancel ladder.
	steerTimeout = 10 * time.Second
	// limitsTimeout bounds the one extra question asked after a usage limit
	// when Codex has not yet said which window ran out.
	limitsTimeout = 5 * time.Second
)

// Settings keys this adapter reads from the owner's configuration.
const (
	settingApproval = "approval"
	settingSandbox  = "sandbox"
)

// The owner's defaults when config.toml sets nothing (decision 0036). A run is
// unattended and nobody can approve anything, so Codex is told never to ask;
// and the owner's machine is the boundary (decision 0015), so the sandbox is
// off — Codex's own sandbox would also cut the network a run needs for git
// push, gh and installing dependencies. An owner who wants the sandbox sets
// sandbox = "workspace-write" under [harness.codex], and that wins.
const (
	defaultApproval = "never"
	defaultSandbox  = "danger-full-access"
)

// threadID is the shape of a Codex thread id: a UUID (v7 today).
var threadID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Start spawns the app-server for one run. The handshake runs after Start
// returns, so that everything it can end in — a thread Codex does not have, a
// server that never answers — is an Outcome with its class, not a start
// error.
func (a Adapter) Start(ctx context.Context, spec adapter.Spec) (adapter.Turn, error) {
	if spec.Binary == "" {
		return nil, fmt.Errorf("no codex binary resolved — run `%s` to see where codex was looked for", spec.YadCommand("doctor"))
	}
	if spec.NativeSessionID != "" && !threadID.MatchString(spec.NativeSessionID) {
		return nil, fmt.Errorf("codex thread id %q is not a UUID — the session store is damaged; close the session and start a new one", spec.NativeSessionID)
	}
	fork := ""
	if spec.NativeSessionID == "" {
		fork = spec.ForkFrom
	}
	if fork != "" && !threadID.MatchString(fork) {
		return nil, fmt.Errorf("codex thread id %q of the session to fork is not a UUID — the session store is damaged; fork another session", fork)
	}
	p, err := supervise.Start(ctx, supervise.Spec{
		Path: spec.Binary, Args: []string{"app-server", "--listen", "stdio://"},
		Dir: spec.Workdir, Env: spec.Env, Stdin: true,
	})
	if err != nil {
		return nil, &adapter.LocalError{Msg: "codex would not start on this runner", Err: err}
	}
	t := &turn{
		ctx:       ctx,
		p:         p,
		spec:      spec,
		asked:     spec.NativeSessionID,
		forkFrom:  fork,
		q:         adapter.NewQueue(),
		wait:      map[int64]string{},
		begun:     make(chan struct{}),
		finished:  make(chan struct{}),
		eof:       make(chan struct{}),
		done:      make(chan struct{}),
		exitGrace: exitGrace, drainGrace: drainGrace, termGrace: termGrace,
	}
	t.tr = newTranslator(t.q.Push)
	t.tr.forkFrom = fork
	t.in = newInput(p.Stdin())
	t.conn = NewConn(t.in)
	if a.Raw != nil {
		t.conn.trace = transcript(a.Raw(spec))
	}
	go t.q.Pump(ctx)
	go t.run()
	go t.reap()
	return t, nil
}

// transcript writes both directions of the conversation to w, ours wrapped.
func transcript(w io.Writer) func(bool, []byte) {
	var mu sync.Mutex
	return func(out bool, line []byte) {
		mu.Lock()
		defer mu.Unlock()
		if out {
			w.Write([]byte(`{">":`))
			w.Write(line)
			w.Write([]byte("}\n"))
			return
		}
		w.Write(line)
		w.Write([]byte{'\n'})
	}
}

type turn struct {
	ctx  context.Context
	p    *supervise.Process
	spec adapter.Spec
	// asked is the thread the run resumes; "" for a new one.
	asked string
	// forkFrom is the thread the run forks into a new one, set only when
	// asked is "".
	forkFrom string
	conn     *Conn
	in       *input
	q        *adapter.Queue

	// Owned by run's goroutine until done is closed.
	tr       *translator
	wait     map[int64]string // requests run has sent, by id, still unanswered
	deadline time.Time        // when the oldest of them is late
	late     string           // its method
	askedFor bool             // the rate limits have been asked for once

	mu          sync.Mutex
	thread      string
	turnID      string
	starting    bool // turn/start has been sent
	over        bool // the turn takes no more input
	interrupted bool
	begun       chan struct{} // closed once the turn has an id, or will never have one

	finished chan struct{} // closed once the turn is over and input is closed
	eof      chan struct{} // closed once the app-server's output has ended
	done     chan struct{}
	outcome  adapter.Outcome

	exitGrace, drainGrace, termGrace time.Duration
}

func (t *turn) Events() <-chan v1.Event { return t.q.Out() }

// NativeSessionID is the thread: the one resumed from the start, a new one
// from the moment thread/start answers — before the first event after it, so
// the runner pins it before anything else can happen (decision 0031).
func (t *turn) NativeSessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.thread != "" {
		return t.thread
	}
	return t.asked
}

func (t *turn) Wait() adapter.Outcome {
	<-t.done
	return t.outcome
}

// Steer adds input to the running turn with turn/steer. Codex takes it into
// the same turn — it answers once, with the steer read — and says so before
// the turn goes on, so a steer it refused is reported here.
func (t *turn) Steer(text string) error {
	ctx, cancel := context.WithTimeout(t.ctx, steerTimeout)
	defer cancel()
	select {
	case <-t.begun:
	case <-ctx.Done():
		return errors.New("codex has not started the turn yet — send the steer again in a moment")
	}
	t.mu.Lock()
	thread, id, over := t.thread, t.turnID, t.over
	t.mu.Unlock()
	if over || id == "" {
		return errors.New("the run has already finished — send this as a new run in the same session")
	}
	_, err := t.conn.Call(ctx, "turn/steer", map[string]any{
		"threadId": thread, "expectedTurnId": id, "input": textInput(text),
	})
	var rpcErr *RPCError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &rpcErr) && strings.Contains(rpcErr.Message, "no active turn"):
		return errors.New("the run has already finished — send this as a new run in the same session")
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("codex did not take the steer in time — it may still arrive; watch for it in the answer before sending it again")
	}
	return fmt.Errorf("codex refused the steer: %w", err)
}

// Interrupt asks Codex to end the turn with turn/interrupt. The session stays
// resumable; the run ends with the turn/completed Codex sends back. An
// interrupt that arrives before the turn has an id is sent the moment it has
// one; one that arrives before turn/start ends the run without it. A turn
// already over is not an error.
func (t *turn) Interrupt() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.over || t.interrupted {
		return nil
	}
	t.interrupted = true
	if t.turnID == "" {
		return nil
	}
	return t.sendInterrupt()
}

// sendInterrupt is called with mu held. Its answer is not waited for: Codex
// answers only while the turn is live, and the turn/completed that follows is
// the answer that matters.
func (t *turn) sendInterrupt() error {
	_, err := t.conn.Send("turn/interrupt", map[string]any{"threadId": t.thread, "turnId": t.turnID})
	return err
}

// Terminate is the ladder's SIGTERM, for a Codex that did not stop when
// interrupted.
func (t *turn) Terminate() error {
	t.p.Terminate()
	return nil
}

func textInput(text string) []map[string]any {
	return []map[string]any{{"type": "text", "text": text}}
}

// run is the conversation: it reads every line the app-server writes, in
// order, and drives the handshake and the turn from them.
func (t *turn) run() {
	lines := make(chan Line)
	go func() {
		defer close(lines)
		t.conn.Read(t.p.Stdout(), func(l Line) { lines <- l })
	}()
	t.send("initialize", map[string]any{
		"clientInfo": map[string]any{"name": "yad", "title": "YAD", "version": buildinfo.Version},
	})
	tick := time.NewTicker(adapter.TextFlush / 4)
	defer tick.Stop()
loop:
	for {
		select {
		case <-tick.C:
			t.tr.text.Tick()
			if !t.deadline.IsZero() && time.Now().After(t.deadline) {
				t.timedOut()
			}
		case l, ok := <-lines:
			if !ok {
				break loop
			}
			t.handle(l)
		}
	}
	t.p.Stdout().Close()
	t.stop()
	close(t.eof)
	exitErr := t.p.Wait()

	t.mu.Lock()
	e := ended{
		interrupted: t.interrupted,
		cancelled:   t.ctx.Err() != nil,
		exitErr:     exitErr,
		stderr:      t.p.Stderr(),
		asked:       t.asked,
		check:       t.spec.HarnessCheck("codex", "login", "status"),
	}
	t.mu.Unlock()
	t.outcome = t.tr.outcome(e)
	t.q.Close()
	close(t.done)
}

// send writes one of the conversation's own requests; its answer comes back
// through handle. Each must be answered within handshakeTimeout.
func (t *turn) send(method string, params any) {
	id, err := t.conn.Send(method, params)
	if err != nil {
		t.tr.fail(adapter.ClassHarnessExited, fmt.Sprintf("codex app-server stopped reading its input before %s: %v", method, err))
		t.stop()
		return
	}
	t.wait[id] = method
	if t.deadline.IsZero() {
		t.deadline, t.late = time.Now().Add(handshakeTimeout), method
	}
}

func (t *turn) timedOut() {
	t.deadline = time.Time{}
	if t.late == "account/rateLimits/read" {
		// Only the reset time is missing; the limit itself is known.
		t.stop()
		return
	}
	t.tr.fail(adapter.ClassHarness, fmt.Sprintf("codex app-server did not answer %s within %s — check that it starts and is logged in: %s", t.late, handshakeTimeout, t.spec.HarnessCheck("codex", "login", "status")))
	t.stop()
}

func (t *turn) handle(l Line) {
	switch {
	case l.Err != nil:
		t.tr.text.Flush()
		t.tr.emitErr(adapter.ClassStream, l.Err.Error())
	case l.Msg.IsResponse():
		method, ok := t.wait[l.Msg.RequestID()]
		if !ok {
			return // turn/interrupt's, which nothing waits for
		}
		delete(t.wait, l.Msg.RequestID())
		t.deadline = time.Time{}
		t.respond(method, l.Msg)
	case l.Msg.IsRequest():
		t.answer(l.Msg)
	default:
		t.notified(l.Msg)
	}
}

// respond moves the handshake on from an answer to one of our requests.
func (t *turn) respond(method string, m *Message) {
	if m.Error != nil && method != "account/rateLimits/read" {
		t.refused(method, m.Error)
		return
	}
	switch method {
	case "initialize":
		t.conn.Notify("initialized", nil)
		t.startThread()
	case "thread/start", "thread/resume", "thread/fork":
		var r threadResult
		if json.Unmarshal(m.Result, &r) != nil || r.Thread.ID == "" {
			t.tr.fail(adapter.ClassHarness, method+" answered without a thread — report this with `codex --version` as a yad bug")
			t.stop()
			return
		}
		t.tr.model = r.Model
		if t.asked != "" && r.Thread.ID != t.asked {
			// Whatever the turn would do, it would do without the
			// conversation it was meant to continue. Stopped before it starts.
			t.tr.mismatch = r.Thread.ID
			t.tr.emitErr(adapter.ClassSessionMismatch, t.tr.mismatchMessage(t.asked))
			t.stop()
			return
		}
		if t.forkFrom != "" && r.Thread.ID == t.forkFrom {
			// The turn would run in the thread it was meant to leave alone.
			t.tr.mismatch = r.Thread.ID
			t.tr.emitErr(adapter.ClassSessionMismatch, t.tr.mismatchMessage(t.asked))
			t.stop()
			return
		}
		t.mu.Lock()
		t.thread = r.Thread.ID
		t.tr.thread = r.Thread.ID
		interrupted := t.interrupted
		t.starting = !interrupted
		t.mu.Unlock()
		// The first event after the thread is known, so the runner pins it
		// before anything else can happen.
		t.tr.status("started")
		if interrupted {
			// Nothing has run yet; ending here keeps the session exactly as
			// an interrupt would (decision 0025).
			t.stop()
			return
		}
		// A fork is a resume into a new thread, and its developerInstructions
		// are read no sooner than a resume's: until a compaction the copy
		// opens with the forked thread's (decision 0050).
		if method != "thread/start" && t.spec.Brief.Context != "" {
			t.send("thread/inject_items", injectContext(r.Thread.ID, t.spec.Brief.Context))
			return
		}
		t.send("turn/start", turnStart(r.Thread.ID, t.spec))
	case "thread/inject_items":
		t.mu.Lock()
		interrupted := t.interrupted
		t.mu.Unlock()
		if interrupted {
			t.stop()
			return
		}
		t.send("turn/start", turnStart(t.thread, t.spec))
	case "turn/start":
		var r struct {
			Turn turnInfo `json:"turn"`
		}
		if json.Unmarshal(m.Result, &r) == nil && r.Turn.ID != "" {
			t.begin(r.Turn.ID)
		}
	case "account/rateLimits/read":
		t.tr.rateLimits(m.Result)
		t.stop()
	}
}

// turnStart is the run's turn/start. The effort goes on the turn, where the
// pinned protocol takes it as "the reasoning effort for this turn and
// subsequent turns" — a string the model advertises, which Codex checks and
// the runner does not. Absent, Codex uses its configured default: the
// protocol says an effort holds for later turns, but each run is its own
// app-server, and a resumed thread was measured running at the default
// again after a run that set low (DEV-124, decision 0049).
func turnStart(thread string, spec adapter.Spec) map[string]any {
	params := map[string]any{"threadId": thread, "input": textInput(spec.Brief.Instruction)}
	if spec.Effort != "" {
		params["effort"] = spec.Effort
	}
	return params
}

func (t *turn) startThread() {
	params := map[string]any{
		"cwd":            t.spec.Workdir,
		"approvalPolicy": setting(t.spec, settingApproval, defaultApproval),
		"sandbox":        setting(t.spec, settingSandbox, defaultSandbox),
	}
	if t.spec.Model != "" {
		params["model"] = t.spec.Model
	}
	// Codex's equivalent of Claude's appended system prompt: kept outside the
	// conversation, so it survives compaction. On a resume Codex keeps it as
	// the thread's and puts it before the model only when it rebuilds the
	// thread's opening, at a compaction; until then the thread still opens
	// with the first run's, so the run's context is also injected before its
	// turn (injectContext, decision 0050).
	if t.spec.Brief.Context != "" {
		params["developerInstructions"] = t.spec.Brief.Context
	}
	// A resume or a fork is answered with the thread and the model; the
	// thread's id is read, its turns never are. Without excludeTurns the
	// answer carries every one of them — one line that grows with the
	// session toward adapter.MaxLine — and, since 0.157.1, a deprecation
	// notice saying hydration is going away (DEV-139). 0.147.0 refused the
	// field; it is no longer pinned (decision 0067).
	if t.asked != "" {
		params["threadId"] = t.asked
		params["excludeTurns"] = true
		t.send("thread/resume", params)
		return
	}
	if t.forkFrom != "" {
		params["threadId"] = t.forkFrom
		params["excludeTurns"] = true
		t.send("thread/fork", params)
		return
	}
	t.send("thread/start", params)
}

// injectContext puts a resumed run's context in the thread as a developer
// message, the role Codex gives developerInstructions itself, just before the
// run's turn. Measured on Codex 0.147.0: thread/resume's developerInstructions
// reach the model only after a compaction, so a continuing run otherwise
// works under the context of the run that opened the session. Injected on
// every resumed run that has a context, so each run's own is the latest one
// the model has read; one that has none injects nothing (decision 0050).
func injectContext(thread, context string) map[string]any {
	return map[string]any{"threadId": thread, "items": []any{map[string]any{
		"type": "message", "role": "developer",
		"content": []any{map[string]any{"type": "input_text", "text": context}},
	}}}
}

func setting(spec adapter.Spec, key, def string) string {
	if v := spec.Settings[key]; v != "" {
		return v
	}
	return def
}

// refused ends the run on a request Codex answered with an error.
func (t *turn) refused(method string, e *RPCError) {
	switch {
	case method == "thread/resume" && strings.Contains(e.Message, "no rollout found"):
		t.tr.fail(adapter.ClassSessionNotFound, fmt.Sprintf("codex has no thread %s on this runner (%s) — the session's rollout is gone; start a new session", t.asked, e.Message))
	case method == "thread/fork" && strings.Contains(e.Message, "no rollout found"):
		// Measured on 0.157.1: the same words as a resume of nothing.
		t.tr.fail(adapter.ClassSessionNotFound, fmt.Sprintf("codex has no thread %s to fork on this runner (%s) — the forked session's rollout is gone; fork a session that has one, or start a new session", t.forkFrom, e.Message))
	case method == "turn/start":
		t.tr.fail(adapter.ClassHarness, "codex refused the turn: "+e.Message)
	case method == "thread/inject_items":
		t.tr.fail(adapter.ClassHarness, "codex would not take the run's context into the resumed thread: "+e.Message+" — check `codex --version` against the protocol this yad was built for")
	default:
		t.tr.fail(adapter.ClassHarness, fmt.Sprintf("codex refused %s: %s — check the owner's [harness.codex] settings in config.toml and `codex --version`", method, e.Message))
	}
	t.stop()
}

// begin opens the gate: from here, notifications for this turn are the run's.
func (t *turn) begin(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// An empty id opens nothing: it names no turn a notification could
	// match, and turnID doubles as the sign that begun is closed.
	if id == "" || t.turnID != "" || t.over {
		return
	}
	t.turnID = id
	close(t.begun)
	if t.interrupted {
		t.sendInterrupt()
	}
}

// notified routes a notification. Codex writes subagents' threads to the same
// pipe, and a resume replays the thread's history before the turn starts; only
// what belongs to this thread's current turn is the run's.
func (t *turn) notified(m *Message) {
	if m.Method == "account/rateLimits/updated" {
		t.tr.rateLimits(m.Params)
		return
	}
	t.mu.Lock()
	thread, current, starting := t.thread, t.turnID, t.starting
	t.mu.Unlock()
	if thread == "" || threadOf(m.Params) != thread {
		if m.Method == "warning" && threadOf(m.Params) == "" {
			t.warning(m.Params)
		}
		return
	}
	var p turnParams
	json.Unmarshal(m.Params, &p)
	switch {
	case m.Method == "warning":
		t.warning(m.Params)
		return
	case m.Method == "turn/started" && current == "" && starting:
		// A thread has one turn at a time, and a replay starts none: the
		// first turn/started after turn/start is this run's, even when it
		// arrives before turn/start's answer.
		t.begin(p.turnID())
		return
	case current == "" || p.turnID() != "" && p.turnID() != current:
		return
	}
	t.tr.notification(m.Method, m.Params)
	if m.Method == "turn/completed" {
		t.completed()
	}
}

func (t *turn) warning(params json.RawMessage) {
	var p struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(params, &p) == nil && p.Message != "" {
		t.tr.text.Flush()
		t.tr.status("warning: " + p.Message)
	}
}

// completed ends the conversation once the turn is over. A usage limit whose
// window Codex has not named yet is asked for first: its reset time is what
// the hub needs to decide where the next run goes (decision 0013).
func (t *turn) completed() {
	if d := t.tr.done; d != nil && d.Error.kind() == "usageLimitExceeded" && t.tr.limits == nil && !t.askedFor {
		t.askedFor = true
		t.send("account/rateLimits/read", nil)
		t.deadline = time.Now().Add(limitsTimeout)
		return
	}
	t.stop()
}

// answer replies to a request from Codex. Nobody is there to approve
// anything: whatever the owner's approval policy sent here is declined, and
// the sandbox the owner chose stands (decision 0036). Any thread's requests
// are answered — a subagent waiting on an approval would hang the turn.
func (t *turn) answer(m *Message) {
	ours := threadOf(m.Params) == t.NativeSessionID()
	declined := func(what string) {
		if ours {
			t.tr.text.Flush()
			t.tr.status("approval declined: " + what)
		}
	}
	switch m.Method {
	case "item/commandExecution/requestApproval":
		var p struct {
			Command string `json:"command"`
			Kind    string `json:"kind"`
		}
		json.Unmarshal(m.Params, &p)
		t.conn.Reply(m.ID, map[string]any{"decision": "decline"})
		if p.Kind == "writeStdin" {
			// Since 0.157.1 the same request asks to type into a command
			// already running; naming only the command would say it was
			// refused a start it already had.
			declined("input to " + firstNonEmpty(p.Command, "a running command"))
			break
		}
		declined(firstNonEmpty(p.Command, "a command"))
	case "item/fileChange/requestApproval":
		t.conn.Reply(m.ID, map[string]any{"decision": "decline"})
		declined("a file change")
	case "execCommandApproval", "applyPatchApproval":
		t.conn.Reply(m.ID, map[string]any{"decision": map[string]any{"denied": map[string]any{"rejection": unattended}}})
		declined(strings.TrimSuffix(m.Method, "Approval"))
	case "item/permissions/requestApproval":
		// Granting nothing is the answer that keeps the sandbox as the owner
		// set it.
		t.conn.Reply(m.ID, map[string]any{"permissions": map[string]any{}, "scope": "turn"})
		declined("more permissions")
	case "mcpServer/elicitation/request":
		t.conn.Reply(m.ID, map[string]any{"action": "decline"})
		declined("an MCP server's question")
	default:
		// Questions for a person, auth refreshes, dynamic tools: none of them
		// has an answer on an unattended runner.
		t.conn.ReplyError(m.ID, codeMethodNotFound, unattended)
		declined(m.Method)
	}
}

const unattended = "This run is unattended and nobody can answer. The runner's owner sets Codex's approval policy and sandbox in config.toml."

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// stop ends input: the turn is over, or never started. Closing stdin is how
// the app-server learns to exit.
func (t *turn) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.over {
		return
	}
	t.over = true
	if t.turnID == "" {
		close(t.begun)
	}
	t.in.Close()
	close(t.finished)
}

// reap makes sure the reader ends: the app-server is stopped if it lingers
// after the turn is over or after its output has ended, and the pipe is closed
// if something outlives it holding it.
func (t *turn) reap() {
	lingering := true
	select {
	case <-t.p.Done():
		lingering = false
	case <-t.finished:
	case <-t.eof:
	}
	if lingering {
		timer := time.NewTimer(t.exitGrace)
		select {
		case <-t.p.Done():
		case <-timer.C:
			t.p.Stop(supervise.Ladder{TermGrace: t.termGrace})
		}
		timer.Stop()
	}
	select {
	case <-t.done:
	case <-time.After(t.drainGrace):
		t.p.Stdout().Close()
	}
}

// input queues writes to the app-server's stdin. A write never blocks: an
// interrupt comes from the runner's event loop, which must go on to the
// ladder's signals if Codex has stopped reading.
type input struct {
	mu     sync.Mutex
	ch     chan []byte
	closed bool
}

var errInputClosed = errors.New("the turn is over and codex takes no more input")

func newInput(w io.WriteCloser) *input {
	in := &input{ch: make(chan []byte, 64)}
	go func() {
		failed := false
		for b := range in.ch {
			if failed {
				continue // drain, so writers never block
			}
			if _, err := w.Write(b); err != nil {
				// Codex has gone; the reader will see its output end and say why.
				failed = true
			}
		}
		w.Close()
	}()
	return in
}

func (in *input) Write(b []byte) (int, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return 0, errInputClosed
	}
	select {
	case in.ch <- bytes.Clone(b):
		return len(b), nil
	default:
		return 0, errors.New("codex is not reading its input")
	}
}

func (in *input) Close() {
	in.mu.Lock()
	defer in.mu.Unlock()
	if !in.closed {
		in.closed = true
		close(in.ch)
	}
}
