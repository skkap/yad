// Package acp drives a harness through the Agent Client Protocol, version 1:
// JSON-RPC over the agent's stdin and stdout, one object per line
// (agentclientprotocol.com). initialize, then session/new for a new session,
// session/resume for one the agent already has or session/fork for a fork,
// then session/set_config_option for the run's model and effort, then one
// session/prompt, whose answer ends the turn; session/cancel interrupts it
// (decision 0073).
//
// The protocol is the core; what is the harness's own — how to start it,
// how the run's context reaches its model, what its failures mean — is an
// Agent (internal/adapter/opencode is the first). ACP v1 has no steer, so no
// turn driven here takes one, and a harness whose runs need one is not a
// harness for this core.
package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/jsonrpc"
	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/supervise"
)

// ProtocolVersion is the ACP major version the core speaks. v2 is in alpha
// and drops session/load and modes; an agent that answers initialize with
// another version is not driven (decision 0073).
const ProtocolVersion = 1

// Agent is everything about one ACP agent that is not the protocol's.
type Agent struct {
	// ID is the harness's id in the catalog; Name how a message names it.
	ID, Name string
	// Args start the agent's ACP server on its stdin and stdout.
	Args []string
	// Prepare readies one run: the environment the agent gets in place of
	// spec.Env — the run's own, less what only the owner may set, plus how
	// the run's context reaches its model and a secret for a server the agent
	// opens — and what to remove once the agent has exited. It must not put a
	// secret in argv or in anything it returns but env. Nil passes spec.Env
	// as it is.
	Prepare func(spec adapter.Spec) (env []string, cleanup func(), err error)
	// Classify reads a prompt the agent answered with an error: which class
	// of failure it is, in the agent's own structure where it gives one.
	// A zero Failure is harness_error with the agent's words.
	Classify func(e *jsonrpc.RPCError, spec adapter.Spec) Failure
	// ExitCode reads a finished command's exit status from a tool call's
	// rawOutput, where the agent puts one; nil where it does not.
	ExitCode func(rawOutput json.RawMessage) *int
	// LoginCheck is the agent's own command that says whether its login
	// works, after its binary: every failure that could be a login names it.
	LoginCheck []string
	// Raw, when set, receives the whole conversation, one JSON object per
	// line, ours wrapped as {">": …}: the local diagnostic copy never sent to
	// a hub (decision 0012), and what the fixture recorder keeps.
	Raw func(spec adapter.Spec) io.Writer
}

// Failure is how a failed prompt is classified.
type Failure struct {
	Class   string
	Message string
	// Limit is set for a usage limit, with the window and reset where the
	// agent said them.
	Limit *adapter.Limit
	// AuthRejected is the provider refusing the credential, said in the
	// agent's structure (adapter.Outcome.AuthRejected).
	AuthRejected bool
}

// Timings a test may shorten.
var (
	// handshakeTimeout bounds each request before the prompt: initialize,
	// the session, each config option. An agent loads its providers and its
	// project on the first of them, which takes seconds on a cold start; one
	// that answers none in this long is wedged, and nothing else would
	// notice until the inactivity watchdog.
	handshakeTimeout = 60 * time.Second
	// exitGrace is how long the agent gets to exit once the turn is over and
	// its input is closed; longer and its group is stopped.
	exitGrace = 5 * time.Second
	// drainGrace is how long the reader keeps going after the agent has
	// exited, for output already in the pipe.
	drainGrace = 2 * time.Second
	termGrace  = 5 * time.Second
)

// settingPermission is the owner's answer to a permission request
// (config.toml's permission_mode under the harness). Never a protocol field:
// a hub cannot set or widen it (AGENTS.md).
const settingPermission = "permission_mode"

// The owner's two answers. allow is the default for the reason decision
// 0036 gives Codex's approval policy: a run is unattended, nobody can
// approve anything, and the owner's machine is the boundary (0015). What the
// agent's own configuration denies is never asked, so it stays denied.
const (
	PermissionAllow  = "allow"
	PermissionReject = "reject"
)

