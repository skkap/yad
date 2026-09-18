package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
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
)

// Executor runs claimed runs. It is how the next layer (the executor, epic E2)
// plugs into the sync loop, and all of it:
//
//   - Start is called once per run, after the hub has acknowledged the claim,
//     from the sync loop itself: it must hand the run off and return at once.
//     The run's capacity is the executor's from then; it calls Claim.Release
//     once the run's terminal state is in the store.
//   - The executor reports a held run's state by writing it to the store
//     (store.SetRunState). The loop lists whatever the store holds on every
//     sync; there is no other channel.
//   - Control delivers the hub's instructions for runs the executor has.
type Executor interface {
	Start(ctx context.Context, c Claim)
	Control(ctx context.Context, connection string, c v1.Control)
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
	Clock    Clock
	// Rand returns a number in [0, 1) for jitter; nil is math/rand.
	Rand func() float64
	Log  *slog.Logger

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
	if err := l.abandonOrphans(ctx); err != nil {
		return err
	}
	failures := 0
	for {
		res, err := l.SyncOnce(ctx)
		if ctx.Err() != nil {
			return nil
		}
		var wait time.Duration
		switch {
		case err != nil && fatal(err):
			return err
		case err != nil:
			failures++
			wait = backoff(failures)
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
		case <-l.Clock.After(l.jitter(wait)):
		}
	}
}

// SyncOnce is one sync: take the free capacity, tell the hub what this runner
// holds, act on the answer, give back what was not used.
func (l *Loop) SyncOnce(ctx context.Context) (v1.SyncResponse, error) {
	l.init()
	held, err := l.Store.ListHeldRuns(ctx, l.Connection)
	if err != nil {
		return v1.SyncResponse{}, err
	}
	doc := l.Capabilities()
	fp := capability.Fingerprint(doc)
	res := emptyReservation()
	if l.Executor != nil {
		res = l.Pool.Reserve()
	}
	defer res.Close()

	req := v1.SyncRequest{RunnerID: l.RunnerID, Fingerprint: fp, Health: l.health(ctx, res)}
	// The document goes with the first sync of every process, after any move
	// and whenever the hub asks; otherwise the fingerprint stands for it.
	if l.wantDocument || fp != l.sentFingerprint {
		req.Capabilities = &doc
	}
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

	out, err := l.Hub.Sync(ctx, l.RunnerID, req)
	if err != nil {
		return out, err
	}
	if req.Capabilities != nil {
		l.sentFingerprint, l.wantDocument = fp, false
	}

	for _, c := range out.Controls {
		switch {
		case c.Kind == v1.ControlReportCapabilities:
			l.wantDocument = true
		case c.Kind == v1.ControlCancel && l.isPending(c.RunID):
			l.withdraw(ctx, c.RunID)
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
	for _, run := range out.Runs {
		l.claim(ctx, run, doc, res)
	}
	// What was not claimed goes back before anything else can wait on the hub.
	res.Close()
	l.sendRefusals(ctx)
	return out, nil
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
		l.Log.Error("claim not recorded; leaving it for the hub to offer again", "connection", l.Connection, "run", run.RunID, "err", err)
		return
	}
	l.pending[run.RunID] = pendingRun{run: run, release: release, newSession: newSession}
}

// refused is a reason a run cannot be taken that only the store can see.
type refused string

func (r refused) Error() string { return string(r) }

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
	err = l.Store.Tx(ctx, func(q *db.Queries) error {
		newSession = false
		sess, err := q.GetSession(ctx, db.GetSessionParams{Connection: l.Connection, ID: run.Session.ID})
		switch {
		case err == nil && run.Session.New:
			return refused(fmt.Sprintf("the hub opened session %s as new, and this runner already has a session by that id", run.Session.ID))
		case err == nil && sess.Harness != run.Harness:
			return refused(fmt.Sprintf("session %s is a %s session, not %s", run.Session.ID, sess.Harness, run.Harness))
		case err == nil && sess.State != "open":
			return refused(fmt.Sprintf("session %s is %s on this runner and cannot be resumed", run.Session.ID, sess.State))
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
			Model: run.Model, Spec: string(spec), CreatedAt: now, UpdatedAt: now,
		})
	})
	return newSession, err
}

// refusal is why this runner cannot take a run, judged from the run alone and
// the current document; "" when it can. A hub is untrusted input: an offer is
// checked here whatever the hub checked before sending it.
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

func (l *Loop) refuse(runID, reason string) {
	l.Log.Warn("refused a run", "connection", l.Connection, "run", runID, "reason", reason)
	if len(l.refused) >= maxRefusals {
		for id := range l.refused {
			delete(l.refused, id)
			break
		}
	}
	l.refused[runID] = v1.Result{State: v1.RunFailed, Error: &v1.RunError{Class: "refused", Message: reason}}
}

// sendRefusals reports each refused run as failed, so the hub stops offering
// it. A refusal the hub answered — accepted, or refused with a 4xx — is done;
// one lost to the network or a 5xx is sent again after the next sync.
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
		var se *hubclient.StatusError
		if err == nil || errors.As(err, &se) && se.Status < 500 {
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
}

// abandonOrphans settles runs a previous process held. Nothing here can
// resume them yet (restart safety is epic E3), and listing them would renew
// their leases forever for work nobody is doing. They become lost locally and
// drop out of the listing, so the hub marks them lost when their leases lapse
// — reported, never silently retried.
func (l *Loop) abandonOrphans(ctx context.Context) error {
	held, err := l.Store.ListHeldRuns(ctx, l.Connection)
	if err != nil {
		return err
	}
	now := l.Clock.Now().UnixMilli()
	for _, r := range held {
		if l.isPending(r.ID) {
			continue
		}
		l.Log.Warn("a previous process held this run; it is lost", "connection", l.Connection, "run", r.ID, "state", r.State)
		if err := l.Store.SetRunState(ctx, db.SetRunStateParams{
			State: string(v1.RunLost), Reason: sql.NullString{String: "the runner restarted while holding it", Valid: true},
			UpdatedAt: now, Connection: l.Connection, ID: r.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (l *Loop) health(ctx context.Context, res *Reservation) v1.Health {
	h := v1.Health{FreeCapacity: res.Free()}
	// Depths are best effort: a health report is not worth failing a sync.
	if n, err := l.Store.SpoolDepth(ctx); err == nil {
		h.SpoolDepth = int(n)
	}
	if n, err := l.Store.OutboxDepth(ctx); err == nil {
		h.OutboxDepth = int(n)
	}
	return h
}

// fatal is an answer no retry can change: the owner has to act.
func fatal(err error) bool {
	switch hubclient.Code(err) {
	case v1.CodeUnauthorized, v1.CodeRunnerRevoked, v1.CodeVersionTooOld, v1.CodeUnsupportedProtocol:
		return true
	}
	return false
}

// interval is the hub's chosen interval, held within bounds.
func interval(ms int) time.Duration {
	if ms <= 0 {
		return defaultInterval
	}
	return min(max(time.Duration(ms)*time.Millisecond, minInterval), maxInterval)
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
