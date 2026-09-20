package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// Timings on the runner's side of ARCHITECTURE.md §2. The interval is the
// hub's to choose; these bound what a hub may ask for, so a hostile or broken
// one can neither spin a runner nor silence it.
const (
	defaultInterval = 15 * time.Second
	minInterval     = 5 * time.Second
	maxInterval     = 60 * time.Second
	jitterFraction  = 0.10
	firstBackoff    = time.Second
	maxBackoff      = 30 * time.Second
	// maxRefusals bounds the refusals kept for re-sending; past it the oldest
	// are dropped, and the hub re-offering them brings them back.
	maxRefusals = 256
	// Refusals go out after the sync, a few at a time and within a deadline,
	// so a hub that stalls on them cannot hold back the next sync — and with
	// it the leases on every run this runner holds.
	refusalsPerSync = 8
	refusalBudget   = 10 * time.Second
	// closedPerSync bounds the closed sessions one sync reports; the rest go
	// in the syncs after it. A sweep after a long absence may expire
	// hundreds at once, and one sync's body should stay small.
	closedPerSync = 64
)

// Executor runs claimed runs; Exec is the real one. The contract with the sync
// loop is all of this:
//
//   - Start is called once per run, after the hub has acknowledged the claim,
//     from the sync loop itself: it must hand the run off and return at once.
//     The run's capacity is the executor's from then; it calls Claim.Release
//     once the run's terminal state is in the store.
//   - The executor reports a held run's state by writing it to the store
//     (store.SetRunState). The loop lists whatever the store holds on every
//     sync; there is no other channel.
//   - Control delivers the hub's instructions for runs the executor has.
//   - Parked, End and Forget are the executor's side of a run waiting on a
//     usage limit. The run itself is in the store and the loop decides what
//     becomes of it; these are the parts only the executor has — the claim it
//     kept in memory, with the grants that never reach disk, and the write of
//     a terminal result that ends the run's wait in the same transaction.
type Executor interface {
	Start(ctx context.Context, c Claim)
	Control(ctx context.Context, connection string, c v1.Control)
	// Parked is a run this process put in waiting, whole: false for one
	// parked by an earlier process, whose grants did not survive.
	Parked(connection, runID string) (v1.Run, bool)
	// End writes a parked run's terminal result and ends its wait together.
	End(ctx context.Context, c Claim, row db.Run, state v1.RunState, rerr *v1.RunError, now time.Time) error
	// Forget drops the claim this process was keeping for a parked run.
	Forget(connection, runID string)
}

// Claim is a run the hub has acknowledged as this runner's. Run carries its
// grants, which exist nowhere but here: the store keeps the run without them.
type Claim struct {
	Connection string
	Run        v1.Run
	Release    func()
}

// Hub is the part of hubclient.Client the loop uses.
type Hub interface {
	Sync(ctx context.Context, runnerID string, req v1.SyncRequest) (v1.SyncResponse, error)
	Result(ctx context.Context, runID string, res v1.Result) error
}

var _ Hub = (*hubclient.Client)(nil)

