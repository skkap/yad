package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/store/db"
	v1 "github.com/skkap/yad/protocol/v1"
)

// progress is what a run keeps across its turns and across a park. Every
// field is read from the run's row rather than from memory, because a parked
// run may be picked up by a process that never saw the first turn.
//
// The rule it exists to keep: a run is one run however many accounts and
// however many processes it took. Its duration covers the whole of it, its
// usage covers every turn (protocol/v1.Result says so), and the hub's
// wall-clock cap is a cap on the run rather than a fresh budget per account.
type progress struct {
	// started is when the run first reached preparing, so duration_ms
	// measures the run rather than its last attempt.
	started  time.Time
	waitedMS int64
	switches int
	// account is the last account a turn of this run ran on. A resumed run
	// that takes a different one has moved, and that move counts exactly as
	// an in-turn move does.
	account string
	spent   spent
}

// spent is what a run's earlier turns cost, carried across a park as JSON in
// the run's row. A result built from the last turn alone would tell a hub
// that a run which spent an account's whole five-hour window used only what
// the turn after it did.
type spent struct {
	Usage      map[string]v1.Usage `json:"usage,omitempty"`
	ToolCalls  int                 `json:"tool_calls,omitempty"`
	APIRetries int                 `json:"api_retries,omitempty"`
	Stalls     int                 `json:"stalls,omitempty"`
	// FirstEventMS is how long the first turn that answered took to answer,
	// measured from that turn's own start, and is only ever set once.
	//
	// Not from the run's start, which would be the natural reading of "the
	// whole run is one run" and is wrong here: a run that parked for five
	// hours and then answered in two seconds would report five hours to
	// first token, with the wait counted a second time beside waited_ms. The
	// metric answers "why is the harness slow" (ARCHITECTURE.md §2), and
	// neither a park nor a repository clone is the harness being slow.
	FirstEventMS int64 `json:"first_event_ms,omitempty"`
	// ExecutedMS is how long earlier turns held a process. It is what the
	// hub's wall_clock_ms is spent against, and it deliberately excludes
	// waiting: a run parked for five hours has used none of its cap, or
	// max_wait_ms and wall_clock_ms would be two names for one limit.
	ExecutedMS int64 `json:"executed_ms,omitempty"`
	// Set is what tells an unset FirstEventMS of 0 from a turn that really
	// answered within a millisecond.
	Answered bool `json:"answered,omitempty"`
}

// absorb folds a finished turn into what the run has spent.
func (p *progress) absorb(out adapter.Outcome, w watch, turnStarted time.Time, now time.Time) {
	p.spent.ExecutedMS += max(now.Sub(turnStarted).Milliseconds(), 0)
	p.spent.ToolCalls += w.toolCalls
	p.spent.APIRetries += out.APIRetries
	p.spent.Stalls += w.stalls
	if !p.spent.Answered && w.firstEventMS >= 0 {
		p.spent.Answered = true
		p.spent.FirstEventMS = w.firstEventMS
	}
	for model, u := range out.Usage {
		if p.spent.Usage == nil {
			p.spent.Usage = map[string]v1.Usage{}
		}
		p.spent.Usage[model] = addUsage(p.spent.Usage[model], u)
	}
}

// addUsage sums two turns' usage for one model. Cost is summed only when at
// least one side reported it; a nil stays nil, because a harness that gives
// no price must not be read as having charged nothing.
func addUsage(a, b v1.Usage) v1.Usage {
	model := a.Model
	if model == "" {
		model = b.Model
	}
	out := v1.Usage{
		Model: model, Input: a.Input + b.Input, Output: a.Output + b.Output,
		CacheRead: a.CacheRead + b.CacheRead, CacheWrite: a.CacheWrite + b.CacheWrite,
	}
	if a.CostUSD != nil || b.CostUSD != nil {
		total := 0.0
		if a.CostUSD != nil {
			total += *a.CostUSD
		}
		if b.CostUSD != nil {
			total += *b.CostUSD
		}
		out.CostUSD = &total
	}
	return out
}

