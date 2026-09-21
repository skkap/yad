package runner

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
	"unicode/utf8"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
	"github.com/skkap/yad/internal/supervise"
	"github.com/skkap/yad/internal/workdir"
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
	// ClassSteer — a steer from the hub did not reach the harness; the run
	// carries on without it. Only ever an event, never a result.
	ClassSteer = "steer_failed"
	// ClassInterrupt — an interrupt from the hub did not reach the harness.
	// Only ever an event, never a result.
	ClassInterrupt = "interrupt_failed"
	// ClassRunnerStopping — the runner cancelled the run on its way down: a
	// drain ran out of time, or its owner asked twice (decision 0029). The
	// state is cancelled; the class says it was not the hub's doing.
	ClassRunnerStopping = "runner_stopping"
	// ClassSessionClosed — the run names a session this runner has closed,
	// or is closing, and whose workdir is or will be gone (decision 0035).
	// The hub starts a new session.
	ClassSessionClosed = "session_closed"
	// ClassRunnerRestarted — the run's process ended with a runner that
	// stopped without finishing it; the next start reports it lost
	// (decision 0030).
	ClassRunnerRestarted = "runner_restarted"
	// ClassMaxWait — the run waited for a free account longer than the hub
	// allowed it (Run.MaxWaitMS). The state is timed_out.
	ClassMaxWait = "max_wait_exceeded"
	// ClassGrantsLost — a run parked on a usage limit was picked up by a
	// later process, and the grants it was given did not survive: they live
	// in the process that claimed them and never touch this machine's disk.
	// The hub offers the run again, with its grants.
	ClassGrantsLost = "grants_lost"
	// ClassResumeRejected — the run continued a session and the harness had
	// no conversation to continue: the transcript is gone. The session's
	// context cannot come back on this runner; the hub starts a new session
	// (decision 0031).
	ClassResumeRejected = "resume_rejected"
)

// hubClass is the class a hub sees for an adapter's. The adapters name what
// the harness said; the hub needs what it can do about it, and "the harness
// has no such session" on a resume is the one case where the two differ.
// session_mismatch passes through as itself (decision 0031).
func hubClass(class string, resumed bool) string {
	if resumed && class == adapter.ClassSessionNotFound {
		return ClassResumeRejected
	}
	return class
}

// maxTextBytes caps an event's text and error message and a result's final
// text and error message, each. The protocol caps only tool payloads, but a
// hub or a proxy before it limits a body, and a report too large to fit is
// retried for ever. At a MiB a field, one event or one result stays well under
// yad hub's 16 MiB limit; a proxy with a limit near 1 MiB can still refuse an
// outlier. A MiB of prose is past anything a person reads from a stream.
const maxTextBytes = 1 << 20

// maxStatusBytes caps an event's status. A status is a word or a line — a
// phase, a declined command — but some carry the harness's own text, and a
// harness can write a line of 32 MiB.
const maxStatusBytes = 1 << 10

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
	// Paths is the runner's profile, which every yad command a message offers
	// has to act on: pasted without it, the command acts on the default
	// runner, or on an empty one. A run's error goes to a hub, so those
	// commands are Paths.RemoteCommand, which names no directory.
	Paths config.Paths
	// Workdirs turns a run's sources into its workdir; nil is one built from
	// Config's [workdirs] and Data.
	Workdirs *workdir.Manager
	// Binary resolves a harness to its executable; nil is harness.Locate, the
	// lookup detection uses.
	Binary func(harness string) (string, bool)
	// Report wakes a connection's reporter, so a result or a full batch goes
	// out now rather than at the next tick. Nil is fine: the tick finds it.
	Report func(connection string)
	// Ended hears that a run's terminal state is in the store: a close of
	// its session may have been waiting on it. Nil is fine: the collector's
	// next sweep finds it.
	Ended func()
	// Grace is how long a harness gets to end its turn once interrupted —
	// by a cancel or a watchdog — before its process group gets SIGTERM, and
	// TermGrace how long after that before SIGKILL. Zero is the cancel
	// ladder's own (supervise.DefaultLadder).
	Grace     time.Duration
	TermGrace time.Duration
	Log       *slog.Logger

	once   sync.Once
	mu     sync.Mutex
	active map[runKey]*activeRun
	// parked are the runs this process put in waiting, kept whole — grants
	// included — so a resume here loses nothing. The row in the store is what
	// makes a run resumable at all; this only spares a resume in this process
	// the one thing the row cannot hold.
	parked map[runKey]v1.Run
	// idle are waiters closed when the last active run ends.
	idle []chan struct{}
	wg   sync.WaitGroup
}

var _ Executor = (*Exec)(nil)

type runKey struct{ connection, run string }

// activeRun is a run the executor has in hand, and how the hub's controls
// reach it: Control finds the run here, and the run's own goroutine acts on
// them, so a control never races the turn it is aimed at.
type activeRun struct {
	mu sync.Mutex
	// started is set once the harness is up; before that there is no turn to
	// interrupt, and an interrupt ends the run as a cancel does.
	started bool
	// cancelled is closed by the first cancel, or by an interrupt before
	// the harness is up; cancelAt is when it arrived.
	cancelled chan struct{}
	cancelAt  time.Time
	// byRunner is why the runner itself cancelled the run, when it was the
	// runner's own way down rather than the hub.
	byRunner string
	// controls carries interrupts and steers to the running turn. A steer
	// sent while the run prepares waits here for the harness.
	controls chan control
}

