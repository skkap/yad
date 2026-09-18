package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// Timings of the reporter (ARCHITECTURE.md §2): events go out about every
// second; a result is retried from the outbox with backoff up to five minutes,
// for as long as it takes.
const (
	reportEvery       = time.Second
	firstResultRetry  = time.Second
	maxResultInterval = 5 * time.Minute
)

// ReportHub is the part of hubclient.Client the reporter uses.
type ReportHub interface {
	Events(ctx context.Context, runID string, batch v1.EventBatch) (v1.EventAck, error)
	Result(ctx context.Context, runID string, res v1.Result) error
}

var _ ReportHub = (*hubclient.Client)(nil)

// Reporter uploads one connection's spooled events and delivers its outbox.
// Everything it sends is already on disk, so it holds nothing that must
// survive it: a restart, or a hub that was down for an hour, resumes exactly
// where the store says — after the hub's acked_through, and at every result still owed.
type Reporter struct {
	Connection string
	Hub        ReportHub
	Store      *store.Store
	// Now is the clock for the outbox's retry schedule; nil is time.Now.
	Now func() time.Time
	Log *slog.Logger

	wake chan struct{}
	// batch is a smaller batch size for a run whose upload was refused as too
	// large — by the hub, or by a proxy in front of it with a lower limit.
	batch map[string]int64
}

func (r *Reporter) init() {
	if r.wake == nil {
		r.wake = make(chan struct{}, 1)
		r.batch = map[string]int64{}
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Log == nil {
		r.Log = slog.New(slog.DiscardHandler)
	}
}

// NewReporter returns a reporter ready to be woken before Run starts.
func NewReporter(connection string, hub ReportHub, st *store.Store, log *slog.Logger) *Reporter {
	r := &Reporter{Connection: connection, Hub: hub, Store: st, Log: log}
	r.init()
	return r
}

// Wake asks for a flush now rather than at the next tick. It never blocks.
func (r *Reporter) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run flushes every tick, and whenever woken, until ctx ends. The first flush
// is at once: that is the replay of whatever a previous process left owed.
func (r *Reporter) Run(ctx context.Context) {
	r.init()
	t := time.NewTicker(reportEvery)
	defer t.Stop()
	for {
		r.Flush(ctx)
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-t.C:
		}
	}
}

// Flush uploads every run's unacknowledged events, then sends every result
// that is due. A run's result waits until its events are in: a hub that shows
// a run finished should already hold what led there. Flush runs on one
// goroutine at a time.
func (r *Reporter) Flush(ctx context.Context) {
	r.init()
	runs, err := r.Store.RunsWithUnackedEvents(ctx, r.Connection)
	if err != nil {
		r.Log.Error("spool not read", "connection", r.Connection, "err", err)
		return
	}
	for _, run := range runs {
		if ctx.Err() != nil {
			return
		}
		r.upload(ctx, run)
	}
	due, err := r.Store.DueOutbox(ctx, db.DueOutboxParams{Connection: r.Connection, NextAttemptAt: r.Now().UnixMilli()})
	if err != nil {
		r.Log.Error("outbox not read", "connection", r.Connection, "err", err)
		return
	}
	for _, o := range due {
		if ctx.Err() != nil {
			return
		}
		// Asked now, not taken from the uploads above: the executor may have
		// spooled a run's last events and written its result since. Its
		// result is written only after its last event, so a spool found
		// empty here stays empty.
		behind, err := r.Store.HasUnackedEvents(ctx, db.HasUnackedEventsParams{Connection: r.Connection, RunID: o.RunID})
		if err != nil {
			r.Log.Error("spool not read", "connection", r.Connection, "run", o.RunID, "err", err)
			continue
		}
		if !behind {
			r.deliver(ctx, o)
		}
	}
}

