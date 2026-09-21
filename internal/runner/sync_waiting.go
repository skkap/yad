package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/store/db"
	v1 "github.com/skkap/yad/protocol/v1"
)

// A run parked on a usage limit comes back through its connection's own sync
// loop, and through nothing else.
//
// It was a process-wide sweep of its own, which is the shape this file
// exists to be instead of. The sweep started runs on its own initiative, so
// every clause of "may I start work now?" had to be restated there — and
// four review rounds each found one that had not been: it started runs for
// connections it was not syncing, then before the first sync, then without
// noticing that readiness had changed, then from a snapshot of a liveness
// that had since ended. A predicate copied by hand drifts from the original,
// and a snapshot cannot track one at all.
//
// Here there is nothing to restate. A claim *is* an answer to a sync, and so
// is a resume: it runs inside SyncOnce, so it cannot precede a sync, cannot
// run before Recover or while draining, takes its capacity from the same
// reservation a claim does, and cannot *start* on a connection with no
// reporter to deliver its result — not because each was checked, but
// because there is no code path on which they are false. Starting is the
// whole of that last one: a resumed run may well outlive its loop, and is
// meant to, since runsOn starts every run on the server's context so a
// connection that stops leaves the runs in hand to finish — and a loop that
// stops takes its reporter with it.
//
// It costs one sync interval. A run that comes due a moment after a sync
// waits for the next one: 15 s by default, bounded 5–60 s, and a number the
// hub itself chooses. Against a park measured in hours that is nothing, and
// it is the same latency a hub already accepts for every offer it makes —
// but only because holdWaiting takes the run's capacity before the hub is
// asked for more work. Left to compete with the offers, a parked run on a
// busy hub waits not one interval but for ever.

// resumeSkew is added to a run's resume time before it is taken as due. The
// reset came from the harness, whose clock is its own; starting a turn a
// moment early gets the same limit back and costs a whole cache-cold turn,
// while starting a moment late costs a moment.
const resumeSkew = 5 * time.Second

// loadAccounts reads this sync's account states, once. Whoever needs them
// first calls it and everyone after reads what it left, so there is no
// order to get wrong — and no second reading of the same question within
// one sync.
//
// Once means once *attempted*, not once succeeded. A read that failed and
// then succeeded on the next caller within the same sync would be the
// disagreement this exists to prevent: holdWaiting would have run against
// no accounts while health and the resume used real ones, so a run
// holdWaiting judged not due would fall back to competing with the hub's
// offers for its unit. A sync that cannot see the accounts sees none of
// them, all the way through.
func (l *Loop) loadAccounts(ctx context.Context) {
	if l.accountsTried {
		return
	}
	l.accountsTried = true
	accounts, err := account.Load(ctx, l.Store.Queries, l.Data, l.Config, l.Clock.Now())
	if err != nil {
		// What is actually lost is the early resume: a run whose account
		// freed *before* its resume time waits for the next sync. One whose
		// resume time has passed is due on the clock alone and still runs,
		// so "resumes nothing" would send an operator looking for a stuck
		// run that is not stuck.
		l.accountsErr = err
		l.Log.Warn("could not read account states; this sync reports no harness health, and a parked run whose account freed before its resume time waits for the next sync",
			"connection", l.Connection, "err", err)
		return
	}
	l.accounts = accounts
}

// holdWaiting takes a unit of this sync's reservation for each parked run
// that looks due, before the request is built, and returns them by run id.
//
// It is a reservation and not a decision: what becomes of each run is
// settled after the hub has answered, against a freshly read row. A unit
// held for a run that turns out to be cancelled, or not due after all, is
// given back at the end of the same sync, so a wrong guess here costs one
// sync's worth of one unit and nothing else. That is why this may read the
// stale snapshot and considerWaiting may not.
func (l *Loop) holdWaiting(held []db.Run, res *Reservation) map[string]func() {
	if l.Executor == nil {
		return nil
	}
	now := l.Clock.Now()
	var reserved map[string]func()
	for _, row := range held {
		if row.State != string(v1.RunWaiting) {
			continue
		}
		var run v1.Run
		if err := json.Unmarshal([]byte(row.Spec), &run); err != nil {
			continue // ended, not started, after the answer
		}
		if waited := row.WaitedMs + waitingFor(row, now); run.MaxWaitMS > 0 && waited >= run.MaxWaitMS {
			continue // the same
		}
		if l.cancelled[row.ID] || !l.due(row, run, now) {
			continue
		}
		release, ok := res.Take(run.Harness)
		if !ok {
			continue
		}
		if reserved == nil {
			reserved = map[string]func(){}
		}
		reserved[row.ID] = release
	}
	return reserved
}

