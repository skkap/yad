package runner

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
	"github.com/skkap/yad/internal/supervise"
)

// Error classes the executor reports itself, beside the adapters' own. A hub
// acts on the class and shows the message; a new class is an addition, never a
// rename.
const (
	// ClassRefused — the runner would not take the run (decision 0019).
	ClassRefused = "refused"
	// ClassPrepare — the workdir or a grant could not be put in place.
	ClassPrepare = "prepare_failed"
	// ClassStart — the harness could not be started.
	ClassStart = "harness_start_failed"
	// ClassInactivity — the harness produced no event for the inactivity
	// timeout and was stopped.
	ClassInactivity = "inactivity_timeout"
	// ClassWallClock — the run outlived its wall-clock cap and was stopped.
	ClassWallClock = "wall_clock_timeout"
	// ClassAdapter — the adapter ended the turn without a terminal state.
	ClassAdapter = "adapter_error"
)

// maxTextBytes caps an event's text and a result's final text. The protocol
// caps only tool payloads, but a hub or a proxy before it limits a body, and a
// report that can never fit would be retried forever — an event batch halves
// down to one event, so one event, and one result, must always fit. A MiB of
// prose is past anything a person reads from a stream.
const maxTextBytes = 1 << 20

// eventBatch is the most events one upload carries, and how many a run may
// spool before its reporter is woken ahead of its one-second tick
// (ARCHITECTURE.md §2).
const eventBatch = 100

// Exec is the executor: it turns a claimed run into an adapter turn under the
// watchdogs, spools the turn's events, and writes its terminal result to the
// outbox. It never talks to a hub — each connection's Reporter does that — so
// a hub that is down costs a run nothing but latency.
type Exec struct {
	Store    *store.Store
	Adapters *Registry
	// Config is the owner's: harness settings and the watchdog default. Nothing
	// the hub sends can widen either (decision 0015).
	Config config.Config
	// Data is the profile's data directory; workdirs and grant files live
	// under it.
	Data string
	// Binary resolves a harness to its executable; nil is harness.Locate, the
	// lookup detection uses.
	Binary func(harness string) (string, bool)
	// Report wakes a connection's reporter, so a result or a full batch goes
	// out now rather than at the next tick. Nil is fine: the tick finds it.
	Report func(connection string)
	// Grace is how long a harness gets to end its turn after a watchdog
	// interrupts it before its process group is killed; zero is the cancel
	// ladder's interrupt grace.
	Grace time.Duration
	Log   *slog.Logger

	once   sync.Once
	mu     sync.Mutex
	active map[runKey]*activeRun
	wg     sync.WaitGroup
}

var _ Executor = (*Exec)(nil)

type runKey struct{ connection, run string }

// activeRun is a run the executor has in hand. It is the seam the cancel
// ladder (DEV-8) acts through: Control finds the run here and reaches its turn.
type activeRun struct {
	mu     sync.Mutex
	turn   adapter.Turn
	cancel context.CancelFunc
}

func (e *Exec) init() {
	e.once.Do(func() {
		e.active = map[runKey]*activeRun{}
		if e.Binary == nil {
			e.Binary = harness.Locate
		}
		if e.Grace <= 0 {
			e.Grace = supervise.DefaultLadder.InterruptGrace
		}
		if e.Log == nil {
			e.Log = slog.New(slog.DiscardHandler)
		}
	})
}

// Start hands the run to its own goroutine and returns at once, as the sync
// loop requires.
func (e *Exec) Start(ctx context.Context, c Claim) {
	e.init()
	key := runKey{c.Connection, c.Run.RunID}
	a := &activeRun{}
	e.mu.Lock()
	e.active[key] = a
	e.mu.Unlock()
	e.wg.Go(func() {
		defer func() {
			e.mu.Lock()
			delete(e.active, key)
			e.mu.Unlock()
			c.Release()
		}()
		e.execute(ctx, c, a)
	})
}

// Wait blocks until every run started has finished or been stopped with the
// runner.
func (e *Exec) Wait() { e.wg.Wait() }