// permission is the run's answer, or an error naming the setting.
func permission(spec adapter.Spec, agent Agent) (string, error) {
	switch v := spec.Settings[settingPermission]; v {
	case "", PermissionAllow:
		return PermissionAllow, nil
	case PermissionReject:
		return PermissionReject, nil
	default:
		return "", fmt.Errorf("permission_mode %q under [harness.%s] in config.toml is not one yad knows — set it to %q or %q", v, agent.ID, PermissionAllow, PermissionReject)
	}
}

// Start spawns the agent for one run. The handshake runs after Start returns,
// so that everything it can end in — a session the agent does not have, a
// model it does not offer — is an Outcome with its class, not a start error.
func Start(ctx context.Context, agent Agent, spec adapter.Spec) (adapter.Turn, error) {
	if spec.Binary == "" {
		return nil, fmt.Errorf("no %s binary resolved — run `%s` to see where it was looked for", agent.ID, spec.YadCommand("doctor"))
	}
	answer, err := permission(spec, agent)
	if err != nil {
		return nil, err
	}
	fork := ""
	if spec.NativeSessionID == "" {
		fork = spec.ForkFrom
	}
	env := slices.Clone(spec.Env)
	cleanup := func() {}
	if agent.Prepare != nil {
		prepared, done, err := agent.Prepare(spec)
		if err != nil {
			return nil, err
		}
		env = prepared
		if done != nil {
			cleanup = done
		}
	}
	p, err := supervise.Start(ctx, supervise.Spec{
		Path: spec.Binary, Args: agent.Args, Dir: spec.Workdir, Env: env, Stdin: true,
	})
	if err != nil {
		cleanup()
		return nil, &adapter.LocalError{Msg: agent.Name + " would not start on this runner", Err: err}
	}
	t := &turn{
		ctx:       ctx,
		agent:     agent,
		p:         p,
		spec:      spec,
		answer:    answer,
		asked:     spec.NativeSessionID,
		forkFrom:  fork,
		cleanup:   cleanup,
		q:         adapter.NewQueue(),
		wait:      map[int64]string{},
		finished:  make(chan struct{}),
		eof:       make(chan struct{}),
		done:      make(chan struct{}),
		exitGrace: exitGrace, drainGrace: drainGrace, termGrace: termGrace,
	}
	t.tr = newTranslator(t.q.Push, agent, spec)
	t.in = newInput(p.Stdin(), agent.Name)
	t.conn = jsonrpc.NewConn(t.in, agent.Name)
	if agent.Raw != nil {
		t.conn.Trace = jsonrpc.Transcript(agent.Raw(spec))
	}
	go t.q.Pump(ctx)
	go t.run()
	go t.reap()
	return t, nil
}

type turn struct {
	ctx    context.Context
	agent  Agent
	p      *supervise.Process
	spec   adapter.Spec
	answer string // the owner's answer to a permission request
	// asked is the session the run resumes; "" for a new one. forkFrom is
	// the one it forks into a new one, set only when asked is "".
	asked    string
	forkFrom string
	cleanup  func()
	conn     *jsonrpc.Conn
	in       *input
	q        *adapter.Queue

	// Owned by run's goroutine until done is closed.
	tr       *translator
	caps     agentCaps
	options  []configOption
	wait     map[int64]string // requests run has sent, by id, still unanswered
	deadline time.Time        // when the oldest of them is late
	late     string           // its method
	refusal  *jsonrpc.RPCError
	pages    int // pages of session/list read after the refusal
	// modelSet and effortSet: each is asked for once, so an agent whose
	// answer carries no options is not asked again for ever.
	modelSet, effortSet bool

	mu          sync.Mutex
	session     string
	prompted    bool // session/prompt has been sent: the turn's updates are the run's
	over        bool // the turn takes no more input
	interrupted bool

	finished chan struct{} // closed once the turn is over and input is closed
	eof      chan struct{} // closed once the agent's output has ended
	done     chan struct{}
	outcome  adapter.Outcome

	exitGrace, drainGrace, termGrace time.Duration
}