// control is an interrupt or a steer, with when the runner received it.
type control struct {
	v1.Control
	at time.Time
}

// pendingControls bounds the interrupts and steers waiting for one run. A hub
// that sends more before the run can take them has lost track of it; the
// excess is dropped and logged.
const pendingControls = 16

func newActiveRun() *activeRun {
	return &activeRun{cancelled: make(chan struct{}), controls: make(chan control, pendingControls)}
}

// stop cancels the run — always for a cancel, and for an interrupt only while
// the harness is not up — and reports whether it did (ok) and whether this was
// the first time (first). One lock covers the check and the close, so an
// interrupt is never lost between the two.
func (a *activeRun) stop(at time.Time, cancel bool) (first, ok bool) {
	return a.stopBy(at, cancel, "")
}

// stopBy is stop, saying why when the runner is the one stopping it.
func (a *activeRun) stopBy(at time.Time, cancel bool, byRunner string) (first, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !cancel && a.started {
		return false, false
	}
	select {
	case <-a.cancelled:
		return false, true
	default:
	}
	a.cancelAt, a.byRunner = at, byRunner
	close(a.cancelled)
	return true, true
}

// stoppedByRunner is the runner's reason for cancelling the run, or "".
func (a *activeRun) stoppedByRunner() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.byRunner
}

// cancelledAt reports whether the run was cancelled, and when.
func (a *activeRun) cancelledAt() (time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	select {
	case <-a.cancelled:
		return a.cancelAt, true
	default:
		return time.Time{}, false
	}
}

func (e *Exec) init() {
	e.once.Do(func() {
		e.active = map[runKey]*activeRun{}
		e.parked = map[runKey]v1.Run{}
		if e.Binary == nil {
			e.Binary = harness.Locate
		}
		if e.Grace <= 0 {
			e.Grace = supervise.DefaultLadder.InterruptGrace
		}
		if e.TermGrace <= 0 {
			e.TermGrace = supervise.DefaultLadder.TermGrace
		}
		if e.Log == nil {
			e.Log = slog.New(slog.DiscardHandler)
		}
		if e.Workdirs == nil {
			w := e.Config.Workdirs
			e.Workdirs = &workdir.Manager{
				Data: e.Data, Roots: w.EffectiveRoots(), GitTimeout: w.GitTimeout.Duration, SetupTimeout: w.SetupTimeout.Duration,
				Slots: e.Store,
			}
		}
	})
}

// Start hands the run to its own goroutine and returns at once, as the sync
// loop requires.
func (e *Exec) Start(ctx context.Context, c Claim) {
	e.init()
	key := runKey{c.Connection, c.Run.RunID}
	a := newActiveRun()
	e.mu.Lock()
	e.active[key] = a
	e.mu.Unlock()
	e.wg.Go(func() {
		defer func() {
			e.mu.Lock()
			delete(e.active, key)
			if len(e.active) == 0 {
				for _, ch := range e.idle {
					close(ch)
				}
				e.idle = nil
			}
			e.mu.Unlock()
			c.Release()
		}()
		e.execute(ctx, c, a)
	})
}

// Wait blocks until every run started has finished or been stopped with the
// runner.
func (e *Exec) Wait() { e.wg.Wait() }

// Idle is closed once no run is in hand — at once if none is now. Unlike Wait
// it may be asked while runs are still being started; a run started after it
// closed needs a new ask.
func (e *Exec) Idle() <-chan struct{} {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	ch := make(chan struct{})
	if len(e.active) == 0 {
		close(ch)
		return ch
	}
	e.idle = append(e.idle, ch)
	return ch
}

// CancelAll cancels every run in hand down the cancel ladder, as a cancel
// from the hub would, and says in each result that the runner did it.
func (e *Exec) CancelAll(reason string) {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	now := time.Now()
	for k, a := range e.active {
		if first, _ := a.stopBy(now, true, reason); first {
			e.Log.Warn("the runner is cancelling the run on its way down", "connection", k.connection, "run", k.run, "reason", reason)
		}
	}
}

// Control receives the hub's instructions for runs. It only hands them over —
// the run's own goroutine acts — so it never blocks the sync loop.
//
// A cancel is delivered on every sync until the run ends (decision 0025), so
// only the first counts. An interrupt before the harness is up has no turn to
// end: the run ends as cancelled, never started, which keeps the session as
// an interrupt would.
func (e *Exec) Control(_ context.Context, connection string, c v1.Control) {
	e.init()
	switch c.Kind {
	case v1.ControlCancel, v1.ControlInterrupt, v1.ControlSteer:
		e.mu.Lock()
		a, ok := e.active[runKey{connection, c.RunID}]
		e.mu.Unlock()
		if !ok {
			// Most often a finished run still listed while its result is on
			// its way; the hub's answer to it is already settled.
			return
		}
		log := e.Log.With("connection", connection, "run", c.RunID, "kind", c.Kind)
		now := time.Now()
		if c.Kind == v1.ControlCancel || c.Kind == v1.ControlInterrupt {
			if first, ok := a.stop(now, c.Kind == v1.ControlCancel); ok {
				if first {
					log.Info("the hub stopped the run")
				}
				return
			}
		}
		select {
		case a.controls <- control{Control: c, at: now}:
		default:
			log.Warn("too many controls are waiting for this run; this one is dropped")
		}
	}
}

