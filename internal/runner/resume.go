package runner

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
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
	// Config and Data are what account.Load reads, so a sweep can see that
	// an account has become usable before the reset the run was parked on.
	Config config.Config
	Data   string
	// Live is the connections this process actually runs: those with a loop
	// syncing and a reporter delivering. A waiting row of any other
	// connection is left alone — nothing here would renew its lease or
	// deliver its result, so starting it would spend tokens on a run the hub
	// is about to give to somebody else. Nil runs every connection, which is
	// what a test with one wants.
	Live map[string]bool
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
	// Read once for the whole sweep rather than once per run: every parked
	// run asks the same question of the same rows, and a limit that ends
	// between two of them would otherwise make the sweep's answers disagree
	// with each other.
	accounts, err := account.Load(ctx, r.Store.Queries, r.Data, r.Config)
	if err != nil {
		// A sweep that cannot see the accounts falls back to the resume
		// times alone, which is what this did before it could see them:
		// later than it might be, never earlier than it should be.
		r.Log.Warn("could not read account states; this sweep goes by resume times alone", "err", err)
		accounts = nil
	}
	var soonest time.Duration
	for _, row := range rows {
		if ctx.Err() != nil {
			return 0
		}
		next := r.consider(ctx, row, accounts)
		if next > 0 && (soonest == 0 || next < soonest) {
			soonest = next
		}
	}
	return soonest
}