// upload sends a run's unacknowledged events in batches until the hub has
// them all or an answer says to stop for now.
func (r *Reporter) upload(ctx context.Context, runID string) {
	log := r.Log.With("connection", r.Connection, "run", runID)
	for {
		limit := int64(eventBatch)
		if n, ok := r.batch[runID]; ok {
			limit = n
		}
		rows, err := r.Store.UnackedEvents(ctx, db.UnackedEventsParams{Connection: r.Connection, RunID: runID, Limit: limit})
		if err != nil {
			log.Error("spool not read", "err", err)
			return
		}
		if len(rows) == 0 {
			delete(r.batch, runID)
			return
		}
		batch := v1.EventBatch{Events: make([]v1.Event, 0, len(rows))}
		for _, row := range rows {
			var ev v1.Event
			if err := json.Unmarshal([]byte(row.Body), &ev); err != nil {
				log.Error("spooled event unreadable", "seq", row.Seq, "err", err)
				return
			}
			batch.Events = append(batch.Events, ev)
		}
		ack, err := r.Hub.Events(ctx, runID, batch)
		var se *hubclient.StatusError
		switch {
		case err != nil && final(err):
			// The hub will never take these: the run is not this runner's, or
			// the hub judged them invalid. Resending forever would only keep
			// the result behind them from going out.
			log.Error("the hub refused the run's events; they stay only in the local spool", "err", err)
			if err := r.Store.DropEvents(ctx, db.DropEventsParams{Connection: r.Connection, RunID: runID}); err != nil {
				log.Error("spool not updated", "err", err)
			}
			delete(r.batch, runID)
			return
		case errors.As(err, &se) && se.Status == http.StatusRequestEntityTooLarge && limit > 1:
			// A proxy's body limit is not the hub's verdict: the same events
			// go again in halves, down to one at a time.
			r.batch[runID] = limit / 2
			log.Warn("event batch too large for the hub or a proxy before it; halving", "batch", limit/2)
			continue
		case err != nil:
			log.Warn("events not uploaded; retrying", "err", err)
			return
		}
		if err := r.Store.AckEvents(ctx, db.AckEventsParams{AckedThrough: ack.AckedThrough, Connection: r.Connection, RunID: runID}); err != nil {
			log.Error("spool not updated", "err", err)
			return
		}
		if ack.AckedThrough < rows[len(rows)-1].Seq {
			// The hub is missing something before this batch: what it lacks is
			// unacknowledged again and goes with the next tick.
			return
		}
	}
}

// deliver sends one result. It leaves the outbox only on an answer that
// settles it: accepted, a different terminal state the hub already holds, or a
// refusal no retry can change.
func (r *Reporter) deliver(ctx context.Context, o db.Outbox) {
	log := r.Log.With("connection", r.Connection, "run", o.RunID)
	var res v1.Result
	if err := json.Unmarshal([]byte(o.Body), &res); err != nil {
		log.Error("outbox entry unreadable; dropping it", "err", err)
		r.remove(ctx, o)
		return
	}
	err := r.Hub.Result(ctx, o.RunID, res)
	var se *hubclient.StatusError
	switch {
	case err == nil:
		log.Info("result delivered", "state", res.State)
	case errors.As(err, &se) && se.Status == http.StatusConflict && hubclient.Code(err) == v1.CodeConflict:
		// §2: the hub already holds a different terminal state, and the
		// hub's wins — decision 0023. Ours stays in the local record.
		log.Warn("the hub holds a different terminal state for this run; the hub's stands", "ours", res.State, "err", err)
	case final(err):
		log.Error("the hub refused the result; it will not be sent again", "state", res.State, "err", err)
	default:
		next := r.Now().Add(resultBackoff(int(o.Attempts) + 1))
		log.Warn("result not delivered; retrying", "attempt", o.Attempts+1, "next", next, "err", err)
		if err := r.Store.RetryOutbox(ctx, db.RetryOutboxParams{
			NextAttemptAt: next.UnixMilli(), LastError: sql.NullString{String: err.Error(), Valid: true},
			Connection: r.Connection, RunID: o.RunID,
		}); err != nil {
			log.Error("outbox not updated", "err", err)
		}
		return
	}
	r.remove(ctx, o)
}

func (r *Reporter) remove(ctx context.Context, o db.Outbox) {
	if err := r.Store.DeleteOutbox(ctx, db.DeleteOutboxParams{Connection: r.Connection, RunID: o.RunID}); err != nil {
		r.Log.Error("outbox not updated; the result will be sent again", "connection", r.Connection, "run", o.RunID, "err", err)
	}
}

// final is a hub answer no retry can change: the hub itself, in the
// protocol's envelope, said the run is not this runner's or the report is
// invalid. Everything else is retried — a 404 may be a wrong connection URL
// rather than an unknown run, a 401 a credential about to be replaced, and a
// bare 4xx a proxy in front of the hub. Giving up on any of those would lose
// a report the hub never saw.
func final(err error) bool {
	var se *hubclient.StatusError
	if !errors.As(err, &se) || se.Status < 400 || se.Status >= 500 {
		return false
	}
	switch hubclient.Code(err) {
	case v1.CodeNotHolder, v1.CodeInvalid:
		return true
	}
	return false
}

// resultBackoff doubles from firstResultRetry to maxResultInterval.
func resultBackoff(attempt int) time.Duration {
	d := firstResultRetry
	for i := 1; i < attempt && d < maxResultInterval; i++ {
		d *= 2
	}
	return min(d, maxResultInterval)
}