// Clock is the loop's time source; tests replace it so no test sleeps.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Loop syncs one connection: heartbeat, lease renewal, claim and the ask for
// work, every interval the hub names (decision 0005).
type Loop struct {
	Connection string // the connection's name, which keys its rows in the store
	RunnerID   string
	Hub        Hub
	Store      *store.Store
	Pool       *Pool
	// Capabilities returns the current capability document. It is called on
	// every sync, so it should return a cached document, not probe.
	Capabilities func() v1.Capabilities
	// Executor nil means there is nothing to run claimed runs with: the loop
	// advertises no free capacity and claims nothing, and syncs only to stay
	// known to the hub.
	Executor Executor
	// Drain is the runner's way down, shared by every connection: while it
	// drains, the loop claims nothing and keeps syncing. Nil never drains.
	Drain *Drain
	// ClaimAfter, until closed, keeps the loop from claiming: what a previous
	// process left owed goes out before new work comes in (decision 0030).
	// Syncs go on meanwhile, so leases renew. Nil claims from the first sync.
	ClaimAfter <-chan struct{}
	Clock      Clock
	// Rand returns a number in [0, 1) for jitter; nil is math/rand.
	Rand func() float64
	Log  *slog.Logger
	// Monitor, when set, hears how every sync went.
	Monitor *Monitor
	// Config and Data are what account.Load reads: the owner's account order
	// and where the homes live. Health reports account state through the same
	// call the executor picks an account with, so the two cannot disagree.
	Config config.Config
	Data   string
	// Sessions closes sessions for the hub's close_session control and
	// measures the disk for health. Nil ignores the control, and the hub
	// hears of no close.
	Sessions *Collector

	sentFingerprint string
	wantDocument    bool
	// pending are runs claimed in the store and not yet acknowledged: the hub
	// has offered them and this runner has not yet listed them in a sync that
	// succeeded. They hold capacity and do not start.
	pending map[string]pendingRun
	// refused are runs this runner will not take, with the failed result the
	// hub is owed. Kept in memory: a run that was never started has nothing
	// on disk to recover, and a hub that re-offers it hears the refusal again.
	refused map[string]v1.Result
	// quiesced is closed at the first sync that begins while draining: from
	// then on this loop starts nothing, so once the executor is idle it stays
	// idle.
	quiesced     chan struct{}
	quiescedOnce sync.Once
	// recovered is set once the runs a previous process held are settled.
	recovered bool
	// harnessReady is what this sync's health said about each harness whose
	// accounts are all limited or need login, so the claim that follows the
	// hub's answer acts on the same reading the hub was sent.
	harnessReady map[string]bool
	// accounts are the states this sync read, once, before its request was
	// built. Health, the claim and the resume all read them here rather
	// than asking again, so what the hub is told and what this runner then
	// does cannot disagree. accountsRead tells "none configured" from "the
	// read failed"; the second reports no harness health at all.
	accounts     []account.Account
	accountsRead bool
	// cancelled are parked runs the hub asked to stop whose terminal result
	// could not be written. The intent lives nowhere else: the transaction
	// leaves the row exactly as it was, which is what keeps the run's grants
	// and its wait, and a row left untouched is indistinguishable from one
	// nobody has cancelled.
	cancelled map[string]bool
	// echoes are closes the store will not report again — a session this
	// runner never held, or one reported before — that the hub asked about
	// since: it is told once more, so it stops asking. Kept in memory: the
	// hub repeats close_session until it hears, so a lost one comes back.
	echoes map[string]v1.ClosedSession
}

type pendingRun struct {
	run     v1.Run
	release func()
	// newSession is whether the claim created the session row, and so
	// whether withdrawing the claim removes it again.
	newSession bool
}

func (l *Loop) init() {
	if l.pending == nil {
		l.pending = map[string]pendingRun{}
		l.refused = map[string]v1.Result{}
		l.echoes = map[string]v1.ClosedSession{}
		l.quiesced = make(chan struct{})
		l.cancelled = map[string]bool{}
	}
	if l.Clock == nil {
		l.Clock = realClock{}
	}
	if l.Rand == nil {
		l.Rand = rand.Float64
	}
	if l.Log == nil {
		l.Log = slog.New(slog.DiscardHandler)
	}
}

// Run syncs until ctx ends. It returns nil on cancellation and an error only
// when syncing cannot succeed without the owner: the credential was refused,
// or the hub requires a newer yad.
func (l *Loop) Run(ctx context.Context) error {
	l.init()
	if err := l.Recover(ctx); err != nil {
		return err
	}
	var drain <-chan struct{}
	if l.Drain != nil {
		drain = l.Drain.Draining()
	}
	// A sync that went out before the replay finished claimed nothing; the
	// next goes the moment it has, not an interval later.
	replayed := l.ClaimAfter
	failures := 0
	for {
		if l.mayClaim() {
			replayed = nil
		}
		res, err := l.SyncOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			l.Monitor.synced(l.Connection, l.Clock.Now())
		}
		var wait time.Duration
		switch {
		case err != nil && fatal(err):
			return err
		case err != nil:
			failures++
			wait = backoff(failures)
			l.Monitor.failed(l.Connection, err, l.Clock.Now(), ConnRetrying)
			l.Log.Warn("sync failed", "connection", l.Connection, "err", err, "retry_in", wait)
		case len(l.pending) > 0:
			// Offers arrived: list them back at once, so a run starts in one
			// round trip rather than one interval.
			failures, wait = 0, 0
		default:
			failures = 0
			wait = interval(res.NextSyncMS)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-drain:
			// The hub hears at once that this runner is draining, and
			// stops offering it work.
			drain = nil
		case <-replayed:
			replayed = nil
		case <-l.Clock.After(l.jitter(wait)):
		}
	}
}

// Quiesced is closed once the loop will start no more runs: it has begun a
// sync while draining.
func (l *Loop) Quiesced() <-chan struct{} {
	l.init()
	return l.quiesced
}