// consider decides what becomes of one parked run, and returns how long until
// it is worth looking at again — zero when it has been dealt with.
func (r *Resumer) consider(ctx context.Context, row db.Run, accounts []account.Account) time.Duration {
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
	// Only past the two ways a parked run ends without running, so a run of
	// a connection this process cannot serve is still bounded by the cap its
	// hub gave it. What is left over — a run of a connection the owner
	// removed from the configuration and that carries no cap — stays
	// waiting, holding its session and workdir out of collection, as every
	// other held run of a removed connection already does; clearing up after
	// one is `yad disconnect` (epic E7, cmd/yad/cmd_later.go).
	if r.Live != nil && !r.Live[row.Connection] {
		// This process has no loop and no reporter for that connection — its
		// credential could not be read, its Recover failed, or the owner
		// took it out of the configuration. Running the turn would spend
		// tokens on a run whose lease nothing here renews and whose result
		// nothing here delivers, so the hub would lose it, give it to
		// another runner, and this machine would run it twice.
		return 0
	}
	due := now
	if row.ResumesAt.Valid {
		due = time.UnixMilli(row.ResumesAt.Int64).Add(resumeSkew)
	}
	// The resume time is the earliest reset known when the run parked, and
	// it is a floor rather than the whole answer: an account can become
	// usable before it. The way that happens is the owner finishing a login
	// in a home, which LoginProbe notices — and only that. A `yad account
	// add` of a *new* label does not reach a running daemon at all: the
	// config is the one it started with, and account.Load enumerates only
	// the labels in it. ARCHITECTURE's run-state
	// diagram says the arrow out of waiting is "limit resets / account
	// frees", and without this only the first half of that is true — a run
	// would sit out the remaining hours beside a working account, and one
	// with a max_wait could time out while that account ran newer work.
	if wait := due.Sub(now); wait > 0 {
		// Inside the skew, nothing is early. resumes_at is the earliest
		// limited_until among the harness's accounts, and account.stateOf
		// reads that account free the instant the moment passes, with no
		// skew of its own — so for resumeSkew after the reset a free account
		// and a not-yet-due run are the *same* reset seen through two
		// clocks. Resuming on that reading is exactly what the skew exists
		// to prevent: the harness's clock is its own, the turn comes back
		// with the same limit, and the run pays a cache-cold turn for it.
		if wait <= resumeSkew {
			return wait
		}
		if _, free := account.Soonest(accounts, row.Harness, now); !free {
			return capWait(wait, run, waited)
		}
		log.Info("an account is free before the reset the run was parked on; it continues now",
			"resumes_at", due.Add(-resumeSkew).UTC())
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
		// and the sweep it does before its first claim picks it up. Which
		// includes the claim taken out of the map above — without putting it
		// back, the next sweep of this same process finds no held claim and
		// reports a run with grants lost, in the middle of a drain the run
		// was supposed to sit out.
		if ours {
			r.Exec.hold(Claim{Connection: row.Connection, Run: held})
		}
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
	// Ending the wait and leaving waiting are one transaction, because
	// EndRunWait clears resumes_at and a row with no resumes_at reads as due
	// right now. Written separately, a failure of the second leaves a
	// waiting row that every later sweep starts again — and the same shape
	// on the ending paths would let a cancelled run be executed. Either both
	// land or the run stays parked exactly as it was.
	err := r.Store.Tx(ctx, func(q *db.Queries) error {
		if err := q.EndRunWait(ctx, db.EndRunWaitParams{Now: now.UnixMilli(), Connection: row.Connection, ID: row.ID}); err != nil {
			return err
		}
		return q.SetRunState(ctx, db.SetRunStateParams{
			State: string(v1.RunClaimed), UpdatedAt: now.UnixMilli(), Connection: row.Connection, ID: row.ID,
		})
	})
	if err != nil {
		release()
		if ours {
			r.Exec.hold(Claim{Connection: row.Connection, Run: held})
		}
		log.Error("the run could not be taken out of waiting; it stays parked", "err", err)
		return resumeEvery
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
	c, err := claimFor(row, nil)
	if err != nil {
		// Only reachable for a spec that would not parse, which is how this
		// was called in the first place; the run still has to end, so it ends
		// with the id the row has and nothing from the spec.
		c = Claim{Connection: row.Connection, Run: v1.Run{RunID: row.ID, Harness: row.Harness}, Release: func() {}}
		c.Run.Session = v1.SessionRef{ID: row.SessionID}
	}
	if err := r.Exec.finishParked(ctx, c, row, state, rerr, now); err != nil {
		return
	}
	// Only now. The transaction leaves the row exactly as it was when it
	// fails, so forgetting the claim first would leave a run still `waiting`
	// whose grants this process no longer holds — and the next sweep reports
	// it lost as grants_lost, which decision 0023 makes final, so the state
	// the hub actually asked for could never be sent.
	r.Exec.forget(row.Connection, row.ID)
}

// finishParked writes the terminal state and result of a run that ended while
// it was parked. It reads the run's cost back from the row rather than from
// memory, because the process that parked it may be gone.
//
// Ending the wait goes in the same transaction as the result. Apart, a
// failure of the result leaves a row still `waiting` whose resumes_at the
// wait's end has already cleared — which reads as due now, so the next sweep
// runs a turn for a run that was cancelled or timed out.
func (e *Exec) finishParked(ctx context.Context, c Claim, row db.Run, state v1.RunState, rerr *v1.RunError, now time.Time) error {
	e.init()
	prog := progress{started: now, waitedMS: row.WaitedMs + waitingFor(row, now), switches: int(row.AccountSwitches)}
	if row.StartedAt.Valid {
		prog.started = time.UnixMilli(row.StartedAt.Int64)
	}
	if row.Spent.Valid && row.Spent.String != "" {
		if err := json.Unmarshal([]byte(row.Spent.String), &prog.spent); err != nil {
			e.Log.Warn("what the run's turns cost could not be read; its result carries none of it",
				"connection", c.Connection, "run", c.Run.RunID, "err", err)
		}
	}
	return e.finishWith(ctx, c, v1.Result{
		State: state, LastSeq: e.lastSeq(ctx, c), Error: rerr,
		Usage:   v1.RunUsage{ByModel: prog.spent.Usage},
		Metrics: prog.metrics(now),
	}, func(q *db.Queries) error {
		return q.EndRunWait(ctx, db.EndRunWaitParams{Now: now.UnixMilli(), Connection: row.Connection, ID: row.ID})
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
	c, cerr := claimFor(row, nil)
	if cerr != nil {
		c = Claim{Connection: connection, Run: v1.Run{RunID: runID, Harness: row.Harness, Session: v1.SessionRef{ID: row.SessionID}}, Release: func() {}}
	}
	// No error, as for any run the hub cancelled: the hub asked, so it knows
	// why, and a runner that invented a reason here would be the only one of
	// the two making something up.
	if err := r.Exec.finishParked(ctx, c, row, v1.RunCancelled, nil, now); err != nil {
		// Still parked, and still this process's to resume or cancel. The
		// hub repeats a cancel until the run ends (decision 0025), so the
		// next sync tries again.
		return false
	}
	r.Exec.forget(connection, runID)
	return true
}