func (t *turn) Events() <-chan v1.Event { return t.q.Out() }

// NativeSessionID is the agent's session id: the one resumed from the start,
// a new one from the moment session/new or session/fork answers — before the
// first event after it, so the runner pins it first (decision 0031).
func (t *turn) NativeSessionID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.session != "" {
		return t.session
	}
	return t.asked
}

func (t *turn) Wait() adapter.Outcome {
	<-t.done
	return t.outcome
}

// Steer is refused: ACP v1 has no way to add input to a turn that is
// running, and a second session/prompt is a second turn, which is a second
// run (decision 0073). The runner advertises no steer for a harness driven
// here, so a hub that reads it sends none.
func (t *turn) Steer(string) error {
	return fmt.Errorf("%s takes no input while a turn runs — send this as a new run in the same session", t.agent.Name)
}

// Interrupt asks the agent to end the turn with session/cancel. The session
// stays resumable; the run ends with the prompt's answer, stop reason
// cancelled. One that arrives before the prompt is sent ends the run without
// it. A turn already over is not an error.
func (t *turn) Interrupt() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.over || t.interrupted {
		return nil
	}
	t.interrupted = true
	if !t.prompted {
		return nil
	}
	return t.conn.Notify("session/cancel", map[string]any{"sessionId": t.session})
}

// Terminate is the ladder's SIGTERM, for an agent that did not stop when
// interrupted.
func (t *turn) Terminate() error {
	t.p.Terminate()
	return nil
}