// resumeWaiting settles what becomes of each parked run of this connection,
// with the capacity holdWaiting put aside for it. held is the listing this
// sync made *before* the hub answered, so it is only a list of candidates:
// every row is read again before it is acted on.
func (l *Loop) resumeWaiting(ctx context.Context, held []db.Run, res *Reservation, reserved map[string]func()) {
	if l.Executor == nil || !l.mayClaim() {
		return
	}
	now := l.Clock.Now()
	waiting := make(map[string]bool, len(held))
	for _, row := range held {
		if row.State != string(v1.RunWaiting) {
			continue
		}
		waiting[row.ID] = true
		l.considerWaiting(ctx, row, res, reserved, now)
	}
	// A run that left waiting some other way — the collector timed it out
	// while its cancel could not be written — takes its remembered cancel
	// with it. Without this the entry outlives the run.
	for id := range l.cancelled {
		if !waiting[id] {
			delete(l.cancelled, id)
		}
	}
}

// considerWaiting decides what becomes of one parked run of this connection.
//
// The row is read again first, and this is not caution. The listing it came
// from was made before the hub was asked, and the hub's answer has since
// been acted on: a cancel in it ends the run, writes `cancelled` and gives
// up the claim — all of which the stale row knows nothing about. Acting on
// that copy wrote `claimed` over the cancelled row and started the harness,
// so a run the hub had asked to stop ran a turn, with the outbox's
// ON CONFLICT DO NOTHING swallowing the second result. The sweep this
// replaced could not do that, because it listed afresh on every pass.
func (l *Loop) considerWaiting(ctx context.Context, stale db.Run, res *Reservation, reserved map[string]func(), now time.Time) {
	log := l.Log.With("connection", l.Connection, "run", stale.ID)
	row, err := l.Store.GetRun(ctx, db.GetRunParams{Connection: l.Connection, ID: stale.ID})
	if err != nil {
		log.Warn("a parked run could not be read again; it is left as it is", "err", err)
		return
	}
	if row.State != string(v1.RunWaiting) {
		return // it stopped waiting while this sync was in flight
	}
	// A cancel this process could not write down is still a cancel, and the
	// row says nothing about it: a failed transaction leaves it exactly as
	// it was, which is what keeps the run's grants and its wait, and makes
	// it indistinguishable from a row nobody cancelled.
	//
	// Retried here, above everything else in the same function that would
	// otherwise start the run. That ordering is the whole guarantee — there
	// is no window between the two for the run to slip through, because
	// they are the same pass over the same row. It used to be a race
	// against the hub's repeat, which loses: the hub's interval is 15 s by
	// default (sync.go, defaultInterval) and the only other thing that
	// would reconsider this run is this loop's own wait,
	// `l.Clock.After(l.jitter(wait))` in Run.
	if l.cancelled[row.ID] {
		if !l.endWait(ctx, row, v1.RunCancelled, nil, now) {
			log.Warn("the run was cancelled and its result still cannot be recorded; it is not started and is tried again")
		}
		return
	}
	var run v1.Run
	if err = json.Unmarshal([]byte(row.Spec), &run); err != nil {
		// Unreadable, so it can never run and never resume: ending it is the
		// only way the hub hears anything at all about it.
		l.endWait(ctx, row, v1.RunFailed, &v1.RunError{Class: ClassAdapter,
			Message: "the parked run could not be read back from this runner's store (" + err.Error() + ") — offer it again"}, now)
		return
	}
	waited := row.WaitedMs + waitingFor(row, now)
	if run.MaxWaitMS > 0 && waited >= run.MaxWaitMS {
		log.Warn("the run waited longer than its hub allowed; it is timed out", "waited_ms", waited, "max_wait_ms", run.MaxWaitMS)
		l.endWait(ctx, row, v1.RunTimedOut, &v1.RunError{Class: ClassMaxWait, Message: maxWaitMessage(waited, run.MaxWaitMS)}, now)
		return
	}
	if !l.due(row, run, now) {
		return
	}
	// The grants are the one thing the row cannot hold. A run that had them
	// and is not the one this process parked has lost them for good: it is
	// reported rather than started, so the failure is a sentence the hub can
	// act on instead of a harness failing on a missing credential.
	parked, ours := l.Executor.Parked(l.Connection, row.ID)
	if !ours {
		if row.HadGrants != 0 {
			log.Warn("the run was parked by an earlier process and its grants did not survive; the hub is asked for it again")
			l.endWait(ctx, row, v1.RunLost, &v1.RunError{Class: ClassGrantsLost,
				Message: "this run was waiting for a free account when the runner stopped, and the grants it was given do not survive a restart — offer it again"}, now)
			return
		}
		parked = run
	}
	release, ok := reserved[row.ID]
	if ok {
		delete(reserved, row.ID)
	} else if release, ok = res.Take(run.Harness); !ok {
		// No unit was put aside for it — it was not due when the request
		// was built — and none is left over from the offers. The next sync
		// holds one for it before asking.
		return
	}
	// Ending the wait and leaving waiting are one transaction, because
	// EndRunWait clears resumes_at and a row with no resumes_at reads as due
	// right now: written separately, a failure of the second would leave a
	// row every later sync starts again.
	// Still waiting, checked inside the transaction: the collector ends a
	// parked run past its cap from another goroutine, so the read above is
	// not the last word.
	err = l.Store.Tx(ctx, func(q *db.Queries) error {
		switch fresh, err := q.GetRun(ctx, db.GetRunParams{Connection: l.Connection, ID: row.ID}); {
		case err != nil:
			return err
		case fresh.State != string(v1.RunWaiting):
			return errNoLongerWaiting
		}
		if err := q.EndRunWait(ctx, db.EndRunWaitParams{Now: now.UnixMilli(), Connection: l.Connection, ID: row.ID}); err != nil {
			return err
		}
		return q.SetRunState(ctx, db.SetRunStateParams{
			State: string(v1.RunClaimed), UpdatedAt: now.UnixMilli(), Connection: l.Connection, ID: row.ID,
		})
	})
	if errors.Is(err, errNoLongerWaiting) {
		release()
		return
	}
	if err != nil {
		release()
		log.Error("the run could not be taken out of waiting; it stays parked", "err", err)
		return
	}
	l.Executor.Forget(l.Connection, row.ID)
	log.Info("the account's limit has reset; the run continues in the same session", "waited_ms", waited)
	l.Executor.Start(ctx, Claim{Connection: l.Connection, Run: parked, Release: release})
}