// seeLogs ends a run error whose cause stays on the machine. The failures it
// ends — a harness that will not start, a directory under the runner's data
// that cannot be made — are the runner's, not the run's, and their errors name
// absolute paths under the owner's home and, for a start, the exec error
// (DEV-67, as DEV-60 found for the capability document). The hub is told what
// failed; the owner reads why in the log line written beside it.
func (e *Exec) seeLogs() string {
	return " — its owner can see why with `" + e.Paths.RemoteCommand("daemon", "logs") + "` on the machine"
}

// execute runs one claim to a terminal state in the outbox — or, when the
// runner itself is stopping, leaves it held for the next start to settle.
func (e *Exec) execute(ctx context.Context, c Claim, a *activeRun) {
	// Local bookkeeping must land even as the runner stops: a result half
	// written is worse than one never started.
	bg := context.WithoutCancel(ctx)
	run := c.Run
	log := e.Log.With("connection", c.Connection, "run", run.RunID)
	// What the run has cost so far. A run that parked on a usage limit and
	// came back is the same run: its duration, its wait and the accounts it
	// has been through carry over, and reading them from the row is the only
	// way that holds when the run came back in another process.
	prog := e.progress(bg, c)
	// lastSeq continues the run's own stream. It is read rather than started
	// at zero for the same reason: a resumed run already has events, and
	// numbering the next one 1 would collide with the first and be dropped.
	lastSeq := e.lastSeq(bg, c)
	fail := func(class, msg string) {
		e.finish(bg, c, v1.Result{State: v1.RunFailed, Error: &v1.RunError{Class: class, Message: msg}, LastSeq: lastSeq,
			Usage: v1.RunUsage{ByModel: prog.spent.Usage}, Metrics: prog.metrics(time.Now())})
	}
	// A run stopped before its harness is up is cancelled with nothing
	// spawned: no process, no events, and the session as it was.
	stoppedEarly := func() bool {
		at, ok := a.cancelledAt()
		if !ok {
			return false
		}
		log.Info("run cancelled before it started")
		m := prog.metrics(time.Now())
		m.CancelLatencyMS = latency(at)
		e.finish(bg, c, v1.Result{State: v1.RunCancelled, Error: runnerStopped(a.stoppedByRunner()), LastSeq: lastSeq,
			Usage: v1.RunUsage{ByModel: prog.spent.Usage}, Metrics: m})
		return true
	}

	// A start time is a moment the run must not start before; the hub may
	// hand the run over early so it starts on time. It waits here, claimed
	// and holding its capacity.
	if run.StartAt != nil {
		if d := time.Until(*run.StartAt); d > 0 {
			log.Info("run waits for its start time", "start_at", run.StartAt.UTC())
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-a.cancelled:
				t.Stop()
				stoppedEarly()
				return
			case <-ctx.Done():
				t.Stop()
				log.Warn("runner stopped before the run's start time; the next start reports it lost")
				return
			}
			// The run's duration is from its start, not from its claim.
			prog.started = time.Now()
		}
	}
	e.setState(bg, c, v1.RunPreparing)
	// Stamped once, on the first preparing: a run that waits between two
	// turns reports how long the whole of it took, not how long its last
	// attempt did.
	e.setStarted(bg, c, prog.started)
	// The claim checked the run already; checked again here because this is
	// where a grant's name becomes a variable and a file (decision 0038).
	if err := run.Validate(); err != nil {
		fail(ClassRefused, "the run is invalid: "+err.Error())
		return
	}
	ad, ok := e.Adapters.Lookup(run.Harness)
	if !ok {
		fail(ClassRefused, fmt.Sprintf("this runner has no adapter for harness %q — it should not have advertised it; report this as a yad bug", run.Harness))
		return
	}
	bin, ok := e.Binary(run.Harness)
	if !ok {
		fail(ClassStart, fmt.Sprintf("harness %q is not installed on this runner any more — `%s` shows where it was looked for", run.Harness, e.Paths.RemoteCommand("doctor")))
		return
	}
	// Which account the run uses. Chosen here, beside the adapter and binary
	// lookups, because it is a config read and one query and it can refuse the
	// run: the workdir below clones or fetches a repository, and paying for
	// that before discovering no account can take the turn is work thrown
	// away on every run a limited runner is offered.
	//
	// The free account whose window resets soonest goes first (decision
	// 0039); a limited account and one that needs login are skipped alike. A
	// harness the owner gave no accounts runs on the harness's own default
	// home, exactly as every installation did before accounts existed — no
	// accounts is a state, not a failure.
	acct, hasAccount, err := e.pickAccount(bg, run.Harness)
	if err != nil {
		if e.park(bg, c, &prog, &lastSeq, err.Error()) {
			return
		}
		// A run the runner will not take is refused (§2), not a preparation
		// that went wrong: nothing was wrong with the run, and a hub may
		// offer it to a runner whose accounts can take it. Reached only when
		// no reset ends the wait — every account of the harness needs a
		// login — because a limited one parks the run instead.
		fail(ClassRefused, err.Error())
		return
	}
	dir, native, err := e.workdir(bg, c)
	if err != nil {
		log.Warn("the session's workdir could not be made", "err", err)
		fail(ClassPrepare, "the session's workdir could not be made on this runner"+e.seeLogs())
		return
	}
	prep, err := e.prepare(ctx, c, a, dir, &lastSeq)
	if err != nil {
		if stoppedEarly() {
			return
		}
		if ctx.Err() != nil {
			log.Warn("runner stopped while the run was preparing; the next start reports it lost")
			return
		}
		class := ClassPrepare
		if we, ok := errors.AsType[*workdir.Error](err); ok {
			class = we.Class
		}
		log.Warn("the workdir could not be prepared", "class", class, "err", err)
		fail(class, err.Error())
		return
	}
	// A path source stays locked until the run is over, so another run on
	// the same directory waits for this one. A run that parks gives the lock
	// up with everything else it holds: a lock is a property of a live
	// process, so one held across a park could not survive the restart the
	// park exists to survive, and holding a directory for five hours while
	// nothing runs in it is the opposite of what it is for.
	defer prep.Release()
	env, cleanup, err := e.grants(c)
	defer cleanup()
	if err != nil {
		log.Warn("the run's grants could not be delivered", "err", err)
		fail(ClassPrepare, "the run's grants could not be delivered on this runner"+e.seeLogs())
		return
	}

	// One turn per account. A usage limit is the one outcome that is not the
	// run's answer: the account is out of quota, the transcript is the whole
	// of the session and lives where every account home can read it, so the
	// work continues on another account in the same session (decision 0013,
	// measured in DEV-24). Anything else ends the run here.
	//
	// tried is the backstop under that loop. What normally ends it is the
	// store — each limit is recorded before the next account is chosen, so
	// the pick cannot return the same one — but a write that failed is
	// logged and carried on from, and without this the run would be handed
	// back the account it just exhausted, for ever.
	tried := map[string]bool{}
	// lastLimit is what moved the run off the previous account, so the move
	// event can name the window that ran out. Nil on the first turn and on a
	// turn after a park, where the wait's own event already said why.
	var lastLimit *adapter.Limit
	for {
		turnLog := log
		if hasAccount {
			turnLog = log.With("account", acct.Label)
		}
		var home string
		if hasAccount {
			if home, err = account.Ensure(e.Data, run.Harness, acct.Label); err != nil {
				turnLog.Warn("the account's harness home could not be prepared", "err", err)
				fail(ClassPrepare, "the account's harness home could not be prepared on this runner"+e.seeLogs())
				return
			}
			// One place counts a move, so that the two kinds are counted the
			// same way: a limit that moves the run in this loop, and a run
			// that parked on one account and was resumed on another. The
			// second is the one that used to be missed — the resumed run
			// reads its last account back from its row, so a process that
			// never saw the first turn still knows it moved.
			if prog.account != "" && prog.account != acct.Label {
				prog.switches++
				e.noteMove(bg, c, &lastSeq, prog.account, acct.Label, lastLimit)
				turnLog.Info("the run continues on another account in the same session",
					"from_account", prog.account, "account_switches", prog.switches)
			}
			prog.account = acct.Label
			e.setRunAccount(bg, c, acct.Label)
			// The label and nothing else. Which account ran a turn is how an
			// owner tells two subscriptions' work apart, and the label is the
			// only thing about an account that may leave this machine.
			ev := v1.Event{Kind: v1.EventStatus, Status: "account", Text: acct.Label}
			if e.spool(bg, c, &ev, lastSeq+1) {
				lastSeq = ev.Seq
				e.report(c.Connection)
			}
		}
		// Built fresh each turn: the home variable belongs to this account,
		// and appending it to env would carry the last account's home into
		// the next turn's environment.
		turnEnv := append(append([]string(nil), env...), account.Env(run.Harness, home)...)

		spec := adapter.Spec{
			RunID: run.RunID, Model: run.Model, Workdir: prep.Dir,
			SessionID: run.Session.ID, NativeSessionID: native, Brief: run.Brief,
			Home: home, Env: turnEnv, Binary: bin, Settings: settings(e.Config.Harness[run.Harness]),
			Yad: e.Paths.RemoteCommand,
		}
		if hasAccount {
			spec.HomeVar, spec.Account = account.HomeVar(run.Harness), acct.Label
		}

		if stoppedEarly() {
			return
		}
		// The cap is the run's, and earlier turns have already spent part of
		// it. A run that has none left does not get one more turn to find
		// that out in.
		wallLeft, capped := prog.wallClockLeft(run)
		if capped && wallLeft <= 0 {
			turnLog.Warn("the run has used its wall-clock cap across its turns; it is not started again")
			e.finish(bg, c, v1.Result{
				State: v1.RunTimedOut, LastSeq: lastSeq,
				Error: &v1.RunError{Class: ClassWallClock, Message: fmt.Sprintf(
					"the run reached its wall-clock cap of %s across the accounts it ran on and was stopped",
					time.Duration(run.WallClockMS)*time.Millisecond)},
				Usage:   v1.RunUsage{ByModel: prog.spent.Usage},
				Metrics: prog.metrics(time.Now()),
			})
			return
		}
		turnStarted := time.Now()
		runCtx, cancel := context.WithCancel(ctx)
		turn, err := ad.Start(runCtx, spec)
		if err != nil {
			cancel()
			// Only a LocalError hides its cause. The adapters' other start
			// errors are sentences whose next action may be the hub's — a
			// model name it sent that is not one, a session to close — and a
			// hub told only to ask the owner would keep sending the same run.
			if le, ok := errors.AsType[*adapter.LocalError](err); ok {
				turnLog.Warn("the harness would not start", "err", le.Err)
				fail(ClassStart, le.Msg+e.seeLogs())
				return
			}
			fail(ClassStart, err.Error())
			return
		}
		// Pinned before the first event: an adapter that chooses the id (Claude)
		// knows it at spawn, and a crash between the spawn and the first line
		// must not leave the session pointing nowhere (ARCHITECTURE.md §3).
		if id := turn.NativeSessionID(); id != "" && id != native {
			e.setNative(bg, c, id)
			native = id
		}
		// From here an interrupt reaches the turn; one that arrived while the
		// harness started closed cancelled instead, which the stream sees at once.
		a.mu.Lock()
		a.started = true
		a.mu.Unlock()
		// Running only once the workdir exists and the harness is up (§2).
		e.setState(bg, c, v1.RunRunning)
		turnLog.Info("run started", "harness", run.Harness, "model", run.Model, "workdir", prep.Dir)

		w := e.stream(bg, c, a, turn, cancel, native, spec.NativeSessionID != "", turnStarted, lastSeq, wallLeft)
		out := turn.Wait()
		cancel()
		lastSeq = w.lastSeq
		if !w.stopAt.IsZero() {
			w.latency = latency(w.stopAt)
		}
		if out.NativeSessionID != "" && out.NativeSessionID != w.native {
			e.setNative(bg, c, out.NativeSessionID)
			w.native = out.NativeSessionID
		}
		native = w.native
		if ctx.Err() != nil && w.stopped == "" && w.stopAt.IsZero() && out.State == v1.RunCancelled {
			// Killed because the runner is exiting now, not because the run
			// ended: it stays held, and the next start reports it lost
			// (decision 0030).
			turnLog.Warn("run killed with the runner; the next start reports it lost")
			return
		}
		// Folded in before the result is built, so the result is the whole
		// run's rather than this turn's — and so a park that follows carries
		// this turn's cost with it.
		prog.absorb(out, w, turnStarted, time.Now())
		res := e.result(out, w, &prog)
		if res.Error != nil {
			res.Error.Class = hubClass(res.Error.Class, spec.NativeSessionID != "")
		}
		if hasAccount {
			e.recordUsage(bg, acct, out, turnLog)
			e.checkLogin(bg, acct, bin, res, turnLog)
		}
		if res.State == v1.RunCancelled && w.cancelled {
			res.Error = runnerStopped(a.stoppedByRunner())
		}

		// A limit only moves the run when the limit is why the turn ended. A
		// cancel, an interrupt or a watchdog has already decided the run, and
		// a harness that reported a limit on its way down is reporting the
		// account's state, not this run's.
		if hasAccount && out.Limit != nil && !w.cancelled && !w.interrupted && w.stopped == "" {
			// recordUsage has just marked the account limited, so the pick
			// below cannot choose it again: the store is the one place the
			// exclusion lives, rather than a list of tried accounts here that
			// would have to be kept in step with it.
			tried[acct.Label] = true
			next, ok, perr := e.pickAccount(bg, run.Harness)
			switch {
			case ok && tried[next.Label]:
				turnLog.Error("the account at a usage limit was offered to this run again; it is not moved a second time",
					"next_account", next.Label)
			case ok:
				// The move itself — the count, the event and the log line —
				// happens at the top of the next turn, where a move across a
				// park is counted by the same code.
				turnLog.Info("the account is at a usage limit; the run moves to another account",
					"next_account", next.Label)
				acct, lastLimit = next, out.Limit
				continue
			case perr != nil && e.park(bg, c, &prog, &lastSeq, perr.Error()):
				return
			}
			// No account free and no reset to wait for: the limit is this
			// run's answer after all, and the result already says so.
		}
		e.finish(bg, c, res)
		return
	}
}