// SyncOnce is one sync: take the free capacity, tell the hub what this runner
// holds, act on the answer, give back what was not used.
func (l *Loop) SyncOnce(ctx context.Context) (v1.SyncResponse, error) {
	l.init()
	draining := l.Drain.IsDraining()
	if draining {
		// Claimed and never listed: the hub still has them as offered, and
		// gives them to another runner once this sync leaves them out.
		for id := range l.pending {
			l.withdraw(ctx, id)
		}
		defer l.quiescedOnce.Do(func() { close(l.quiesced) })
	}
	held, err := l.Store.ListHeldRuns(ctx, l.Connection)
	if err != nil {
		return v1.SyncResponse{}, err
	}
	reporting, err := l.Store.ListReportingRuns(ctx, l.Connection)
	if err != nil {
		return v1.SyncResponse{}, err
	}
	doc := l.Capabilities()
	fp := capability.Fingerprint(doc)
	res := emptyReservation()
	if l.Executor != nil && !draining && l.mayClaim() {
		res = l.Pool.Reserve()
	}
	defer res.Close()

	// Account states, once per sync: the health this request carries, the
	// parked runs it holds capacity for and the claim after the hub's reply
	// all read the same answer rather than asking again.
	//
	// Dropped again at the end, so the field is never a stale answer to
	// somebody outside a sync — health asked on its own loads afresh.
	l.accounts, l.accountsRead = nil, false
	defer func() { l.accounts, l.accountsRead = nil, false }()
	l.loadAccounts(ctx)
	// Capacity for the parked runs that are due, taken before the hub is
	// asked so it is out of the free capacity the request advertises.
	//
	// Without this a parked run is last in line for ever. A hub fills
	// whatever free capacity the request advertises (internal/hub/sync.go
	// pages offers until it is full), and those offers are claimed before
	// the resume is reached — so on a runner whose hub has a standing
	// queue, each unit that frees goes to a new run at every sync and the
	// run that has already waited hours never moves. With a max_wait it is
	// then timed out for a limit that had in fact lifted.
	//
	// The units are only held here; what becomes of each run is decided
	// after the hub's answer, and a unit held for one that turns out not to
	// run is given back at the end of the same sync.
	reserved := l.holdWaiting(held, res)
	defer func() {
		for _, release := range reserved {
			release()
		}
	}()

	req := v1.SyncRequest{RunnerID: l.RunnerID, Fingerprint: fp, Health: l.health(ctx, res)}
	req.Health.Draining = draining
	// The document goes with the first sync of every process, after any move
	// and whenever the hub asks; otherwise the fingerprint stands for it.
	if l.wantDocument || fp != l.sentFingerprint {
		req.Capabilities = &doc
	}
	closed, err := l.closedSessions(ctx)
	if err != nil {
		return v1.SyncResponse{}, err
	}
	req.ClosedSessions = closed
	listed := map[string]bool{}
	for _, r := range held {
		listed[r.ID] = true
		h := v1.HeldRun{RunID: r.ID, State: v1.RunState(r.State), Reason: r.Reason.String}
		if r.ResumesAt.Valid {
			t := time.UnixMilli(r.ResumesAt.Int64).UTC()
			h.ResumesAt = &t
		}
		req.Runs = append(req.Runs, h)
	}
	// A finished run whose result is still in the outbox stays listed, as
	// running: its lease must outlast a hub outage, or the result that
	// arrives after it would find the run already lost.
	for _, r := range reporting {
		if !listed[r.ID] {
			listed[r.ID] = true
			req.Runs = append(req.Runs, v1.HeldRun{RunID: r.ID, State: v1.RunRunning, Reason: "reporting its result"})
		}
	}

	out, err := l.Hub.Sync(ctx, l.RunnerID, req)
	if err != nil {
		return out, err
	}
	if req.Capabilities != nil {
		l.sentFingerprint, l.wantDocument = fp, false
	}
	l.reported(ctx, closed)

	for _, c := range out.Controls {
		switch {
		case c.Kind == v1.ControlReportCapabilities:
			l.wantDocument = true
		case c.Kind == v1.ControlDrain:
			// Repeated until a sync says draining; only the first moves it.
			if l.Drain == nil {
				l.Log.Warn("the hub asked this runner to drain, and nothing here can", "connection", l.Connection)
			} else if l.Drain.Begin("the hub " + l.Connection + " asked the runner to drain") {
				l.Log.Warn("draining at the hub's request: no new runs; exiting once the runs held have ended", "connection", l.Connection)
			}
		case c.Kind == v1.ControlCancel && l.isPending(c.RunID):
			l.withdraw(ctx, c.RunID)
		case c.Kind == v1.ControlCancel && l.cancelWaiting(ctx, c.RunID):
			// A parked run has no turn to interrupt and no process to
			// signal, so the executor has nothing to cancel: the loop ends
			// it where it stands. A cancel is repeated until the run ends
			// (decision 0025), and the second one finds it already terminal
			// and falls through to the executor, which drops it.
		case c.Kind == v1.ControlCloseSession:
			l.closeSession(ctx, c.SessionID)
		case l.Executor != nil:
			l.Executor.Control(ctx, l.Connection, c)
		}
	}
	// A pending run listed in a sync the hub answered is claimed: it starts.
	for id, p := range l.pending {
		if listed[id] {
			delete(l.pending, id)
			l.Executor.Start(ctx, Claim{Connection: l.Connection, Run: p.run, Release: p.release})
		}
	}
	// A runner draining since the hub's answer takes none of what it
	// offered: left out of the next listing, each goes back in the queue.
	if !l.Drain.IsDraining() {
		for _, run := range out.Runs {
			l.claim(ctx, run, doc, res)
		}
	}
	// What was not claimed goes back before anything else can wait on the hub.
	// Parked runs come last, after the hub's controls have been acted on
	// and after its offers: a run already held is this runner's, but a run
	// the hub is offering now is work it is waiting on an answer about.
	if !l.Drain.IsDraining() {
		l.resumeWaiting(ctx, held, res, reserved)
	}
	res.Close()
	l.sendRefusals(ctx)
	return out, nil
}

