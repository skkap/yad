package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/store/db"
	v1 "github.com/skkap/yad/protocol/v1"
)

// progress is what a run keeps across its turns and across a park. Every
// field is read from the run's row rather than from memory, because a parked
// run may be picked up by a process that never saw the first turn.
type progress struct {
	// started is when the run first reached preparing, so duration_ms
	// measures the run rather than its last attempt.
	started  time.Time
	waitedMS int64
	switches int
}

// progress reads what earlier turns of this run recorded. A row that cannot
// be read is not worth failing a run over: the metrics restart at zero and
// the run goes on, which is the direction that costs nothing but a number.
func (e *Exec) progress(ctx context.Context, c Claim) progress {
	p := progress{started: time.Now()}
	r, err := e.Store.GetRun(ctx, db.GetRunParams{Connection: c.Connection, ID: c.Run.RunID})
	if err != nil {
		e.Log.Warn("could not read what the run has cost so far; its metrics start again from zero",
			"connection", c.Connection, "run", c.Run.RunID, "err", err)
		return p
	}
	if r.StartedAt.Valid {
		p.started = time.UnixMilli(r.StartedAt.Int64)
	}
	p.waitedMS, p.switches = r.WaitedMs, int(r.AccountSwitches)
	return p
}

// lastSeq is the highest event number this run has already spooled. A run on
// its first turn has none and starts at zero; one that parked and came back
// continues its own stream, because (run, seq) is the idempotency key the
// protocol promises and a repeated number is dropped, not renumbered.
func (e *Exec) lastSeq(ctx context.Context, c Claim) int64 {
	seq, err := e.Store.LastEventSeq(ctx, db.LastEventSeqParams{Connection: c.Connection, RunID: c.Run.RunID})
	if err != nil {
		e.Log.Error("could not read the run's last event number; its next events may be lost",
			"connection", c.Connection, "run", c.Run.RunID, "err", err)
		return 0
	}
	return seq
}

func (e *Exec) setStarted(ctx context.Context, c Claim, at time.Time) {
	err := e.Store.SetRunStarted(ctx, db.SetRunStartedParams{
		StartedAt:  sql.NullInt64{Int64: at.UnixMilli(), Valid: true},
		UpdatedAt:  time.Now().UnixMilli(),
		Connection: c.Connection, ID: c.Run.RunID,
	})
	if err != nil {
		e.Log.Warn("could not record when the run started", "connection", c.Connection, "run", c.Run.RunID, "err", err)
	}
}