// errNoLongerWaiting ends a transaction that found the run had stopped
// waiting since it was read: something else has already decided it.
var errNoLongerWaiting = errors.New("the run is no longer waiting")

// due says whether a parked run may start now.
//
// The resume time is the earliest reset known when the run parked, and it is
// a floor rather than the whole answer: an account can become usable before
// it, which is the other half of ARCHITECTURE's "limit resets / account
// frees". The way that happens to a running daemon is the owner finishing a
// login in a home, which LoginProbe notices; `yad account add` of a new
// label does not reach one at all, since the account list is the
// configuration it started with.
//
// Inside resumeSkew of the resume time, nothing counts as early. resumes_at
// is the earliest limited_until among the harness's accounts, and
// account.stateOf reads that account free the instant the moment passes with
// no skew of its own — so for those few seconds a free account and a
// not-yet-due run are the same reset seen through two clocks, and acting on
// it buys exactly the cache-cold turn the skew exists to avoid.
func (l *Loop) due(row db.Run, run v1.Run, now time.Time) bool {
	if !row.ResumesAt.Valid {
		return true
	}
	wait := time.UnixMilli(row.ResumesAt.Int64).Add(resumeSkew).Sub(now)
	switch {
	case wait <= 0:
		return true
	case wait <= resumeSkew:
		return false
	}
	// The accounts this sync's health was built from, not a second reading
	// of them: what the hub was told and what this runner then does come
	// from one answer.
	_, free := account.Soonest(l.accounts, run.Harness, now)
	return free
}