// mayClaim is whether the replay that must come first is done.
func (l *Loop) mayClaim() bool {
	if l.ClaimAfter == nil {
		return true
	}
	select {
	case <-l.ClaimAfter:
		return true
	default:
		return false
	}
}

func (l *Loop) isPending(runID string) bool {
	_, ok := l.pending[runID]
	return ok
}

// claim takes one offered run: checks it, takes a unit of the reserved
// capacity, and records it in the store as claimed, so the next sync lists it.
// A run that fails a check is refused; one past the capacity is left out of
// the listing, and the hub offers it again later.
func (l *Loop) claim(ctx context.Context, run v1.Run, doc v1.Capabilities, res *Reservation) {
	if l.isPending(run.RunID) {
		return
	}
	if _, ok := l.refused[run.RunID]; ok {
		return
	}
	if prev, err := l.Store.GetRun(ctx, db.GetRunParams{Connection: l.Connection, ID: run.RunID}); err == nil {
		if !v1.RunState(prev.State).IsTerminal() {
			return // held: it is listed on every sync already
		}
		// It ended here — lost at a restart, say — and the hub offers it
		// again. A run is never run twice; saying so ends the re-offers.
		l.refuse(run.RunID, fmt.Sprintf("this runner already held run %s and it ended as %s; a run is not run twice", run.RunID, prev.State))
		return
	}
	if reason := refusal(run, doc); reason != "" {
		l.refuse(run.RunID, reason)
		return
	}
	if l.notClaimable(run.Harness) {
		l.Log.Warn("every account of this harness is at a usage limit or needs a login; leaving the run for the hub to offer again",
			"connection", l.Connection, "run", run.RunID, "harness", run.Harness)
		return
	}
	release, ok := res.Take(run.Harness)
	if !ok {
		l.Log.Warn("offered past free capacity; leaving it for the hub to offer again", "connection", l.Connection, "run", run.RunID)
		return
	}
	newSession, err := l.record(ctx, run)
	if err != nil {
		res.putBack(run.Harness)
		var r refused
		if errors.As(err, &r) {
			l.refuse(run.RunID, string(r))
			return
		}
		var gone sessionGone
		if errors.As(err, &gone) {
			l.refuseAs(run.RunID, ClassSessionClosed, string(gone))
			return
		}
		l.Log.Error("claim not recorded; leaving it for the hub to offer again", "connection", l.Connection, "run", run.RunID, "err", err)
		return
	}
	l.pending[run.RunID] = pendingRun{run: run, release: release, newSession: newSession}
}

// refused is a reason a run cannot be taken that only the store can see.
type refused string

func (r refused) Error() string { return string(r) }

// sessionGone is a run in a session this runner has closed, or is closing.
type sessionGone string

func (g sessionGone) Error() string { return string(g) }

