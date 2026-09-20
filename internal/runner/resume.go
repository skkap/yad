package runner

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
	v1 "github.com/skkap/yad/protocol/v1"
)

// Resumer brings parked runs back (decision 0013). A run that hit a usage
// limit with no free account to move to is `waiting` in the store, with the
// moment it comes back, the wait it has already served and the accounts it
// has been through; it holds no process and no goroutine. This is what picks
// it up — at the next sweep on this runner, or at the first sweep of a later
// process, which is the same code either way.
//
// One per process rather than one per connection: capacity is one pool and a
// parked run of one hub must not be resumed twice, nor jump the queue of
// another hub's.
type Resumer struct {
	Store *store.Store
	Pool  *Pool
	// Exec is what a resumed run is handed to, exactly as a fresh claim is.
	Exec *Exec
	// Drain, while it is draining, stops resumes: a runner on its way down
	// takes on nothing new, and a parked run is better left parked for the
	// next process than started ten seconds before the last one exits.
	Drain *Drain
	// Runs is what a resumed run's own context is, as runsOn is for a
	// claimed one: a connection stopping, or a sweep ending, must not kill a
	// run it has just handed over. Nil starts runs on the sweep's context.
	Runs  context.Context
	Clock Clock
	Log   *slog.Logger

	once sync.Once
	wake chan struct{}
	// mu makes sweeps one at a time, so two of them cannot start one run.
	mu sync.Mutex
}

// resumeEvery is the longest a sweep waits when nothing is due. Each sweep
// asks for a moment sooner if a run is parked for one, so this is only the
// floor under "notice that capacity freed, or that a run was parked by
// another goroutine while this one slept" — both of which also wake it.
const resumeEvery = time.Minute

// resumeSkew is added to a run's resume time before it is taken as due. The
// reset came from the harness, whose clock is its own; starting a turn a
// moment early gets the same limit back and costs a whole turn, while
// starting a moment late costs a moment.
const resumeSkew = 5 * time.Second

func (r *Resumer) init() {
	r.once.Do(func() {
		r.wake = make(chan struct{}, 1)
		if r.Clock == nil {
			r.Clock = realClock{}
		}
		if r.Log == nil {
			r.Log = slog.New(slog.DiscardHandler)
		}
	})
}

// Wake asks for a sweep now: a run ended and its capacity is free, or one has
// just been parked. Safe on a nil Resumer.
func (r *Resumer) Wake() {
	if r == nil {
		return
	}
	r.init()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run sweeps until ctx ends: at once, whenever woken, and when the soonest
// parked run is due. A nil Resumer returns at once.
func (r *Resumer) Run(ctx context.Context) {
	if r == nil {
		return
	}
	r.init()
	for {
		next := r.Sweep(ctx)
		if next <= 0 || next > resumeEvery {
			next = resumeEvery
		}
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-r.Clock.After(next):
		}
	}
}

// Sweep takes every parked run that is due, and ends every one whose hub's
// max_wait has run out. It returns how long until the soonest thing it left
// behind needs looking at again, or zero when nothing is parked.
func (r *Resumer) Sweep(ctx context.Context) time.Duration {
	r.init()
	r.mu.Lock()
	defer r.mu.Unlock()
	rows, err := r.Store.ListWaitingRuns(ctx)
	if err != nil {
		r.Log.Error("could not read the parked runs; they are tried again at the next sweep", "err", err)
		return 0
	}
	var soonest time.Duration
	for _, row := range rows {
		if ctx.Err() != nil {
			return 0
		}
		next := r.consider(ctx, row)
		if next > 0 && (soonest == 0 || next < soonest) {
			soonest = next
		}
	}
	return soonest
}