// runnerStopped is the error a result carries when the runner, not the hub,
// cancelled the run; nil when it was the hub.
func runnerStopped(reason string) *v1.RunError {
	if reason == "" {
		return nil
	}
	return &v1.RunError{Class: ClassRunnerStopping, Message: "the runner cancelled the run on its way down: " + reason}
}

// latency is the time from a control's arrival to now, for cancel_latency_ms.
func latency(since time.Time) *int64 {
	ms := time.Since(since).Milliseconds()
	return &ms
}

// watch is what streaming a turn observed.
type watch struct {
	// stopped is the watchdog class that stopped the turn, or "".
	stopped    string
	stoppedMsg string
	// cancelled: the hub cancelled the run; interrupted: it interrupted the
	// turn. stopAt is when the first of either arrived, and latency how long
	// the turn took to end after it.
	cancelled    bool
	interrupted  bool
	stopAt       time.Time
	latency      *int64
	stalls       int
	firstEventMS int64
	toolCalls    int
	lastSeq      int64
	native       string
}

// stream spools the turn's events until it closes them, under the two
// watchdogs and the hub's controls. wallLeft is what remains of the run's
// wall-clock cap, not the cap itself: the cap belongs to the run, so a run
// that took three turns across two accounts gets one budget between them
// rather than a fresh one each time (DOMAIN.md, "Watchdog"). A cancel or a watchdog climbs the cancel
// ladder: interrupt, which keeps the session resumable; SIGTERM to the
// process group Grace later; SIGKILL TermGrace after that. Events keep being
// spooled all the way down, so what the harness says as it stops is kept.
func (e *Exec) stream(ctx context.Context, c Claim, a *activeRun, turn adapter.Turn, kill context.CancelFunc, native string, resumed bool, started time.Time, lastSeq int64, wallLeft time.Duration) watch {
	w := watch{firstEventMS: -1, native: native, lastSeq: lastSeq}
	log := e.Log.With("connection", c.Connection, "run", c.Run.RunID)
	idleFor := e.inactivity(c.Run)
	idle := time.NewTimer(idleFor)
	defer idle.Stop()
	var wall <-chan time.Time
	if wallLeft > 0 {
		t := time.NewTimer(wallLeft)
		defer t.Stop()
		wall = t.C
	}
	cancelled := a.cancelled
	var term, sigkill <-chan time.Time
	climbing := false
	climb := func() {
		if climbing {
			return
		}
		climbing = true
		// A failed interrupt only means SIGTERM comes sooner in effect; the
		// timers below do not depend on the harness cooperating.
		_ = turn.Interrupt()
		term = time.After(e.Grace)
	}
	stop := func(class, msg string) {
		if w.stopped != "" || w.cancelled {
			return
		}
		w.stopped, w.stoppedMsg = class, msg
		log.Warn("watchdog stopping run", "class", class)
		climb()
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
			if ev.Error != nil {
				// The event says what the result will: one class per cause.
				er := *ev.Error
				er.Class = hubClass(er.Class, resumed)
				ev.Error = &er
			}
			if e.spool(ctx, c, &ev, w.lastSeq+1) {
				w.lastSeq = ev.Seq
				if unreported++; unreported >= eventBatch {
					e.report(c.Connection)
					unreported = 0
				}
			}
			// For an adapter that learns its id mid-turn (Codex's thread id);
			// Claude's was pinned at spawn and never differs here. Stored the
			// moment it appears, so a crash does not lose the resume pointer.
			if id := turn.NativeSessionID(); id != "" && id != w.native {
				w.native = id
				e.setNative(ctx, c, id)
			}
		case <-cancelled:
			cancelled = nil
			at, _ := a.cancelledAt()
			if w.stopped == "" {
				w.cancelled = true
			}
			if w.stopAt.IsZero() {
				w.stopAt = at
			}
			log.Info("cancelling run")
			e.note(ctx, c, &w, v1.Event{Kind: v1.EventStatus, Status: "cancelling"})
			climb()
		case ctl := <-a.controls:
			e.control(ctx, c, &w, turn, ctl)
		case <-idle.C:
			w.stalls++
			stop(ClassInactivity, fmt.Sprintf("the harness produced no event for %s and was stopped", idleFor))
		case <-wall:
			stop(ClassWallClock, fmt.Sprintf("the run reached its wall-clock cap of %s and was stopped", time.Duration(c.Run.WallClockMS)*time.Millisecond))

		case <-term:
			term = nil
			log.Warn("the harness did not stop when interrupted; sending SIGTERM to its process group")
			_ = turn.Terminate()
			sigkill = time.After(e.TermGrace)
		case <-sigkill:
			sigkill = nil
			log.Warn("the harness did not stop on SIGTERM; killing its process group")
			kill()
		}
	}
}