// record writes the claim: the session when the run opens one, and the run as
// claimed, without its grants — grants never touch this machine's disk. It
// reports whether it created the session.
func (l *Loop) record(ctx context.Context, run v1.Run) (newSession bool, err error) {
	stored := run
	stored.Grants = nil
	spec, err := json.Marshal(stored)
	if err != nil {
		return false, err
	}
	now := l.Clock.Now().UnixMilli()
	// Whether, not how many and never which: the only question anything asks
	// of it is whether a run parked on a usage limit can be rebuilt by a
	// later process, and a grant's name says as much about what a hub sent as
	// its value does.
	var hadGrants int64
	if len(run.Grants) > 0 {
		hadGrants = 1
	}
	err = l.Store.Tx(ctx, func(q *db.Queries) error {
		newSession = false
		sess, err := q.GetSession(ctx, db.GetSessionParams{Connection: l.Connection, ID: run.Session.ID})
		switch {
		case err == nil && run.Session.New:
			return refused(fmt.Sprintf("the hub opened session %s as new, and this runner already has a session by that id", run.Session.ID))
		case err == nil && sess.Harness != run.Harness:
			return refused(fmt.Sprintf("session %s is a %s session, not %s", run.Session.ID, sess.Harness, run.Harness))
		case err == nil && sess.State != "open":
			return sessionGone(fmt.Sprintf("session %s was closed on this runner (%s) and its workdir reclaimed; start a new session", run.Session.ID, sess.CloseReason.String))
		case err == nil && sess.CloseRequestedAt.Valid:
			return sessionGone(fmt.Sprintf("session %s is closing on this runner (%s) once its run ends; start a new session", run.Session.ID, sess.CloseReason.String))
		case errors.Is(err, sql.ErrNoRows) && !run.Session.New:
			return refused(fmt.Sprintf("this runner does not hold session %s — sessions resume only on the runner that has them", run.Session.ID))
		case errors.Is(err, sql.ErrNoRows):
			// The workdir is prepared by the executor, which records where.
			if err := q.CreateSession(ctx, db.CreateSessionParams{
				Connection: l.Connection, ID: run.Session.ID, Harness: run.Harness, CreatedAt: now, LastUsedAt: now,
			}); err != nil {
				return err
			}
			newSession = true
		case err != nil:
			return err
		}
		live, err := q.ListHeldRuns(ctx, l.Connection)
		if err != nil {
			return err
		}
		for _, r := range live {
			if r.SessionID == run.Session.ID {
				return refused(fmt.Sprintf("session %s already has run %s live; a session runs one at a time", run.Session.ID, r.ID))
			}
		}
		return q.CreateRun(ctx, db.CreateRunParams{
			Connection: l.Connection, ID: run.RunID, SessionID: run.Session.ID, Harness: run.Harness,
			Model: run.Model, Spec: string(spec), HadGrants: hadGrants, CreatedAt: now, UpdatedAt: now,
		})
	})
	return newSession, err
}

// refusal is why this runner cannot take a run, judged from the run alone and
// the current document; "" when it can. An offer is checked here whatever the
// hub checked before sending it — not because the hub is an attacker (0038)
// but because this runner is the one that has to run it, and a hub on an older
// protocol may not have checked at all.
func refusal(run v1.Run, doc v1.Capabilities) string {
	if err := run.Validate(); err != nil {
		return "the run is invalid: " + err.Error()
	}
	if run.Session.Mode == v1.SessionLive {
		return "this runner does not support live sessions; offer the run with session.mode per_run"
	}
	if !capability.Drivable(doc, run.Harness) {
		return fmt.Sprintf("harness %q is not one this runner can drive — see its capability document", run.Harness)
	}
	return ""
}

func (l *Loop) refuse(runID, reason string) { l.refuseAs(runID, ClassRefused, reason) }

func (l *Loop) refuseAs(runID, class, reason string) {
	l.Log.Warn("refused a run", "connection", l.Connection, "run", runID, "class", class, "reason", reason)
	if len(l.refused) >= maxRefusals {
		for id := range l.refused {
			delete(l.refused, id)
			break
		}
	}
	l.refused[runID] = v1.Result{State: v1.RunFailed, Error: &v1.RunError{Class: class, Message: reason}}
}

// sendRefusals reports each refused run as failed, so the hub stops offering
// it. A refusal the hub settled — accepted, answered with a terminal state it
// already holds, or refused in the protocol's own terms — is done; anything
// else, a proxy's bare 4xx included, is sent again after the next sync.
func (l *Loop) sendRefusals(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, refusalBudget)
	defer cancel()
	sent := 0
	for id, res := range l.refused {
		if sent == refusalsPerSync || ctx.Err() != nil {
			return
		}
		sent++
		err := l.Hub.Result(ctx, id, res)
		if err == nil || final(err) || hubclient.Code(err) == v1.CodeConflict {
			delete(l.refused, id)
		}
	}
}