// consider decides what becomes of one parked run, and returns how long until
// it is worth looking at again — zero when it has been dealt with.
func (r *Resumer) consider(ctx context.Context, row db.Run) time.Duration {
	now := r.Clock.Now()
	log := r.Log.With("connection", row.Connection, "run", row.ID)
	var run v1.Run
	if err := json.Unmarshal([]byte(row.Spec), &run); err != nil {
		// Unreadable, so it can never run and never resume: ending it is the
		// only way the hub hears anything at all about it.
		r.end(ctx, row, v1.RunFailed, &v1.RunError{Class: ClassAdapter,
			Message: "the parked run could not be read back from this runner's store (" + err.Error() + ") — offer it again"}, now)
		return 0
	}
	waited := row.WaitedMs + waitingFor(row, now)
	if run.MaxWaitMS > 0 && waited >= run.MaxWaitMS {
		log.Warn("the run waited longer than its hub allowed; it is timed out", "waited_ms", waited, "max_wait_ms", run.MaxWaitMS)
		r.end(ctx, row, v1.RunTimedOut, &v1.RunError{Class: ClassMaxWait, Message: maxWaitMessage(waited, run.MaxWaitMS)}, now)
		return 0
	}
	due := now
	if row.ResumesAt.Valid {
		due = time.UnixMilli(row.ResumesAt.Int64).Add(resumeSkew)
	}
	if wait := due.Sub(now); wait > 0 {
		return capWait(wait, run, waited)
	}
	// Its grants are the one thing the row cannot hold. A run that had them
	// and is not the one this process parked has lost them for good: it is
	// reported rather than started, so the failure is a sentence the hub can
	// act on instead of a harness failing on a missing credential.
	held, ours := r.Exec.takeParked(row.Connection, row.ID)
	if !ours && row.HadGrants != 0 {
		log.Warn("the run was parked by an earlier process and its grants did not survive; the hub is asked for it again")
		r.end(ctx, row, v1.RunLost, &v1.RunError{Class: ClassGrantsLost,
			Message: "this run was waiting for a free account when the runner stopped, and the grants it was given do not survive a restart — offer it again"}, now)
		return 0
	}
	if r.Drain.IsDraining() {
		// Left exactly as it is: the next process finds it parked and due,
		// and the sweep it does before its first claim picks it up.
		return 0
	}
	release, ok := r.Pool.Take(row.Harness)
	if !ok {
		// Capacity is a shared pool and nothing reserves it for a parked
		// run. Wake is called when a run ends, so this is not a poll.
		if ours {
			r.Exec.hold(Claim{Connection: row.Connection, Run: held})
		}
		return resumeEvery
	}
	if !ours {
		held = run
	}
	if err := r.Store.EndRunWait(ctx, db.EndRunWaitParams{Now: now.UnixMilli(), Connection: row.Connection, ID: row.ID}); err != nil {
		release()
		if ours {
			r.Exec.hold(Claim{Connection: row.Connection, Run: held})
		}
		log.Error("the end of the run's wait could not be recorded; it stays parked", "err", err)
		return resumeEvery
	}
	// Out of waiting before the run's goroutine starts, so a sweep that
	// overlaps this one does not find it parked and start it twice. A write
	// that fails is not worth stopping for: the run is about to set
	// preparing itself, and the sweep is one at a time.
	if err := r.Store.SetRunState(ctx, db.SetRunStateParams{
		State: string(v1.RunClaimed), UpdatedAt: now.UnixMilli(), Connection: row.Connection, ID: row.ID,
	}); err != nil {
		log.Warn("the resumed run's state could not be recorded", "err", err)
	}
	log.Info("the account's limit has reset; the run continues in the same session", "waited_ms", waited)
	runCtx := r.Runs
	if runCtx == nil {
		runCtx = ctx
	}
	r.Exec.Start(runCtx, Claim{Connection: row.Connection, Run: held, Release: release})
	return 0
}

// capWait shortens a wait that would run past the hub's max_wait, so the run
// is timed out when its cap runs out rather than at the next reset after it.
func capWait(wait time.Duration, run v1.Run, waited int64) time.Duration {
	if run.MaxWaitMS <= 0 {
		return wait
	}
	return min(wait, time.Duration(run.MaxWaitMS-waited)*time.Millisecond)
}