// park puts the run in waiting and reports whether it did.
//
// Waiting holds no process and no goroutine. The run's own goroutine ends
// here: what it was doing is on disk — the state, the moment it comes back,
// the wait so far and the accounts it has been through — and the Resumer
// builds the run again from that row. A parked goroutine would pass the
// acceptance test and still be wrong, because a `kill -9` has no goroutine to
// wake; with the run reconstructed from the row every time, a restart is not
// a second path to keep in step with the first, it is the first path.
//
// It parks only when a reset ends the wait. Every account needing a login is
// not a wait, it is a job for the owner, and a run parked on it would sit
// until someone noticed; that case falls through to the caller, which refuses
// the run and names the command to run.
func (e *Exec) park(ctx context.Context, c Claim, prog *progress, lastSeq *int64, reason string) bool {
	accounts, err := account.Load(ctx, e.Store.Queries, e.Data, e.Config)
	if err != nil {
		e.Log.Warn("could not read account states; the run is not parked", "connection", c.Connection, "run", c.Run.RunID, "err", err)
		return false
	}
	at := account.NextFree(accounts, c.Run.Harness)
	if at.IsZero() {
		return false
	}
	now := time.Now()
	log := e.Log.With("connection", c.Connection, "run", c.Run.RunID)
	// A cap already spent is not a wait the run gets to start. Checked here
	// as well as in the Resumer so a run whose hub allowed it less time than
	// it has already waited ends now rather than after one more park.
	if c.Run.MaxWaitMS > 0 && prog.waitedMS >= c.Run.MaxWaitMS {
		e.finish(ctx, c, v1.Result{
			State: v1.RunTimedOut, LastSeq: *lastSeq,
			Error: &v1.RunError{Class: ClassMaxWait, Message: maxWaitMessage(prog.waitedMS, c.Run.MaxWaitMS)},
			Metrics: v1.Metrics{DurationMS: time.Since(prog.started).Milliseconds(),
				WaitedMS: prog.waitedMS, AccountSwitches: prog.switches},
		})
		return true
	}
	ev := v1.Event{Kind: v1.EventStatus, Status: "waiting", Text: reason + " — the run continues at " + at.UTC().Format(time.RFC3339)}
	if e.spool(ctx, c, &ev, *lastSeq+1) {
		*lastSeq = ev.Seq
	}
	err = e.Store.SetRunWaiting(ctx, db.SetRunWaitingParams{
		ResumesAt:    sql.NullInt64{Int64: at.UnixMilli(), Valid: true},
		WaitingSince: sql.NullInt64{Int64: now.UnixMilli(), Valid: true},
		// Written with the park rather than after every move: a run that
		// switches and then finishes reports the count from memory, and one
		// that parks is the only one another process has to read it back for.
		AccountSwitches: int64(prog.switches),
		Reason:          sql.NullString{String: reason, Valid: true},
		UpdatedAt:       now.UnixMilli(),
		Connection:      c.Connection, ID: c.Run.RunID,
	})
	if err != nil {
		// Nothing was parked, so the caller's own path — refusing the run —
		// is the right answer: a run that neither waits nor ends is one the
		// hub hears nothing about at all.
		log.Error("the run could not be parked", "err", err)
		return false
	}
	// Kept so a resume in this process still has the run's grants; a resume
	// after a restart cannot, which is what had_grants exists to say.
	e.hold(c)
	e.report(c.Connection)
	// A close of the session may have been waiting on this run, and it still
	// is: a waiting run is a run held. The collector is woken anyway, because
	// it also recounts what is live.
	if e.Ended != nil {
		e.Ended()
	}
	log.Info("no account is free; the run waits and holds no process", "resumes_at", at.UTC(), "reason", reason)
	return true
}

func maxWaitMessage(waited, cap int64) string {
	return fmt.Sprintf("the run waited %s for a free account, past the %s the hub allowed it; offer it again when an account is free",
		time.Duration(waited)*time.Millisecond, time.Duration(cap)*time.Millisecond)
}

// noteMove puts the move into the run's own event stream, where whoever is
// watching the run can see why it changed accounts. Labels and a reset time:
// nothing else about an account leaves this machine.
func (e *Exec) noteMove(ctx context.Context, c Claim, lastSeq *int64, from, to account.Account, limit *adapter.Limit) {
	text := "the account " + from.Label + " is at a usage limit"
	if limit != nil && limit.Window != "" {
		text += " on its " + limit.Window + " window"
	}
	if limit != nil && !limit.ResetAt.IsZero() {
		text += " until " + limit.ResetAt.UTC().Format(time.RFC3339)
	}
	text += "; the run continues on " + to.Label + " in the same session"
	ev := v1.Event{Kind: v1.EventStatus, Status: "account_switch", Text: text}
	if e.spool(ctx, c, &ev, *lastSeq+1) {
		*lastSeq = ev.Seq
		e.report(c.Connection)
	}
}

// hold keeps a parked run's claim, grants and all, for a resume in this
// process. Grants deliberately never reach disk, so this is the only place a
// parked run's are: a later process finds none and cannot rebuild the run.
func (e *Exec) hold(c Claim) {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.parked[runKey{c.Connection, c.Run.RunID}] = c.Run
}

// takeParked returns a parked run's claim as this process kept it, and forgets
// it: whoever takes it is now responsible for ending or re-parking it.
func (e *Exec) takeParked(connection, runID string) (v1.Run, bool) {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	key := runKey{connection, runID}
	run, ok := e.parked[key]
	delete(e.parked, key)
	return run, ok
}

// forget drops a parked run this process was keeping, without taking it: for
// a run that ended some other way.
func (e *Exec) forget(connection, runID string) {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.parked, runKey{connection, runID})
}

// claimFor rebuilds a claim from a stored run row, for a run this process did
// not park itself. release is the capacity it holds, and may be nil.
//
// The spec in the store has no grants — Loop.record strips them, because a
// hub's secrets never touch this machine's disk — so a run that had them
// cannot be rebuilt here. That is not a silent difference: it would start the
// harness without the credentials the hub gave it and fail in whatever way
// the missing secret happens to fail.
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