// run is the conversation: it reads every line the agent writes, in order,
// and drives the handshake and the turn from them.
func (t *turn) run() {
	lines := make(chan jsonrpc.Line)
	go func() {
		defer close(lines)
		t.conn.Read(t.p.Stdout(), func(l jsonrpc.Line) { lines <- l })
	}()
	// No filesystem and no terminal: the agent works in the run's workdir
	// with its own tools, and a client that served them would be a second
	// place the owner's rules had to hold.
	t.send("initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
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
	t.cleanup()

	t.mu.Lock()
	e := ended{
		interrupted: t.interrupted,
		cancelled:   t.ctx.Err() != nil,
		exitErr:     exitErr,
		stderr:      t.p.Stderr(),
		asked:       t.asked,
		check:       t.spec.HarnessCheck(t.agent.ID, t.agent.LoginCheck...),
	}
	t.mu.Unlock()
	t.outcome = t.tr.outcome(e)
	t.q.Close()
	close(t.done)
}

// send writes one of the handshake's requests; its answer comes back through
// handle. Each must be answered within handshakeTimeout — all but the prompt,
// which takes as long as the turn does.
func (t *turn) send(method string, params any) {
	id, err := t.conn.Send(method, params)
	if err != nil {
		t.tr.fail(adapter.ClassHarnessExited, fmt.Sprintf("%s stopped reading its input before %s: %v", t.agent.Name, method, err))
		t.stop()
		return
	}
	t.wait[id] = method
	if method != "session/prompt" && t.deadline.IsZero() {
		t.deadline, t.late = time.Now().Add(handshakeTimeout), method
	}
}

func (t *turn) timedOut() {
	t.deadline = time.Time{}
	t.tr.fail(adapter.ClassHarness, fmt.Sprintf("%s did not answer %s within %s — check that it starts and has a working login: %s", t.agent.Name, t.late, handshakeTimeout, t.spec.HarnessCheck(t.agent.ID, t.agent.LoginCheck...)))
	t.stop()
}

func (t *turn) handle(l jsonrpc.Line) {
	switch {
	case l.Err != nil:
		t.tr.text.Flush()
		t.tr.emitErr(adapter.ClassStream, l.Err.Error())
	case l.Msg.IsResponse():
		method, ok := t.wait[l.Msg.RequestID()]
		if !ok {
			return
		}
		delete(t.wait, l.Msg.RequestID())
		t.deadline = time.Time{}
		t.respond(method, l.Msg)
	case l.Msg.IsRequest():
		t.request(l.Msg)
	default:
		t.notified(l.Msg)
	}
}

// agentCaps is what initialize's answer says the agent can do, as far as the
// core reads it.
type agentCaps struct {
	ProtocolVersion   int `json:"protocolVersion"`
	AgentCapabilities struct {
		SessionCapabilities struct {
			Resume *struct{} `json:"resume"`
			Fork   *struct{} `json:"fork"`
			List   *struct{} `json:"list"`
		} `json:"sessionCapabilities"`
	} `json:"agentCapabilities"`
}

// configOption is one of a session's config options: a model, a mode, an
// effort. Category is ACP's word for which it is; the id is the agent's.
type configOption struct {
	ID           string `json:"id"`
	Category     string `json:"category"`
	CurrentValue any    `json:"currentValue"`
	Options      []struct {
		Value string `json:"value"`
	} `json:"options"`
}

// respond moves the handshake on from an answer to one of our requests.
func (t *turn) respond(method string, m *jsonrpc.Message) {
	if m.Error != nil {
		t.refused(method, m.Error)
		return
	}
	switch method {
	case "initialize":
		if json.Unmarshal(m.Result, &t.caps) != nil || t.caps.ProtocolVersion != ProtocolVersion {
			t.tr.fail(adapter.ClassHarness, fmt.Sprintf("%s speaks ACP version %d and this yad speaks %d — install the %s release yad was built against (`%s` shows it)",
				t.agent.Name, t.caps.ProtocolVersion, ProtocolVersion, t.agent.Name, t.spec.YadCommand("doctor")))
			t.stop()
			return
		}
		t.openSession()
	case "session/new", "session/resume", "session/fork":
		var r struct {
			SessionID     string         `json:"sessionId"`
			ConfigOptions []configOption `json:"configOptions"`
		}
		json.Unmarshal(m.Result, &r)
		id := r.SessionID
		if method == "session/resume" {
			// A resume answers without an id: the session is the one asked.
			id = t.asked
		}
		if id == "" {
			t.tr.fail(adapter.ClassHarness, fmt.Sprintf("%s answered %s without a session — report this with `%s --version` as a yad bug", t.agent.Name, method, t.agent.ID))
			t.stop()
			return
		}
		if t.forkFrom != "" && id == t.forkFrom {
			// The turn would run in the session it was meant to leave alone.
			t.tr.mismatch = id
			t.tr.emitErr(adapter.ClassSessionMismatch, t.tr.mismatchMessage())
			t.stop()
			return
		}
		t.options = r.ConfigOptions
		t.mu.Lock()
		t.session = id
		t.tr.session = id
		interrupted := t.interrupted
		t.mu.Unlock()
		// The first event after the session is known, so the runner pins it
		// before anything else can happen.
		t.tr.status("started")
		if interrupted {
			// Nothing has run yet; ending here keeps the session exactly as
			// an interrupt would (decision 0025).
			t.stop()
			return
		}
		t.configure()
	case "session/set_config_option":
		var r struct {
			ConfigOptions []configOption `json:"configOptions"`
		}
		if json.Unmarshal(m.Result, &r) == nil && r.ConfigOptions != nil {
			t.options = r.ConfigOptions
		}
		t.configure()
	case "session/list":
		t.missing(m.Result)
	case "session/prompt":
		var r promptResult
		if json.Unmarshal(m.Result, &r) != nil || r.StopReason == "" {
			t.tr.fail(adapter.ClassHarness, t.agent.Name+" answered the prompt without a stop reason — report this as a yad bug")
		} else {
			t.tr.text.Flush()
			t.tr.done = &r
		}
		t.stop()
	}
}

// openSession sends the request that opens the run's session: a resume, a
// fork or a new one. What the agent cannot do is refused here in words, not
// sent to fail in the agent's.
func (t *turn) openSession() {
	sc := t.caps.AgentCapabilities.SessionCapabilities
	base := map[string]any{"cwd": t.spec.Workdir, "mcpServers": []any{}}
	switch {
	case t.asked != "":
		if sc.Resume == nil {
			t.tr.fail(adapter.ClassHarness, t.agent.Name+" does not resume a session over ACP (no sessionCapabilities.resume) — start a new session")
			t.stop()
			return
		}
		// session/resume rather than session/load: load replays the whole
		// conversation as updates before it answers, and nothing of it is
		// the run's.
		base["sessionId"] = t.asked
		t.send("session/resume", base)
	case t.forkFrom != "":
		if sc.Fork == nil {
			t.tr.fail(adapter.ClassHarness, t.agent.Name+" does not fork a session over ACP (no sessionCapabilities.fork) — open the session without fork_from")
			t.stop()
			return
		}
		base["sessionId"] = t.forkFrom
		t.send("session/fork", base)
	default:
		t.send("session/new", base)
	}
}

// configure sets what the run asks of the session, one option at a time —
// its model, then its effort — and sends the prompt once nothing is left.
// Each is the agent's own option of ACP's category: the id is the agent's,
// the category the protocol's.
func (t *turn) configure() {
	t.mu.Lock()
	interrupted := t.interrupted
	t.mu.Unlock()
	if interrupted {
		t.stop()
		return
	}
	if model := t.spec.Model; model != "" {
		o := t.option("model")
		switch {
		case o == nil:
			t.tr.fail(adapter.ClassHarness, fmt.Sprintf("%s offers no model to choose over ACP, and the run asks for %q — send it to a harness that does", t.agent.Name, model))
			t.stop()
			return
		case !t.modelSet && o.CurrentValue != model:
			t.modelSet, t.tr.model = true, model
			t.send("session/set_config_option", map[string]any{"sessionId": t.session, "configId": o.ID, "value": model})
			return
		}
		t.tr.model = model
	}
	if t.tr.model == "" {
		if o := t.option("model"); o != nil {
			if s, ok := o.CurrentValue.(string); ok {
				t.tr.model = s
			}
		}
	}
	// An effort is a run's own (decision 0049): asked for, it is set on
	// every run, since a resumed session may still hold another run's.
	if effort := t.spec.Effort; effort != "" && !t.effortSet {
		o := t.option("thought_level")
		if o == nil {
			t.tr.fail(adapter.ClassHarness, fmt.Sprintf("%s offers no effort levels for model %q — send the run without an effort, or with a model that has them", t.agent.Name, t.tr.model))
			t.stop()
			return
		}
		// Sent even when the option already shows the level: an agent's
		// current value can be the default it would pick, not a choice the
		// session holds, and the run asked for this one.
		t.effortSet = true
		t.send("session/set_config_option", map[string]any{"sessionId": t.session, "configId": o.ID, "value": effort})
		return
	}
	t.prompt()
}

// option is the session's config option of an ACP category, or nil.
func (t *turn) option(category string) *configOption {
	for i, o := range t.options {
		if o.Category == category {
			return &t.options[i]
		}
	}
	return nil
}

func (t *turn) prompt() {
	t.mu.Lock()
	if t.interrupted || t.over {
		t.mu.Unlock()
		t.stop()
		return
	}
	// From here the session's updates are the run's: a fork's replay of the
	// conversation it copies came before its answer, and none of it was.
	//
	// The prompt is queued before the lock is let go: Interrupt sends its
	// cancel once it sees prompted, and a cancel queued ahead of the prompt
	// would reach the agent with no turn to end, and the turn that followed
	// would run to the end. The queue never blocks, so holding the lock
	// across it costs the runner's event loop nothing.
	t.prompted = true
	if promptHook != nil {
		promptHook()
	}
	id, err := t.conn.Send("session/prompt", map[string]any{
		"sessionId": t.session,
		"prompt":    []any{map[string]any{"type": "text", "text": t.spec.Brief.Instruction}},
	})
	t.mu.Unlock()
	if err != nil {
		t.tr.fail(adapter.ClassHarnessExited, fmt.Sprintf("%s stopped reading its input before session/prompt: %v", t.agent.Name, err))
		t.stop()
		return
	}
	t.wait[id] = "session/prompt"
}

// promptHook, when a test sets it, runs the moment a turn is marked prompted
// and before its prompt is queued: where an interrupt must not get in.
var promptHook func()

// refused ends the run on a request the agent answered with an error. A
// resume or a fork of a session the agent does not know is asked once more,
// by listing its sessions: the agent's refusal says nothing a runner can tell
// apart from any other failure, and the list does. The list is not narrowed
// to the workdir, since a fork's own workdir is not the one the session it
// forks was made in — measured on OpenCode 1.18.33, which forks across
// directories and lists the source only when asked for every session.
func (t *turn) refused(method string, e *jsonrpc.RPCError) {
	switch method {
	case "session/resume", "session/fork":
		if t.caps.AgentCapabilities.SessionCapabilities.List != nil && t.refusal == nil {
			t.refusal = e
			t.send("session/list", map[string]any{})
			return
		}
		t.tr.fail(adapter.ClassHarness, fmt.Sprintf("%s refused %s: %s", t.agent.Name, method, e.Message))
	case "session/set_config_option":
		// A model or an effort the agent does not have: its words, which
		// name what it refused, and nothing the runner checks itself.
		t.tr.fail(adapter.ClassHarness, fmt.Sprintf("%s refused the run's settings: %s", t.agent.Name, e.Message))
	case "session/prompt":
		t.tr.failed(e)
	case "session/list":
		t.tr.fail(adapter.ClassHarness, fmt.Sprintf("%s refused to open the session: %s", t.agent.Name, t.refusal.Message))
	default:
		t.tr.fail(adapter.ClassHarness, fmt.Sprintf("%s refused %s: %s — check `%s --version` against the release this yad was built for (`%s`)", t.agent.Name, method, e.Message, t.agent.ID, t.spec.YadCommand("doctor")))
	}
	t.stop()
}

// listPages bounds the pages of session/list a refused resume or fork reads:
// OpenCode answers a hundred sessions to a page, so a machine with more than
// this many thousand is told the refusal rather than asked on for ever.
const listPages = 50

// missing reads one page of the agent's sessions after a refused resume or
// fork, and asks for the next while there is one: session_not_found once no
// page names the session asked for, the agent's refusal once one does — or
// once the pages run past listPages, when nothing can be said.
func (t *turn) missing(result json.RawMessage) {
	want, what := t.asked, "resume"
	if want == "" {
		want, what = t.forkFrom, "fork"
	}
	var r struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
		} `json:"sessions"`
		NextCursor string `json:"nextCursor"`
	}
	json.Unmarshal(result, &r)
	listed := slices.ContainsFunc(r.Sessions, func(s struct {
		SessionID string `json:"sessionId"`
	}) bool {
		return s.SessionID == want
	})
	t.pages++
	if !listed && r.Sessions != nil && r.NextCursor != "" && t.pages < listPages {
		t.send("session/list", map[string]any{"cursor": r.NextCursor})
		return
	}
	if listed || r.Sessions == nil || r.NextCursor != "" {
		t.tr.fail(adapter.ClassHarness, fmt.Sprintf("%s would not %s session %s: %s", t.agent.Name, what, want, t.refusal.Message))
	} else if what == "fork" {
		t.tr.fail(adapter.ClassSessionNotFound, fmt.Sprintf("%s has no session %s to fork on this runner — the forked session's conversation is gone; fork a session that has one, or start a new session", t.agent.Name, want))
	} else {
		t.tr.fail(adapter.ClassSessionNotFound, fmt.Sprintf("%s has no session %s on this runner — the session's conversation is gone; start a new session", t.agent.Name, want))
	}
	t.stop()
}