// waitingFor is how long the current park has lasted. A row with no start —
// written by a version that did not record one, or by hand — counts as zero
// rather than as a wait since the epoch, which would time every run out at
// once.
func waitingFor(row db.Run, now time.Time) int64 {
	if !row.WaitingSince.Valid {
		return 0
	}
	return max(now.UnixMilli()-row.WaitingSince.Int64, 0)
}

// end finishes a parked run: the wait it served is counted first, so the
// result reports it, and then the executor writes the terminal state and the
// result together as it does for any other run.
func (r *Resumer) end(ctx context.Context, row db.Run, state v1.RunState, rerr *v1.RunError, now time.Time) {
	if err := r.Store.EndRunWait(ctx, db.EndRunWaitParams{Now: now.UnixMilli(), Connection: row.Connection, ID: row.ID}); err != nil {
		r.Log.Error("the end of the run's wait could not be recorded", "connection", row.Connection, "run", row.ID, "err", err)
	}
	r.Exec.forget(row.Connection, row.ID)
	c, err := claimFor(row, nil)
	if err != nil {
		// Only reachable for a spec that would not parse, which is how this
		// was called in the first place; the run still has to end, so it ends
		// with the id the row has and nothing from the spec.
		c = Claim{Connection: row.Connection, Run: v1.Run{RunID: row.ID, Harness: row.Harness}, Release: func() {}}
		c.Run.Session = v1.SessionRef{ID: row.SessionID}
	}
	r.Exec.finishParked(ctx, c, row, state, rerr, now)
}

// finishParked writes the terminal state and result of a run that ended while
// it was parked. It reads the run's cost back from the row rather than from
// memory, because the process that parked it may be gone.
func (e *Exec) finishParked(ctx context.Context, c Claim, row db.Run, state v1.RunState, rerr *v1.RunError, now time.Time) {
	e.init()
	seq := e.lastSeq(ctx, c)
	started := now
	if row.StartedAt.Valid {
		started = time.UnixMilli(row.StartedAt.Int64)
	}
	waited := row.WaitedMs + waitingFor(row, now)
	e.finish(ctx, c, v1.Result{
		State: state, LastSeq: seq, Error: rerr,
		Metrics: v1.Metrics{
			DurationMS: max(now.Sub(started).Milliseconds(), 0),
			WaitedMS:   waited, AccountSwitches: int(row.AccountSwitches),
		},
	})
}

// CancelWaiting ends a parked run because the hub or the owner asked. A
// waiting run has no turn to interrupt and no process to signal, so the
// cancel ladder has nothing to climb: the run ends here, with the wait it
// served reported. It answers whether the run was parked.
func (r *Resumer) CancelWaiting(ctx context.Context, connection, runID string) bool {
	if r == nil {
		return false
	}
	r.init()
	r.mu.Lock()
	defer r.mu.Unlock()
	row, err := r.Store.GetRun(ctx, db.GetRunParams{Connection: connection, ID: runID})
	if err != nil || row.State != string(v1.RunWaiting) {
		return false
	}
	now := r.Clock.Now()
	r.Log.Info("the run was waiting for a free account and is cancelled", "connection", connection, "run", runID)
	if err := r.Store.EndRunWait(ctx, db.EndRunWaitParams{Now: now.UnixMilli(), Connection: connection, ID: runID}); err != nil {
		r.Log.Error("the end of the run's wait could not be recorded", "connection", connection, "run", runID, "err", err)
	}
	r.Exec.forget(connection, runID)
	c, cerr := claimFor(row, nil)
	if cerr != nil {
		c = Claim{Connection: connection, Run: v1.Run{RunID: runID, Harness: row.Harness, Session: v1.SessionRef{ID: row.SessionID}}, Release: func() {}}
	}
	// No error, as for any run the hub cancelled: the hub asked, so it knows
	// why, and a runner that invented a reason here would be the only one of
	// the two making something up.
	r.Exec.finishParked(ctx, c, row, v1.RunCancelled, nil, now)
	return true
}