// Control receives the hub's instructions for runs. Acting on them — the
// interrupt, the cancel ladder, steering — is DEV-8; until then an instruction
// for a run in hand is logged, so an operator can see the hub asked.
func (e *Exec) Control(_ context.Context, connection string, c v1.Control) {
	e.init()
	switch c.Kind {
	case v1.ControlCancel, v1.ControlInterrupt, v1.ControlSteer:
		e.mu.Lock()
		_, ok := e.active[runKey{connection, c.RunID}]
		e.mu.Unlock()
		if !ok {
			// Most often a finished run still listed while its result is on
			// its way; the hub's answer to it is already settled.
			return
		}
		e.Log.Warn("the hub sent a control this runner does not act on yet; the run carries on (DEV-8)",
			"connection", connection, "run", c.RunID, "kind", c.Kind)
	case v1.ControlCloseSession, v1.ControlDrain:
		e.Log.Warn("the hub sent a control this runner does not act on yet (epics E3, E4)", "connection", connection, "kind", c.Kind)
	}
}

// execute runs one claim to a terminal state in the outbox — or, when the
// runner itself is stopping, leaves it held for the next start to settle.
func (e *Exec) execute(ctx context.Context, c Claim, a *activeRun) {
	// Local bookkeeping must land even as the runner stops: a result half
	// written is worse than one never started.
	bg := context.WithoutCancel(ctx)
	run := c.Run
	log := e.Log.With("connection", c.Connection, "run", run.RunID)
	started := time.Now()
	fail := func(class, msg string) {
		e.finish(bg, c, v1.Result{State: v1.RunFailed, Error: &v1.RunError{Class: class, Message: msg},
			Metrics: v1.Metrics{DurationMS: time.Since(started).Milliseconds()}})
	}

	e.setState(bg, c, v1.RunPreparing)
	ad, ok := e.Adapters.Lookup(run.Harness)
	if !ok {
		fail(ClassRefused, fmt.Sprintf("this runner has no adapter for harness %q — it should not have advertised it; report this as a yad bug", run.Harness))
		return
	}
	bin, ok := e.Binary(run.Harness)
	if !ok {
		fail(ClassStart, fmt.Sprintf("harness %q is not installed on this runner any more — `yad doctor` shows where it was looked for", run.Harness))
		return
	}
	workdir, native, err := e.workdir(bg, c)
	if err != nil {
		fail(ClassPrepare, "the workdir could not be prepared: "+err.Error())
		return
	}
	env, cleanup, err := e.grants(c)
	defer cleanup()
	if err != nil {
		fail(ClassPrepare, err.Error())
		return
	}
	spec := adapter.Spec{
		RunID: run.RunID, Model: run.Model, Workdir: workdir,
		SessionID: run.Session.ID, NativeSessionID: native, Brief: run.Brief,
		Env: env, Binary: bin, Settings: settings(e.Config.Harness[run.Harness]),
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	turn, err := ad.Start(runCtx, spec)
	if err != nil {
		fail(ClassStart, err.Error())
		return
	}
	a.mu.Lock()
	a.turn, a.cancel = turn, cancel
	a.mu.Unlock()
	// Running only once the workdir exists and the harness is up (§2).
	e.setState(bg, c, v1.RunRunning)
	log.Info("run started", "harness", run.Harness, "model", run.Model, "workdir", workdir)

	w := e.stream(bg, c, turn, cancel, native, started)
	out := turn.Wait()
	if out.NativeSessionID != "" && out.NativeSessionID != w.native {
		e.setNative(bg, c, out.NativeSessionID)
	}
	if ctx.Err() != nil && w.stopped == "" && out.State == v1.RunCancelled {
		// Stopped because the runner is stopping, not because the run ended:
		// no result is owed yet. The next start settles it (E3).
		log.Warn("run stopped with the runner; it stays held")
		return
	}
	e.finish(bg, c, e.result(out, w, started))
}

// watch is what streaming a turn observed.
type watch struct {
	// stopped is the watchdog class that stopped the turn, or "".
	stopped      string
	stoppedMsg   string
	stalls       int
	firstEventMS int64
	toolCalls    int
	lastSeq      int64
	native       string
}

// stream spools the turn's events until it closes them, under the two
// watchdogs. A watchdog that fires interrupts the turn, which keeps the session
// resumable; a harness that has not ended its turn Grace later loses its
// process group.
func (e *Exec) stream(ctx context.Context, c Claim, turn adapter.Turn, kill context.CancelFunc, native string, started time.Time) watch {
	w := watch{firstEventMS: -1, native: native}
	idleFor := e.inactivity(c.Run)
	idle := time.NewTimer(idleFor)
	defer idle.Stop()
	var wall <-chan time.Time
	if c.Run.WallClockMS > 0 {
		t := time.NewTimer(time.Duration(c.Run.WallClockMS) * time.Millisecond)
		defer t.Stop()
		wall = t.C
	}
	var grace <-chan time.Time
	stop := func(class, msg string) {
		if w.stopped != "" {
			return
		}
		w.stopped, w.stoppedMsg = class, msg
		e.Log.Warn("watchdog stopping run", "connection", c.Connection, "run", c.Run.RunID, "class", class)
		// A failed interrupt only means the kill comes sooner in effect; the
		// grace timer below does not depend on the harness cooperating.
		_ = turn.Interrupt()
		grace = time.After(e.Grace)
	}
	unreported := 0
	for {
		select {
		case ev, ok := <-turn.Events():
			if !ok {
				return w
			}
			// Since Go 1.23 a Reset leaves no stale fire to drain.
			idle.Reset(idleFor)
			if w.firstEventMS < 0 {
				w.firstEventMS = time.Since(started).Milliseconds()
			}
			if ev.Kind == v1.EventToolCall {
				w.toolCalls++
			}
			if e.spool(ctx, c, &ev, w.lastSeq+1) {
				w.lastSeq = ev.Seq
				if unreported++; unreported >= eventBatch {
					e.report(c.Connection)
					unreported = 0
				}
			}
			if id := nativeID(turn); id != "" && id != w.native {
				w.native = id
				e.setNative(ctx, c, id)
			}
		case <-idle.C:
			w.stalls++
			stop(ClassInactivity, fmt.Sprintf("the harness produced no event for %s and was stopped", idleFor))
		case <-wall:
			stop(ClassWallClock, fmt.Sprintf("the run reached its wall-clock cap of %s and was stopped", time.Duration(c.Run.WallClockMS)*time.Millisecond))
		case <-grace:
			grace = nil
			kill()
		}
	}
}

// nativeID reads the harness's session id mid-turn from an adapter that
// reveals it (the Claude adapter does from its first line), so a crash does
// not lose the resume pointer.
func nativeID(t adapter.Turn) string {
	if n, ok := t.(interface{ NativeSessionID() string }); ok {
		return n.NativeSessionID()
	}
	return ""
}

// spool numbers an event and writes it to the spool. The number is taken only
// when the write succeeds: a gap in a run's sequence would hold the hub's
// acked_through below it forever.
func (e *Exec) spool(ctx context.Context, c Claim, ev *v1.Event, seq int64) bool {
	ev.Seq = seq
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	ev.Text, _ = capBytes(ev.Text, maxTextBytes)
	if ev.Error != nil {
		e := *ev.Error
		e.Message, _ = capBytes(e.Message, maxTextBytes)
		ev.Error = &e
	}
	if ev.Tool != nil {
		t := *ev.Tool
		var cut bool
		t.Input, cut = capBytes(t.Input, v1.MaxToolOutputBytes)
		t.Truncated = t.Truncated || cut
		t.Output, cut = capBytes(t.Output, v1.MaxToolOutputBytes)
		t.Truncated = t.Truncated || cut
		ev.Tool = &t
	}
	body, err := json.Marshal(ev)
	if err == nil {
		err = e.Store.AppendEvent(ctx, db.AppendEventParams{Connection: c.Connection, RunID: c.Run.RunID, Seq: seq, Body: string(body)})
	}
	if err != nil {
		e.Log.Error("event not spooled; it is lost", "connection", c.Connection, "run", c.Run.RunID, "kind", ev.Kind, "err", err)
		return false
	}
	return true
}

// capBytes holds a string to n bytes without splitting a rune. Adapters cap
// tool payloads too; this is the last line, because a hub stores what it gets.
func capBytes(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// result turns the adapter's outcome into the protocol's terminal report. A
// watchdog's verdict wins over whatever the stopped harness said last.
func (e *Exec) result(out adapter.Outcome, w watch, started time.Time) v1.Result {
	res := v1.Result{
		State: out.State, FinalText: out.FinalText, Error: out.Error, LastSeq: w.lastSeq,
		Metrics: v1.Metrics{
			DurationMS: time.Since(started).Milliseconds(), FirstEventMS: max(w.firstEventMS, 0),
			ToolCalls: w.toolCalls, APIRetries: out.APIRetries, Stalls: w.stalls,
		},
	}
	if len(out.Usage) > 0 {
		res.Usage.ByModel = out.Usage
	}
	switch {
	case w.stopped != "":
		res.State, res.Error = v1.RunTimedOut, &v1.RunError{Class: w.stopped, Message: w.stoppedMsg}
	case !out.State.IsTerminal() || out.State == v1.RunLost:
		// Lost is the hub's to decide, and a turn that is over is not waiting.
		res.State = v1.RunFailed
		res.Error = &v1.RunError{Class: ClassAdapter, Message: fmt.Sprintf("the adapter ended the turn in state %q, which is not a result — report this as a yad bug", out.State)}
	}
	return res
}

// finish records the terminal state and the result together, so the result is
// in the outbox before the first attempt to send it and a crash between the
// two cannot leave a finished run with nothing owed.
func (e *Exec) finish(ctx context.Context, c Claim, res v1.Result) {
	log := e.Log.With("connection", c.Connection, "run", c.Run.RunID)
	res.FinalText, _ = capBytes(res.FinalText, maxTextBytes)
	if res.Error != nil {
		e := *res.Error
		e.Message, _ = capBytes(e.Message, maxTextBytes)
		res.Error = &e
	}
	body, err := json.Marshal(res)
	if err != nil {
		log.Error("result not recorded", "err", err)
		return
	}
	var reason sql.NullString
	if res.Error != nil {
		reason = sql.NullString{String: res.Error.Message, Valid: true}
	}
	now := time.Now().UnixMilli()
	err = e.Store.Tx(ctx, func(q *db.Queries) error {
		if err := q.SetRunState(ctx, db.SetRunStateParams{
			State: string(res.State), Reason: reason, UpdatedAt: now, Connection: c.Connection, ID: c.Run.RunID,
		}); err != nil {
			return err
		}
		return q.PutOutbox(ctx, db.PutOutboxParams{Connection: c.Connection, RunID: c.Run.RunID, Body: string(body), NextAttemptAt: now})
	})
	if err != nil {
		log.Error("result not recorded; the run stays held", "err", err)
		return
	}
	log.Info("run finished", "state", res.State, "last_seq", res.LastSeq)
	e.report(c.Connection)
}

func (e *Exec) setState(ctx context.Context, c Claim, s v1.RunState) {
	if err := e.Store.SetRunState(ctx, db.SetRunStateParams{
		State: string(s), UpdatedAt: time.Now().UnixMilli(), Connection: c.Connection, ID: c.Run.RunID,
	}); err != nil {
		e.Log.Error("run state not recorded", "connection", c.Connection, "run", c.Run.RunID, "state", s, "err", err)
	}
}

func (e *Exec) setNative(ctx context.Context, c Claim, id string) {
	if err := e.Store.SetSessionNativeID(ctx, db.SetSessionNativeIDParams{
		NativeID: sql.NullString{String: id, Valid: true}, LastUsedAt: time.Now().UnixMilli(),
		Connection: c.Connection, ID: c.Run.Session.ID,
	}); err != nil {
		e.Log.Error("native session id not recorded; the session may not resume", "connection", c.Connection, "session", c.Run.Session.ID, "err", err)
	}
}

func (e *Exec) report(connection string) {
	if e.Report != nil {
		e.Report(connection)
	}
}

// inactivity is the owner's timeout, lowered — never raised — by the run.
func (e *Exec) inactivity(run v1.Run) time.Duration {
	d := e.Config.Supervise.Inactivity.Duration
	if d <= 0 {
		d = config.DefaultInactivity
	}
	if run.InactivityMS > 0 {
		d = min(d, time.Duration(run.InactivityMS)*time.Millisecond)
	}
	return d
}

// workdir returns the session's workdir, creating it on first use, and the
// session's native id. E2 workdirs are empty directories; sources and the
// setup hook are E4.
func (e *Exec) workdir(ctx context.Context, c Claim) (dir, native string, err error) {
	sess, err := e.Store.GetSession(ctx, db.GetSessionParams{Connection: c.Connection, ID: c.Run.Session.ID})
	if err != nil {
		return "", "", err
	}
	dir = sess.Workdir
	if dir == "" {
		dir = filepath.Join(e.Data, "workdirs", c.Connection, pathName(c.Run.Session.ID))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	if err := e.Store.SetSessionWorkdir(ctx, db.SetSessionWorkdirParams{
		Workdir: dir, LastUsedAt: time.Now().UnixMilli(), Connection: c.Connection, ID: c.Run.Session.ID,
	}); err != nil {
		return "", "", err
	}
	return dir, sess.NativeID.String, nil
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// pathName turns a hub-chosen id into one path component. A hub is untrusted
// input: an id like "../../.ssh" must not name a directory. A readable id is
// kept; anything else becomes a hash, prefixed with a character readable ids
// cannot start with, so the two can never collide.
func pathName(id string) string {
	if safeName.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "_" + hex.EncodeToString(sum[:12])
}

var grantName = regexp.MustCompile(`^[A-Z][A-Z0-9_]*_(TOKEN|KEY|SECRET|PASSWORD|CREDENTIAL|CREDENTIALS)$`)

// grantPrefixes are namespaces a grant may not use even with a secret-shaped
// name: the loader's, the runner's, and the harnesses' own, where a key moves
// billing or configuration (ANTHROPIC_API_KEY, OPENAI_API_KEY).
var grantPrefixes = []string{"LD_", "DYLD_", "YAD_", "CLAUDE", "ANTHROPIC_", "CODEX_", "OPENAI_", "GIT_", "NODE_", "NPM_CONFIG_", "BUN_"}

// grantAllowed is whether a hub may set this variable. A grant is a secret
// for the run to use, never a way to steer what runs or how (decision 0015),
// and the variables that steer — PATH, proxies, CA bundles, shell options,
// loader and runtime hooks — are too many to list. So the rule is an
// allowlist of shape: upper case, named as the secret it is.
func grantAllowed(name string) bool {
	if !grantName.MatchString(name) {
		return false
	}
	for _, p := range grantPrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}

// grants delivers the run's grants: an env grant as NAME=value, a file grant
// as a 0600 file whose path is NAME. Never argv. cleanup deletes the files and
// is always safe to call.
func (e *Exec) grants(c Claim) (env []string, cleanup func(), err error) {
	cleanup = func() {}
	var dir string
	for _, g := range c.Run.Grants {
		if !grantAllowed(g.Name) {
			return nil, cleanup, fmt.Errorf("grant %q is not a name a grant may use — a grant is named as the secret it is, in upper case and ending in _TOKEN, _KEY, _SECRET, _PASSWORD or _CREDENTIAL(S), such as ZUMINO_TOKEN; harness, runtime and loader namespaces are refused", g.Name)
		}
		switch g.As {
		case v1.GrantEnv:
			env = append(env, g.Name+"="+g.Value)
		case v1.GrantFile:
			if dir == "" {
				dir = filepath.Join(e.Data, "grants", c.Connection, pathName(c.Run.RunID))
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return nil, cleanup, fmt.Errorf("grant directory: %w", err)
				}
				cleanup = func() {
					if err := os.RemoveAll(dir); err != nil {
						e.Log.Error("grant files not removed — delete them by hand", "dir", dir, "err", err)
					}
				}
			}
			path := filepath.Join(dir, g.Name)
			if err := os.WriteFile(path, []byte(g.Value), 0o600); err != nil {
				return nil, cleanup, fmt.Errorf("grant %s: %w", g.Name, err)
			}
			env = append(env, g.Name+"="+path)
		default:
			return nil, cleanup, fmt.Errorf("grant %q is delivered as %q, which is neither env nor file", g.Name, g.As)
		}
	}
	return env, cleanup, nil
}

// settings is the owner's harness configuration as the adapter reads it.
func settings(h config.HarnessConfig) map[string]string {
	s := map[string]string{}
	for k, v := range map[string]string{"permission_mode": h.PermissionMode, "sandbox": h.Sandbox, "approval": h.Approval} {
		if v != "" {
			s[k] = v
		}
	}
	return s
}