// notified routes a notification: only the run's session's updates, and only
// once its prompt is sent, are the run's. An agent that runs subagents as
// sessions of their own writes them to the same pipe.
func (t *turn) notified(m *jsonrpc.Message) {
	if m.Method != "session/update" {
		return
	}
	var p struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if json.Unmarshal(m.Params, &p) != nil {
		return
	}
	t.mu.Lock()
	ours := p.SessionID != "" && p.SessionID == t.session && t.prompted
	t.mu.Unlock()
	if ours {
		t.tr.update(p.Update)
	}
}

// request answers one of the agent's requests. A permission is answered from
// the owner's setting, whichever session asks — a subagent left waiting would
// hang the turn. Anything else is a capability this client did not offer.
func (t *turn) request(m *jsonrpc.Message) {
	if m.Method != "session/request_permission" {
		t.conn.ReplyError(m.ID, jsonrpc.CodeMethodNotFound, unattended)
		return
	}
	var p struct {
		SessionID string `json:"sessionId"`
		ToolCall  struct {
			Title string `json:"title"`
		} `json:"toolCall"`
		Options []struct {
			OptionID string `json:"optionId"`
			Kind     string `json:"kind"`
		} `json:"options"`
	}
	json.Unmarshal(m.Params, &p)
	kinds := []string{"allow_once", "allow_always"}
	if t.answer == PermissionReject {
		kinds = []string{"reject_once", "reject_always"}
	}
	// A one-time answer before a standing one: the owner's setting is asked
	// again for every request, and a standing answer would outlive a change
	// to it within the session.
	//
	// Once the turn is interrupted, every request is answered cancelled,
	// as ACP v1 asks of a client that has sent session/cancel: nothing more
	// of an ending turn is approved.
	t.mu.Lock()
	ours, interrupted := p.SessionID == t.session, t.interrupted
	t.mu.Unlock()
	chosen := ""
	for _, k := range kinds {
		for _, o := range p.Options {
			if o.Kind == k && chosen == "" && !interrupted {
				chosen = o.OptionID
			}
		}
	}
	if chosen == "" {
		t.conn.Reply(m.ID, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
	} else {
		t.conn.Reply(m.ID, map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": chosen}})
	}
	if ours && (chosen == "" || t.answer == PermissionReject) {
		t.tr.text.Flush()
		t.tr.status("permission declined: " + firstNonEmpty(p.ToolCall.Title, "a tool call"))
	}
}

const unattended = "This run is unattended and yad offers no filesystem, terminal or questions over ACP. The runner's owner sets how permission requests are answered in config.toml."

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// stop ends input: the turn is over, or never started. Closing stdin is how
// the agent learns to exit.
func (t *turn) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.over {
		return
	}
	t.over = true
	t.in.Close()
	close(t.finished)
}

// reap makes sure the reader ends: the agent is stopped if it lingers after
// the turn is over or after its output has ended, and the pipe is closed if
// something outlives it holding it.
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

// input queues writes to the agent's stdin. A write never blocks: an
// interrupt comes from the runner's event loop, which must go on to the
// ladder's signals if the agent has stopped reading.
type input struct {
	mu     sync.Mutex
	ch     chan []byte
	closed bool
	name   string
}

func newInput(w io.WriteCloser, name string) *input {
	in := &input{ch: make(chan []byte, 64), name: name}
	go func() {
		failed := false
		for b := range in.ch {
			if failed {
				continue // drain, so writers never block
			}
			if _, err := w.Write(b); err != nil {
				// The agent has gone; the reader will see its output end and say why.
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
		return 0, errors.New("the turn is over and " + in.name + " takes no more input")
	}
	select {
	case in.ch <- bytes.Clone(b):
		return len(b), nil
	default:
		return 0, errors.New(in.name + " is not reading its input")
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