// control acts on an interrupt or a steer for the running turn. What the hub
// asked for, and a steer the harness would not take, go into the run's own
// event stream, where whoever sent it is watching.
func (e *Exec) control(ctx context.Context, c Claim, w *watch, turn adapter.Turn, ctl control) {
	log := e.Log.With("connection", c.Connection, "run", c.Run.RunID)
	switch ctl.Kind {
	case v1.ControlInterrupt:
		// Repeated on every sync until the run ends; the first is the one.
		if w.interrupted {
			return
		}
		// Marked only once it reached the harness: one that did not is
		// tried again when the hub repeats it, and a run that later dies on
		// its own is not reported as cancelled by it.
		if err := turn.Interrupt(); err != nil {
			log.Warn("interrupt not delivered", "err", err)
			e.note(ctx, c, w, v1.Event{Kind: v1.EventError, Error: &v1.RunError{Class: ClassInterrupt, Message: "the interrupt did not reach the harness: " + err.Error() + " — the hub repeats it at the next sync; cancel the run to stop it for certain"}})
			return
		}
		w.interrupted = true
		if w.stopAt.IsZero() {
			w.stopAt = ctl.at
		}
		log.Info("interrupting run")
		e.note(ctx, c, w, v1.Event{Kind: v1.EventStatus, Status: "interrupting"})
	case v1.ControlSteer:
		if err := turn.Steer(ctl.Text); err != nil {
			log.Warn("steer not delivered", "err", err)
			e.note(ctx, c, w, v1.Event{Kind: v1.EventError, Error: &v1.RunError{Class: ClassSteer, Message: "the steer was not delivered: " + err.Error()}})
			return
		}
		e.note(ctx, c, w, v1.Event{Kind: v1.EventStatus, Status: "steered"})
	}
}

