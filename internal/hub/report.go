package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// maxBatch bounds one events call. A runner sends at most 100; the slack is
// for other runner implementations, the bound for a runner gone wrong.
const maxBatch = 1000

// reportBodyLimit is the largest events or result body the hub reads. A batch
// of 100 tool events at the 8 KiB cap on input and output is under 2 MiB; a
// final text can be long.
const reportBodyLimit = 16 << 20

// seqPage is how many stored sequence numbers one step of advancing
// acked_through reads.
const seqPage = 1000

// appendEvents stores a batch and answers how far the run's events are now
// contiguous. Only the runner holding the run may add to it — the one it was
// claimed by, before or after it finished — so a runner that lost the race for
// a run cannot write into its stream.
func (h *Hub) appendEvents(ctx context.Context, in *eventsInput) (*eventsOutput, error) {
	runner, err := h.caller(ctx)
	if err != nil {
		return nil, err
	}
	if len(in.Body.Events) > maxBatch {
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid,
			fmt.Sprintf("a batch carries at most %d events; this one has %d", maxBatch, len(in.Body.Events)),
			"send the events in smaller batches")
	}
	for i, ev := range in.Body.Events {
		if ev.Seq < 1 {
			return nil, Fail(http.StatusBadRequest, v1.CodeInvalid,
				fmt.Sprintf("events[%d].seq is %d; a run's events are numbered from 1", i, ev.Seq),
				"number each run's events 1, 2, 3, … in the order they happened")
		}
	}
	now := store.Ms(h.now())
	var through int64
	err = h.store.Tx(ctx, func(q *db.Queries) error {
		run, err := q.GetRun(ctx, in.Run)
		if err != nil {
			return missingRun(in.Run, err)
		}
		if !reporting(run, runner.ID, false) {
			return notHolder(in.Run)
		}
		for _, ev := range in.Body.Events {
			body, err := json.Marshal(ev)
			if err != nil {
				return err
			}
			// A resent event lands on the row it already has: (run, seq) is
			// the idempotency key, and the first copy stands.
			if _, err := q.AppendEvent(ctx, db.AppendEventParams{RunID: run.ID, Seq: ev.Seq, Body: string(body), ReceivedAt: now}); err != nil {
				return err
			}
		}
		through, err = advance(ctx, q, run)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &eventsOutput{Body: v1.EventAck{AckedThrough: through}}, nil
}

// advance moves the run's acked_through over every event now stored
// contiguously after it.
func advance(ctx context.Context, q *db.Queries, run db.Run) (int64, error) {
	through := run.EventsThrough
	for {
		seqs, err := q.EventSeqsFrom(ctx, db.EventSeqsFromParams{RunID: run.ID, After: through, Max: seqPage})
		if err != nil {
			return 0, err
		}
		gap := false
		for _, s := range seqs {
			if s != through+1 {
				gap = true
				break
			}
			through = s
		}
		if gap || len(seqs) < seqPage {
			break
		}
	}
	if through != run.EventsThrough {
		if err := q.SetEventsThrough(ctx, db.SetEventsThroughParams{EventsThrough: through, ID: run.ID}); err != nil {
			return 0, err
		}
	}
	return through, nil
}

// submitResult applies a run's terminal state at most once. The same result
// again is acknowledged; a different one is 409, and the state the hub already
// holds stands — including lost, which a result arriving after the lease
// lapsed does not overturn (decision 0023).
func (h *Hub) submitResult(ctx context.Context, in *resultInput) (*ackOutput, error) {
	runner, err := h.caller(ctx)
	if err != nil {
		return nil, err
	}
	res := in.Body
	if !res.State.IsTerminal() {
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid,
			fmt.Sprintf("state %q is not terminal", res.State),
			"report non-terminal states in the sync; a result carries succeeded, failed, cancelled, timed_out or lost")
	}
	body, err := json.Marshal(res)
	if err != nil {
		return nil, err
	}
	now := store.Ms(h.now())
	err = h.store.Tx(ctx, func(q *db.Queries) error {
		run, err := q.GetRun(ctx, in.Run)
		if err != nil {
			return missingRun(in.Run, err)
		}
		if !reporting(run, runner.ID, true) {
			return notHolder(in.Run)
		}
		held := v1.RunState(run.State)
		if prev, err := q.GetResult(ctx, run.ID); err == nil {
			held = v1.RunState(prev.State)
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		} else if !held.IsTerminal() {
			held = ""
		}
		switch {
		case held == res.State:
			// The same terminal state again: a retry whose first answer was
			// lost. Recorded once; the lease sweep may have got there first
			// with the same verdict, and then this is its report.
			_, err := q.PutResult(ctx, db.PutResultParams{RunID: run.ID, State: string(res.State), Body: string(body), ReceivedAt: now})
			return err
		case held != "":
			return Fail(http.StatusConflict, v1.CodeConflict,
				fmt.Sprintf("run %s is already %s on this hub; the result says %s", run.ID, held, res.State),
				"nothing to do: the hub's state stands, and the runner stops reporting this run")
		}
		if _, err := q.PutResult(ctx, db.PutResultParams{RunID: run.ID, State: string(res.State), Body: string(body), ReceivedAt: now}); err != nil {
			return err
		}
		var reason sql.NullString
		if res.Error != nil {
			reason = sql.NullString{String: res.Error.Message, Valid: true}
		}
		return q.FinishRun(ctx, db.FinishRunParams{State: string(res.State), Reason: reason, UpdatedAt: now, ID: run.ID})
	})
	if err != nil {
		return nil, err
	}
	return &ackOutput{Body: v1.Ack{OK: true}}, nil
}

// reporting is whether a runner may report on a run: it is the runner the run
// was claimed by. An offered run takes a result — the refusal of decision
// 0019 — but no events, because a run starts only once its claim is
// acknowledged. After the run ends its holder stays its holder, so a late
// batch or a retried result still lands; a runner the run was never bound to
// never does.
func reporting(r db.Run, runnerID string, result bool) bool {
	if !r.RunnerID.Valid || r.RunnerID.String != runnerID {
		return false
	}
	switch r.State {
	case "queued":
		return false
	case "offered":
		return result
	}
	return true
}

func missingRun(id string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return Fail(http.StatusNotFound, v1.CodeNotFound, "this hub has no run "+id,
			"check the connection URL points at the hub that issued the run; a runner keeps retrying, since a wrong URL answers 404 too")
	}
	return err
}

func notHolder(id string) error {
	return Fail(http.StatusForbidden, v1.CodeNotHolder,
		"run "+id+" is not held by this runner — it was offered elsewhere, or never to this runner",
		"stop the run and stop reporting it; nothing this runner sends for it is applied")
}

// caller is the runner the request's credential belongs to. Unlike sync, the
// run calls name no runner in the path: the credential alone says who it is.
func (h *Hub) caller(ctx context.Context) (db.Runner, error) {
	cred := bearer(ctx)
	if cred == "" {
		return db.Runner{}, Fail(http.StatusUnauthorized, v1.CodeUnauthorized, "no runner credential", "run `yad connect` to register this runner")
	}
	r, err := h.store.GetRunnerByCredential(ctx, hashSecret(cred))
	if errors.Is(err, sql.ErrNoRows) {
		return db.Runner{}, Fail(http.StatusUnauthorized, v1.CodeUnauthorized,
			"the hub does not know this runner credential — a newer registration replaced it, or the hub's database was reset",
			newTokenAction)
	}
	return r, err
}