// withdraw drops a run the hub cancelled before this runner listed it: it
// never started, so there is nothing to stop and no result is owed — the hub
// already decided its fate. Its rows are removed rather than marked, because
// the hub may offer the same run again (a withdrawn offer goes back in its
// queue), and a leftover row, or a leftover empty session, would make this
// runner refuse the very run it gave up.
func (l *Loop) withdraw(ctx context.Context, runID string) {
	p := l.pending[runID]
	delete(l.pending, runID)
	defer p.release()
	err := l.Store.Tx(ctx, func(q *db.Queries) error {
		if err := q.DeleteUnstartedRun(ctx, db.DeleteUnstartedRunParams{Connection: l.Connection, ID: runID}); err != nil {
			return err
		}
		if !p.newSession {
			return nil
		}
		return q.DeleteEmptySession(ctx, db.DeleteEmptySessionParams{Connection: l.Connection, ID: p.run.Session.ID})
	})
	if err != nil {
		l.Log.Error("withdrawn run not removed", "connection", l.Connection, "run", runID, "err", err)
	}
	// A close may have been waiting on the claim.
	l.Sessions.Wake()
}

// Recover settles runs a previous process held (decision 0030). No
// process of theirs survived it — a harness is its runner's child, in a
// process group the runner killed on the way out — so none can be finished,
// and running one again could do its work twice. Each is reported lost: the
// state and the result go into the store in one transaction, the reporter
// delivers the result once the run's spooled events are in, and the run stays
// listed until then, as any run with a result owed does. A hub that already
// lost it on a lapsed lease answers 409, which agrees. The run's session keeps
// its native id and its workdir, so the hub can resume it with a new run.
//
// Run recovers first; Serve recovers every connection before any reporter
// starts, so the lost results are in the outbox for the first flush — the
// replay that comes before any claim. Recover does its work once.
func (l *Loop) Recover(ctx context.Context) error {
	l.init()
	if l.recovered {
		return nil
	}
	held, err := l.Store.ListHeldRuns(ctx, l.Connection)
	if err != nil {
		return err
	}
	now := l.Clock.Now().UnixMilli()
	for _, r := range held {
		if l.isPending(r.ID) {
			continue
		}
		if r.State == string(v1.RunWaiting) {
			// A waiting run held no process to lose (decision 0013). It is
			// the one run a restart does not end: everything it was doing is
			// in its row, and this connection's own sync picks it up from
			// there when its reset passes, and reporting it lost here would throw away the wait
			// the persistence exists for. It stays listed in every sync
			// meanwhile, as it was before the restart.
			l.Log.Info("a previous process parked this run on a usage limit; it keeps waiting", "connection", l.Connection, "run", r.ID)
			continue
		}
		if r.State == string(v1.RunClaimed) {
			withdrawn, err := l.withdrawOrphan(ctx, r)
			if err != nil {
				return err
			}
			if withdrawn {
				continue
			}
		}
		l.Log.Warn("a previous process held this run and no process of it is left; reporting it lost", "connection", l.Connection, "run", r.ID, "state", r.State)
		msg := fmt.Sprintf("the runner stopped while the run was %s, and a run is never run twice; resume its session with a new run", r.State)
		err := l.Store.Tx(ctx, func(q *db.Queries) error {
			last, err := q.LastEventSeq(ctx, db.LastEventSeqParams{Connection: l.Connection, RunID: r.ID})
			if err != nil {
				return err
			}
			body, err := json.Marshal(v1.Result{
				State: v1.RunLost, LastSeq: last,
				Error: &v1.RunError{Class: ClassRunnerRestarted, Message: msg},
			})
			if err != nil {
				return err
			}
			if err := q.SetRunState(ctx, db.SetRunStateParams{
				State: string(v1.RunLost), Reason: sql.NullString{String: msg, Valid: true},
				UpdatedAt: now, Connection: l.Connection, ID: r.ID,
			}); err != nil {
				return err
			}
			// When the run really ended is unknown — some time before this
			// start. Now is the late bound, and late is the safe side: an idle
			// TTL counted from too early reclaims a workdir a hub still wants
			// (decision 0032).
			if err := q.TouchSession(ctx, db.TouchSessionParams{LastUsedAt: now, Connection: l.Connection, ID: r.SessionID}); err != nil {
				return err
			}
			// Due from the start of time: the first flush sends it, whatever
			// the reporter's clock says.
			return q.PutOutbox(ctx, db.PutOutboxParams{Connection: l.Connection, RunID: r.ID, Body: string(body), NextAttemptAt: 0})
		})
		if err != nil {
			return err
		}
	}
	l.recovered = true
	return nil
}