// wallClockLeft is what remains of the hub's cap for the next turn, and
// whether there is a cap at all. Zero or less means the run has used it up.
func (p *progress) wallClockLeft(run v1.Run) (time.Duration, bool) {
	if run.WallClockMS <= 0 {
		return 0, false
	}
	return time.Duration(run.WallClockMS-p.spent.ExecutedMS) * time.Millisecond, true
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
	p.waitedMS, p.switches, p.account = r.WaitedMs, int(r.AccountSwitches), r.Account.String
	if r.Spent.Valid && r.Spent.String != "" {
		if err := json.Unmarshal([]byte(r.Spent.String), &p.spent); err != nil {
			// The turns already taken are lost to the metrics, and nothing
			// else reads this: the run continues rather than failing over a
			// number.
			e.Log.Warn("what the run's earlier turns cost could not be read; its usage and counters start again from zero",
				"connection", c.Connection, "run", c.Run.RunID, "err", err)
			p.spent = spent{}
		}
	}
	return p
}

// progressOf is what a run's row says it has cost, for whoever has to report
// a run they did not watch: the collector ending a parked one, and the start
// that finds a run a previous process held.
//
// until is the moment the run is being measured to, and it is the caller's
// because the two have different answers. A run still parked is measured to
// now; one whose process is gone stopped at some unknown moment before this
// process started, so its caller passes the last moment anything is known to
// have been true of it. Measuring that one to now would report how long the
// machine was off.
func progressOf(row db.Run, until time.Time, log *slog.Logger) progress {
	p := progress{started: until, waitedMS: row.WaitedMs + waitingFor(row, until), switches: int(row.AccountSwitches)}
	if row.StartedAt.Valid {
		p.started = time.UnixMilli(row.StartedAt.Int64)
	}
	if row.Spent.Valid && row.Spent.String != "" {
		if err := json.Unmarshal([]byte(row.Spent.String), &p.spent); err != nil {
			// Cleared, not left as it fell. json.Unmarshal fills what it
			// parsed before it failed, so without this the result ships
			// half a decode while the line below says it ships none of it.
			// Saying nothing is better than saying a wrong number, and the
			// two have to agree.
			p.spent = spent{}
			log.Warn("what the run's turns cost could not be read; its result carries none of it",
				"connection", row.Connection, "run", row.ID, "err", err)
		}
	}
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
// the wait so far and the accounts it has been through — and the run's own
// connection builds it again from that row at its next sync. A parked goroutine would pass the
// acceptance test and still be wrong, because a `kill -9` has no goroutine to
// wake; with the run reconstructed from the row every time, a restart is not
// a second path to keep in step with the first, it is the first path.
//
// It parks only when a reset ends the wait. Every account needing a login is
// not a wait, it is a job for the owner, and a run parked on it would sit
// until someone noticed; that case falls through to the caller, which refuses
// the run and names the command to run. A needs-login account that the owner
// then logs in does bring a parked run back early — LoginProbe frees it and
// the next sync reads it — but it cannot be what a run is parked on, because
// nothing can say when that will happen.
func (e *Exec) park(ctx context.Context, c Claim, prog *progress, lastSeq *int64, reason string) bool {
	now := time.Now()
	accounts, err := account.Load(ctx, e.Store.Queries, e.Data, e.Config, now)
	if err != nil {
		e.Log.Warn("could not read account states; the run is not parked", "connection", c.Connection, "run", c.Run.RunID, "err", err)
		return false
	}
	at := account.NextFree(accounts, c.Run.Harness)
	if at.IsZero() {
		return false
	}
	log := e.Log.With("connection", c.Connection, "run", c.Run.RunID)
	// A cap already spent is not a wait the run gets to start. Checked here
	// as well as in the sync loop, so a run whose hub allowed it less time
	// than it has already waited ends now rather than after one more park.
	if c.Run.MaxWaitMS > 0 && prog.waitedMS >= c.Run.MaxWaitMS {
		e.finish(ctx, c, v1.Result{
			State: v1.RunTimedOut, LastSeq: *lastSeq,
			Error:   &v1.RunError{Class: ClassMaxWait, Message: maxWaitMessage(prog.waitedMS, c.Run.MaxWaitMS)},
			Usage:   v1.RunUsage{ByModel: prog.spent.Usage},
			Metrics: prog.metrics(now),
		})
		return true
	}
	ev := v1.Event{Kind: v1.EventStatus, Status: "waiting", Text: reason + " — the run continues at " + at.UTC().Format(time.RFC3339)}
	if e.spool(ctx, c, &ev, *lastSeq+1) {
		*lastSeq = ev.Seq
	}
	// Written with the park rather than after every turn: a run that moves
	// and then finishes reports these from memory, and one that parks is the
	// only one another process has to read them back for.
	var carried sql.NullString
	if body, merr := json.Marshal(prog.spent); merr == nil {
		carried = sql.NullString{String: string(body), Valid: true}
	} else {
		log.Warn("what this run's turns have cost could not be recorded; its usage restarts if another process resumes it", "err", merr)
	}
	err = e.Store.SetRunWaiting(ctx, db.SetRunWaitingParams{
		ResumesAt:       sql.NullInt64{Int64: at.UnixMilli(), Valid: true},
		WaitingSince:    sql.NullInt64{Int64: now.UnixMilli(), Valid: true},
		AccountSwitches: int64(prog.switches),
		Spent:           carried,
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

// noteMove puts the move into the run's own event stream, where whoever is
// watching the run can see why it changed accounts. Labels and a reset time:
// nothing else about an account leaves this machine.
func (e *Exec) noteMove(ctx context.Context, c Claim, lastSeq *int64, from, to string, limit *adapter.Limit) {
	text := "the account " + from + " is at a usage limit"
	if limit != nil && limit.Window != "" {
		text += " on its " + limit.Window + " window"
	}
	if limit != nil && !limit.ResetAt.IsZero() {
		text += " until " + limit.ResetAt.UTC().Format(time.RFC3339)
	}
	text += "; the run continues on " + to + " in the same session"
	ev := v1.Event{Kind: v1.EventStatus, Status: "account_switch", Text: text}
	if e.spool(ctx, c, &ev, *lastSeq+1) {
		*lastSeq = ev.Seq
		e.report(c.Connection)
	}
}

// metrics is the whole run's metrics, not the last turn's: every turn it has
// taken, plus the waits between them.
func (p *progress) metrics(now time.Time) v1.Metrics {
	return v1.Metrics{
		DurationMS: max(now.Sub(p.started).Milliseconds(), 0), FirstEventMS: p.spent.FirstEventMS,
		ToolCalls: p.spent.ToolCalls, APIRetries: p.spent.APIRetries, Stalls: p.spent.Stalls,
		WaitedMS: p.waitedMS, AccountSwitches: p.switches,
	}
}

// Parked is a run this process put in waiting, whole. False for one parked
// by an earlier process: its grants live only in the process that claimed
// them, so what is left in the store cannot be started.
func (e *Exec) Parked(connection, runID string) (v1.Run, bool) {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	run, ok := e.parked[runKey{connection, runID}]
	return run, ok
}

// Forget drops the claim this process was keeping for a parked run.
func (e *Exec) Forget(connection, runID string) {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.parked, runKey{connection, runID})
}

// End writes a parked run's terminal state and result, ending its wait in
// the same transaction, and reads what the run cost back from the row rather
// than from memory — the process that parked it may be gone.
//
// One transaction, because EndRunWait clears resumes_at and a waiting row
// without one reads as due right now: apart, a failed result would leave a
// run still waiting and permanently due, and the next sync would run a turn
// for one that was cancelled or timed out.
//
// The same transaction checks the run is still waiting, and that is not
// belt and braces: the collector ends a parked run past its cap from its
// own goroutine while a connection's sync may be resuming it. Without the
// check the collector's write lands on a run that is already running and
// reports it timed out, which is a second terminal state for one run.
func (e *Exec) End(ctx context.Context, c Claim, row db.Run, state v1.RunState, rerr *v1.RunError, now time.Time) error {
	e.init()
	prog := progressOf(row, now, e.Log)
	return e.finishWith(ctx, c, v1.Result{
		State: state, LastSeq: e.lastSeq(ctx, c), Error: rerr,
		Usage:   v1.RunUsage{ByModel: prog.spent.Usage},
		Metrics: prog.metrics(now),
	}, func(q *db.Queries) error {
		switch fresh, err := q.GetRun(ctx, db.GetRunParams{Connection: row.Connection, ID: row.ID}); {
		case err != nil:
			return err
		case fresh.State != string(v1.RunWaiting):
			return errNoLongerWaiting
		}
		return q.EndRunWait(ctx, db.EndRunWaitParams{Now: now.UnixMilli(), Connection: row.Connection, ID: row.ID})
	})
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