// note spools an event of the runner's own into the run's stream.
func (e *Exec) note(ctx context.Context, c Claim, w *watch, ev v1.Event) {
	if e.spool(ctx, c, &ev, w.lastSeq+1) {
		w.lastSeq = ev.Seq
		e.report(c.Connection)
	}
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
	ev.Status, _ = capBytes(ev.Status, maxStatusBytes)
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

// result turns the adapter's outcome into the protocol's terminal report,
// with the metrics and usage of the whole run rather than of its last turn. A
// watchdog's verdict wins over whatever the stopped harness said last. A
// cancel's does not: the harness's own answer, when it had one before the
// cancel reached it, stands (decision 0025); only a turn that ended without
// one is cancelled.
func (e *Exec) result(out adapter.Outcome, w watch, prog *progress) v1.Result {
	// From prog, not from this turn: it has already absorbed this turn, and
	// it is the only thing that knows about the ones before it — on another
	// account, or in another process.
	metrics := prog.metrics(time.Now())
	metrics.CancelLatencyMS = w.latency
	res := v1.Result{
		State: out.State, FinalText: out.FinalText, Error: out.Error, LastSeq: w.lastSeq,
		Metrics: metrics,
	}
	// Before any return below, not after them. What the run spent is true
	// whatever the run then became, and the tokens of a turn that already
	// finished on another account are not undone by a cancel arriving during
	// the next one. Assigned after the early return, a run cancelled a
	// moment after its turn started reported no usage while one cancelled a
	// moment before — through stoppedEarly — reported all of it.
	if len(prog.spent.Usage) > 0 {
		res.Usage.ByModel = prog.spent.Usage
	}
	// A watchdog that stopped the turn has the last word even when the hub
	// asked for an interrupt too: the case below says timed_out.
	stoppedByHub := (w.cancelled || w.interrupted) && w.stopped == ""
	if stoppedByHub && (!out.State.IsTerminal() || out.State == v1.RunLost ||
		out.State == v1.RunFailed && out.Error != nil && out.Error.Class == adapter.ClassHarnessExited) {
		// A harness killed on the way down exits without a result of its
		// own; that is the cancel, not a crash.
		res.State, res.Error, res.FinalText = v1.RunCancelled, nil, ""
		return res
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
	_ = e.finishWith(ctx, c, res, nil)
}

// finishWith is finish with one more statement inside the same transaction,
// for a caller whose bookkeeping must land exactly when the result does. A
// parked run's is the case: ending its wait writes away the resume time that
// keeps it from being started again, so a wait ended beside a result that
// was not recorded leaves a run a later sync would start.
//
// It runs *first*, before the result's own writes, so that a caller can use
// it to refuse the whole transaction on what it finds — reading the run's
// state after SetRunState has already changed it would only ever see what
// this function just wrote.
func (e *Exec) finishWith(ctx context.Context, c Claim, res v1.Result, also func(*db.Queries) error) error {
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
		return err
	}
	var reason sql.NullString
	if res.Error != nil {
		reason = sql.NullString{String: res.Error.Message, Valid: true}
	}
	now := time.Now().UnixMilli()
	err = e.Store.Tx(ctx, func(q *db.Queries) error {
		if also != nil {
			if err := also(q); err != nil {
				return err
			}
		}
		if err := q.SetRunState(ctx, db.SetRunStateParams{
			State: string(res.State), Reason: reason, UpdatedAt: now, Connection: c.Connection, ID: c.Run.RunID,
		}); err != nil {
			return err
		}
		if err := q.TouchSession(ctx, db.TouchSessionParams{LastUsedAt: now, Connection: c.Connection, ID: c.Run.Session.ID}); err != nil {
			return err
		}
		return q.PutOutbox(ctx, db.PutOutboxParams{Connection: c.Connection, RunID: c.Run.RunID, Body: string(body), NextAttemptAt: now})
	})
	switch {
	case errors.Is(err, errNoLongerWaiting):
		// Not a failure and not a retry: the extra statement refused the
		// transaction because something else has already settled this run.
		// Logged as an error it reads as a storage fault on a run that is
		// in fact running perfectly well, which on an unattended runner is
		// what an operator is woken by.
		return err
	case err != nil:
		log.Error("result not recorded; the run stays held", "err", err)
		return err
	}
	log.Info("run finished", "state", res.State, "last_seq", res.LastSeq)
	e.report(c.Connection)
	if e.Ended != nil {
		e.Ended()
	}
	return nil
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

// prepare builds the run's workdir from its sources (internal/workdir), with
// what it does spooled as the run's first events. A cancel from the hub, or
// the runner stopping, ends it early: a clone or a setup hook can take
// minutes.
func (e *Exec) prepare(ctx context.Context, c Claim, a *activeRun, dir string, lastSeq *int64) (*workdir.Prepared, error) {
	bg := context.WithoutCancel(ctx)
	sources, record, err := e.sessionSources(bg, c)
	if err != nil {
		return nil, err
	}
	// A session is bound to its first run's sources — none included — before
	// anything is made on disk: a runner that dies mid-checkout or mid-hook
	// must come back to a session that still knows what its workdir holds, so
	// the next run repairs the worktree and runs the hook again. Sources that
	// break the owner's rules bind nothing; the run is refused as it is.
	if record {
		if err := e.Workdirs.Check(sources, c.Run.Session.ID); err != nil {
			return nil, err
		}
		if sources == nil {
			sources = []v1.Source{}
		}
		body, _ := json.Marshal(sources)
		if err := e.Store.SetSessionSources(bg, db.SetSessionSourcesParams{
			Sources: sql.NullString{String: string(body), Valid: true}, Connection: c.Connection, ID: c.Run.Session.ID,
		}); err != nil {
			return nil, fmt.Errorf("the session's sources could not be recorded: %w", err)
		}
	}
	pctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		select {
		case <-a.cancelled:
			stop()
		case <-pctx.Done():
		}
	}()
	return e.Workdirs.Prepare(pctx, workdir.Request{
		Dir: dir, Connection: c.Connection, Session: c.Run.Session.ID, Sources: sources,
		Emit: func(ev v1.Event) {
			if e.spool(bg, c, &ev, *lastSeq+1) {
				*lastSeq = ev.Seq
				e.report(c.Connection)
			}
		},
	})
}

// sessionSources is what the run's workdir is built from. A session keeps
// the sources its first run named, none included (decision 0033): a
// continuing run that names none is prepared from them — a path source
// locked again, the harness started where the session's conversation lives,
// a setup hook that failed run again — and one naming others is refused,
// since the workdir is already theirs. record says the session has none
// recorded yet, so this run's are to be.
func (e *Exec) sessionSources(ctx context.Context, c Claim) (sources []v1.Source, record bool, err error) {
	sess, err := e.Store.GetSession(ctx, db.GetSessionParams{Connection: c.Connection, ID: c.Run.Session.ID})
	if err != nil {
		return nil, false, err
	}
	if !sess.Sources.Valid {
		return c.Run.Sources, true, nil
	}
	if err := json.Unmarshal([]byte(sess.Sources.String), &sources); err != nil {
		return nil, false, fmt.Errorf("the session's recorded sources are unreadable (%v) — start a new session", err)
	}
	if len(c.Run.Sources) == 0 {
		return sources, false, nil
	}
	want, _ := json.Marshal(c.Run.Sources)
	if string(want) != sess.Sources.String {
		return nil, false, &workdir.Error{Class: workdir.ClassSourceRefused,
			Msg: "the run names sources other than the ones its session's workdir was built from (" + sess.Sources.String + ") — send the same sources, or none, to continue it; start a new session for others"}
	}
	return sources, false, nil
}

// workdir returns the session's workdir, creating it on first use, and the
// session's native id. The directory is the session's, not the run's: a
// resumed conversation expects the files its earlier runs left, so a later
// run reuses the recorded path and recreates it if it vanished, and nothing
// here deletes one — reclaiming is the session's close or its idle TTL
// (decision 0011). Sources inside it are internal/workdir's.
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

// safeName is an id kept as it is. Lower case only: macOS and Windows file
// systems fold case, so "S1" and "s1" — two sessions to a hub — would share one
// workdir, and two runs' grant files one directory.
var safeName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// pathName turns a hub-chosen id into one path component. Hub input is data
// (0038): an id like "../../.ssh" must not name a directory, not because the
// hub is an attacker but because a path built from a string nobody checked is
// a bug. A readable id is
// kept; anything else becomes a hash, prefixed with a character readable ids
// cannot start with, so the two can never collide.
func pathName(id string) string {
	if safeName.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "_" + hex.EncodeToString(sum[:12])
}

// grants delivers the run's grants, which v1.Grant.Validate has passed: an env
// grant as NAME=value, a file grant as a 0600 file whose path is NAME. Never
// argv. cleanup deletes the files and is always safe to call.
func (e *Exec) grants(c Claim) (env []string, cleanup func(), err error) {
	cleanup = func() {}
	var dir string
	for _, g := range c.Run.Grants {
		switch g.As {
		case v1.GrantEnv:
			env = append(env, g.Name+"="+g.Value)
		case v1.GrantFile:
			if dir == "" {
				dir = filepath.Join(e.Data, "grants", c.Connection, pathName(c.Run.RunID))
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return nil, cleanup, fmt.Errorf("grant directory: %w", err)
				}
				cleanup = func() { destroyGrants(dir, e.Log) }
			}
			path := filepath.Join(dir, g.Name)
			if err := os.WriteFile(path, []byte(g.Value), 0o600); err != nil {
				return nil, cleanup, fmt.Errorf("grant %s: %w", g.Name, err)
			}
			env = append(env, g.Name+"="+path)
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