// withdrawOrphan drops a run a previous process claimed and never began to
// prepare: nothing ran, and the claim may never have been listed — the hub
// may still have it as offered. Left out of the listing, an offer goes back
// in the hub's queue; a claim the hub had acknowledged lapses into lost on
// its side, which is the truth of it. Reporting it lost here would end, for
// good, a run that may only ever have been offered. It reports whether the
// run is gone; one with events or a result owed is not an unstarted claim.
func (l *Loop) withdrawOrphan(ctx context.Context, r db.Run) (bool, error) {
	gone := false
	err := l.Store.Tx(ctx, func(q *db.Queries) error {
		if err := q.DeleteUnstartedRun(ctx, db.DeleteUnstartedRunParams{Connection: l.Connection, ID: r.ID}); err != nil {
			return err
		}
		if _, err := q.GetRun(ctx, db.GetRunParams{Connection: l.Connection, ID: r.ID}); !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		gone = true
		return q.DeleteEmptySession(ctx, db.DeleteEmptySessionParams{Connection: l.Connection, ID: r.SessionID})
	})
	if gone && err == nil {
		l.Log.Warn("a previous process claimed this run and never started it; withdrawn, for the hub to offer again or lose", "connection", l.Connection, "run", r.ID)
	}
	return gone && err == nil, err
}

func (l *Loop) health(ctx context.Context, res *Reservation) v1.Health {
	h := v1.Health{FreeCapacity: res.Free(), DiskFreeBytes: l.Sessions.FreeBytes()}
	// Depths are best effort: a health report is not worth failing a sync.
	if n, err := l.Store.SpoolDepth(ctx); err == nil {
		h.SpoolDepth = int(n)
	}
	if n, err := l.Store.OutboxDepth(ctx); err == nil {
		h.OutboxDepth = int(n)
	}
	h.Harnesses = l.harnessHealth(ctx)
	return h
}

// harnessHealth is every harness this runner can drive, with each of its
// accounts' state, so a hub can see why a runner is not claiming: an account
// limited until a reset, or one whose login the owner has to finish, is a
// reason, and having no accounts at all is not.
//
// The states come from account.Load — the same call pickAccount makes, so what
// health reports and what a claim would actually do are read from one answer
// rather than derived twice. Building them here from store rows instead was
// how an account whose home had been deleted came out free in health while the
// capability document, built from Load in the same sync, called it
// needs_login.
//
// One seam remains and is deliberate: Report re-derives a limit's expiry
// against the clock at the moment it is called, so a limit that ends between
// this function's Load and its Reports leaves an account reported free beside
// a harness whose readiness was computed from the same Load and still says
// not ready. It lasts until the next sync and errs towards a hub being told
// the runner can do less than it can, which is the harmless direction. The
// re-derivation is worth that: it is what stops an Account built anywhere
// else reporting a state Load would not.
func (l *Loop) harnessHealth(ctx context.Context) []v1.HarnessHealth {
	doc := l.Capabilities()
	// Rebuilt every sync and read by the claim below, so what the hub was
	// told and what this runner then does come from one answer.
	ready := map[string]bool{}
	defer func() { l.harnessReady = ready }()
	l.loadAccounts(ctx)
	accounts, ok := l.accounts, l.accountsRead
	if !ok {
		// Reporting every account free because the read failed would be the
		// one direction that costs something: the hub keeps offering runs for
		// a harness whose accounts cannot take them, and each is refused after
		// being claimed. Saying nothing is the honest answer to a question
		// that did not get one, and health carries no harnesses when empty.
		return nil
	}
	var out []v1.HarnessHealth
	for _, hr := range doc.Harnesses {
		// The predicate a claim uses, not a copy of part of it: a harness
		// present but unusable — a --version that would not parse — must not
		// be reported ready to a hub that routes on health.
		if !capability.Drivable(doc, hr.ID) {
			continue
		}
		hh := v1.HarnessHealth{ID: hr.ID, Accounts: account.Reports(accounts, hr.ID)}
		// No accounts means the harness runs on its own default home, which
		// is ready; accounts that all need login or are limited mean it is
		// not, and the claim below leaves such a harness's offers alone.
		_, usable := account.Soonest(accounts, hr.ID, l.Clock.Now())
		hh.Ready = len(hh.Accounts) == 0 || usable
		if !hh.Ready {
			ready[hr.ID] = false
		}
		out = append(out, hh)
	}
	return out
}

// notClaimable says whether every account of this harness is limited or needs
// a login, as the health this sync sent said. Reading the answer the hub was
// given, rather than asking again, is what keeps the two from disagreeing:
// a runner that reports a harness not ready and then claims for it anyway is
// telling the hub one thing and doing another.
//
// Not claiming rather than refusing: a refusal is a terminal result and ends
// the run for every runner, and there is nothing wrong with this run — only
// with this machine, for as long as the reset says. Left out of the listing,
// the offer goes back in the hub's queue.
func (l *Loop) notClaimable(harness string) bool {
	ready, ok := l.harnessReady[harness]
	return ok && !ready
}