// cancelWaiting ends a parked run of this connection because the hub asked,
// and answers whether the cancellation was recorded — not whether the run
// was parked. False covers both "this run is not waiting" and "it is, and
// the write failed", which is what the caller wants collapsed: in either
// case the control goes on to the executor, and a write that failed is
// remembered here and tried again at the next sync.
//
// A waiting run has no turn to interrupt and no process to signal, so the
// cancel ladder has nothing to climb: the run ends where it stands, with the
// wait it served reported.
func (l *Loop) cancelWaiting(ctx context.Context, runID string) bool {
	l.init()
	if l.Executor == nil {
		return false
	}
	row, err := l.Store.GetRun(ctx, db.GetRunParams{Connection: l.Connection, ID: runID})
	if err != nil || row.State != string(v1.RunWaiting) {
		return false
	}
	l.Log.Info("the run was waiting for a free account and is cancelled", "connection", l.Connection, "run", runID)
	// No error on the result, as for any run the hub cancelled: the hub
	// asked, so it knows why, and a runner that invented a reason here would
	// be the only one of the two making something up.
	return l.endWait(ctx, row, v1.RunCancelled, nil, l.Clock.Now())
}

// endWait writes a parked run's terminal result, and reports whether it
// landed. A cancel that did not is remembered: the row is left exactly as it
// was, so nothing on disk says this run must not be started, and the next
// sync would otherwise read an ordinary parked run.
func (l *Loop) endWait(ctx context.Context, row db.Run, state v1.RunState, rerr *v1.RunError, now time.Time) bool {
	c, err := claimFor(row, nil)
	if err != nil {
		c = Claim{Connection: row.Connection, Release: func() {},
			Run: v1.Run{RunID: row.ID, Harness: row.Harness, Session: v1.SessionRef{ID: row.SessionID}}}
	}
	if err := l.Executor.End(ctx, c, row, state, rerr, now); err != nil {
		if state == v1.RunCancelled {
			l.cancelled[row.ID] = true
		}
		return false
	}
	delete(l.cancelled, row.ID)
	// Only now. The transaction leaves the row exactly as it was when it
	// fails, so forgetting the claim first would leave a run still waiting
	// whose grants this process no longer holds — and the next sync reports
	// it lost as grants_lost, which decision 0023 makes final, so the state
	// the hub actually asked for could never be sent.
	l.Executor.Forget(row.Connection, row.ID)
	return true
}

// maxWaitMessage says how long the run waited and what the hub allowed it.
func maxWaitMessage(waited, cap int64) string {
	return fmt.Sprintf("the run waited %s for a free account, past the %s the hub allowed it; offer it again when an account is free",
		time.Duration(waited)*time.Millisecond, time.Duration(cap)*time.Millisecond)
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

// claimFor rebuilds a claim from a stored run row, for a run this process
// did not park itself. The spec in the store has no grants — Loop.record
// strips them, because a hub's secrets never touch this machine's disk — so
// this is only ever enough to end a run, never to start one.
func claimFor(r db.Run, release func()) (Claim, error) {
	if release == nil {
		release = func() {}
	}
	var run v1.Run
	if err := json.Unmarshal([]byte(r.Spec), &run); err != nil {
		return Claim{}, fmt.Errorf("the stored run could not be read: %w", err)
	}
	return Claim{Connection: r.Connection, Run: run, Release: release}, nil
}