// fatal is an answer no retry can change: the owner has to act.
func fatal(err error) bool {
	switch hubclient.Code(err) {
	case v1.CodeUnauthorized, v1.CodeRunnerRevoked, v1.CodeVersionTooOld, v1.CodeUnsupportedProtocol:
		return true
	}
	return false
}

// SyncFloorForTests replaces minInterval while it is positive, so an
// end-to-end test that is also the hub can drive the protocol in milliseconds;
// at the shipped floor those tests spend their time asleep (DEV-63). The floor
// itself stays: it is what keeps a hostile or broken hub from spinning this
// machine, so it is reachable from no configuration file, flag or protocol
// field — only from a test in this module. TestOnlyTestsReachTheSyncFloor fails
// on any shipped file that assigns it, this one included; a backstop against the
// assignment someone would actually write, not a proof.
var SyncFloorForTests time.Duration

// interval is the hub's chosen interval, held within bounds.
func interval(ms int) time.Duration {
	if ms <= 0 {
		return defaultInterval
	}
	floor := minInterval
	if SyncFloorForTests > 0 {
		floor = SyncFloorForTests
	}
	return min(max(time.Duration(ms)*time.Millisecond, floor), maxInterval)
}

// backoff doubles from firstBackoff to maxBackoff over consecutive failures.
func backoff(failures int) time.Duration {
	d := firstBackoff
	for i := 1; i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

// jitter spreads d by ±10 %, so runners restarted together do not sync in
// lockstep for the rest of the day.
func (l *Loop) jitter(d time.Duration) time.Duration {
	return time.Duration(float64(d) * (1 - jitterFraction + 2*jitterFraction*l.Rand()))
}

// closeSession acts on the hub's close_session. A close that waits on a run
// is reported once it happens; one the store will not report again is echoed.
func (l *Loop) closeSession(ctx context.Context, id string) {
	if id == "" {
		l.Log.Warn("the hub sent close_session without a session id; ignored", "connection", l.Connection)
		return
	}
	if l.Sessions == nil {
		l.Log.Warn("the hub asked to close a session, and nothing here can", "connection", l.Connection, "session", id)
		return
	}
	res, err := l.Sessions.Close(ctx, l.Connection, id, v1.SessionClosed)
	if err != nil {
		l.Log.Error("session not closed; the hub asks again at the next sync", "connection", l.Connection, "session", id, "err", err)
		return
	}
	switch res.Outcome {
	case CloseUnknown:
		// Nothing to reclaim, and the hub can stop asking: a session this
		// runner does not hold is as closed as one it reclaimed.
		l.echoes[id] = v1.ClosedSession{SessionID: id, Reason: v1.SessionClosed, ClosedAt: l.Clock.Now().UTC()}
	case CloseAlready:
		l.echoes[id] = v1.ClosedSession{SessionID: id, Reason: res.Reason, ClosedAt: res.ClosedAt}
	}
}

// closedSessions is what this sync reports closed: what the store has not
// yet had acknowledged, then the echoes, up to closedPerSync.
func (l *Loop) closedSessions(ctx context.Context) ([]v1.ClosedSession, error) {
	rows, err := l.Store.UnreportedClosedSessions(ctx, db.UnreportedClosedSessionsParams{Connection: l.Connection, Max: closedPerSync})
	if err != nil {
		return nil, err
	}
	var out []v1.ClosedSession
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.ID] = true
		out = append(out, v1.ClosedSession{SessionID: r.ID, Reason: v1.SessionCloseReason(r.CloseReason.String), ClosedAt: msTime(r.ClosedAt.Int64)})
	}
	for id, e := range l.echoes {
		if len(out) >= closedPerSync {
			break
		}
		if !seen[id] {
			out = append(out, e)
		}
	}
	return out, nil
}

// reported records that the hub answered a sync carrying these closes.
func (l *Loop) reported(ctx context.Context, closed []v1.ClosedSession) {
	now := sql.NullInt64{Int64: l.Clock.Now().UnixMilli(), Valid: true}
	for _, c := range closed {
		delete(l.echoes, c.SessionID)
		if err := l.Store.SetSessionReported(ctx, db.SetSessionReportedParams{ReportedAt: now, Connection: l.Connection, ID: c.SessionID}); err != nil {
			// Reported again at the next sync, which a hub takes as the same news.
			l.Log.Error("a close the hub heard is not recorded as heard", "connection", l.Connection, "session", c.SessionID, "err", err)
		}
	}
}
