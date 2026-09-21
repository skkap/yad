package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// unstartedEnd is what becomes of a run still waiting in a session the hub
// closes: the state it ends in, and the reason its submitter reads.
type unstartedEnd struct {
	state  v1.RunState
	reason string
}

var (
	// A close someone asked for: the runs waiting in the session were
	// abandoned with it, which is a cancel.
	closedBeforeStart = unstartedEnd{v1.RunCancelled, "its session was closed before a runner started it"}
	// The session's runner deregistered, and the session's transcript and
	// workdir went with it: the run cannot happen as submitted, and the
	// submitter's work is not lost — it needs a new session to go to.
	runnerDeregistered = unstartedEnd{v1.RunFailed, "its session's runner was deregistered; submit the work to a new session"}
)

type (
	sessionInput struct {
		Session string `path:"session" doc:"The session id."`
	}
	sessionOutput struct{ Body hubapi.Session }
)

func (h *Hub) registerSessions(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "getSession", Method: http.MethodGet, Path: "/sessions/{session}",
		Summary:  "Read a session: its runner, and whether it is open, closing or closed",
		Security: adminSecurity, Errors: []int{401, 404},
	}, func(ctx context.Context, in *sessionInput) (*sessionOutput, error) {
		var view hubapi.Session
		err := h.store.Tx(ctx, func(q *db.Queries) (err error) {
			view, err = sessionView(ctx, q, in.Session)
			return err
		})
		if err != nil {
			return nil, err
		}
		return &sessionOutput{Body: view}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "closeSession", Method: http.MethodPost, Path: "/sessions/{session}/close",
		Summary: "Close a session",
		Description: "The session takes no new run, and its runner deletes its workdir. A session no runner holds yet closes " +
			"here and now, and its runs that have not started are cancelled. One a runner holds is closing until that runner " +
			"says it is done, which it does at a sync once no run of the session is held there — a run in it finishes first. " +
			"Closing a closed session answers it as it is. A runner that does not advertise the close_session feature is 409: " +
			"it would ignore the control.",
		Security: adminSecurity, Errors: []int{401, 404, 409},
	}, func(ctx context.Context, in *sessionInput) (*sessionOutput, error) {
		now := h.now()
		var view hubapi.Session
		err := h.store.Tx(ctx, func(q *db.Queries) error {
			sess, err := q.GetSession(ctx, in.Session)
			if errors.Is(err, sql.ErrNoRows) {
				_, err = sessionView(ctx, q, in.Session) // the 404
			}
			if err != nil {
				return err
			}
			switch {
			case sess.ClosedAt.Valid:
			case !sess.RunnerID.Valid:
				// No runner has claimed a run of it, so nothing is on any
				// runner's disk and no runner's report is owed.
				if err := closeHere(ctx, q, sess.ID, v1.SessionClosed, closedBeforeStart, now); err != nil {
					return err
				}
			default:
				r, err := q.GetRunner(ctx, sess.RunnerID.String)
				if err != nil {
					return err
				}
				doc, err := storedDoc(r)
				if err != nil {
					return err
				}
				if !slices.Contains(doc.ProtocolFeatures, capability.FeatureCloseSession) {
					return Fail(http.StatusConflict, v1.CodeConflict,
						fmt.Sprintf("session %s is on runner %s, which does not advertise the %q feature; it would ignore the control", sess.ID, r.ID, capability.FeatureCloseSession),
						"upgrade yad on that runner — until then its workdir stays on its disk; a run naming a new session starts fresh")
				}
				if err := q.RequestSessionClose(ctx, db.RequestSessionCloseParams{Now: sql.NullInt64{Int64: store.Ms(now), Valid: true}, ID: sess.ID}); err != nil {
					return err
				}
			}
			view, err = sessionView(ctx, q, sess.ID)
			return err
		})
		if err != nil {
			return nil, err
		}
		h.bell.ring()
		return &sessionOutput{Body: view}, nil
	})
}

// closeHere closes a session by the hub's own act (decision 0011), when no
// runner will report the close: one no runner ever claimed a run of, or one
// whose runner has gone. The runs still waiting in it end with it rather than
// being left queued in a session no runner will take another run in — a
// queued run holds no lease, so nothing else would ever end it.
//
// The binding is never cleared instead. A session is resumable only on the
// runner that holds it (DOMAIN.md), so offering one to another runner would be
// offering a resume that cannot work.
func closeHere(ctx context.Context, q *db.Queries, id string, reason v1.SessionCloseReason, end unstartedEnd, now time.Time) error {
	if _, err := q.CloseSessionHere(ctx, db.CloseSessionHereParams{
		Now: sql.NullInt64{Int64: store.Ms(now), Valid: true}, Reason: sql.NullString{String: string(reason), Valid: true}, ID: id,
	}); err != nil {
		return err
	}
	runs, err := q.UnstartedRunsInSession(ctx, id)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if err := endUnstarted(ctx, q, r, end, now); err != nil {
			return err
		}
	}
	return nil
}

// endUnstarted ends a run no runner has started, on the hub alone. An offered
// run keeps its runner, which hears cancel at its next sync and withdraws it.
func endUnstarted(ctx context.Context, q *db.Queries, runID string, end unstartedEnd, now time.Time) error {
	_, err := q.EndUnstartedRun(ctx, db.EndUnstartedRunParams{
		State: string(end.state), Reason: sql.NullString{String: end.reason, Valid: true}, Now: store.Ms(now), ID: runID,
	})
	return err
}

func sessionView(ctx context.Context, q *db.Queries, id string) (hubapi.Session, error) {
	s, err := q.GetSession(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return hubapi.Session{}, Fail(http.StatusNotFound, v1.CodeNotFound, fmt.Sprintf("this hub has no session %q", id),
			"check the id against a run's session_id")
	}
	if err != nil {
		return hubapi.Session{}, err
	}
	view := hubapi.Session{
		SessionID: s.ID, Harness: s.Harness, RunnerID: s.RunnerID.String, State: hubapi.SessionOpen,
		CloseReason: s.CloseReason.String, CreatedAt: time.UnixMilli(s.CreatedAt).UTC(),
	}
	if s.CloseRequestedAt.Valid {
		t := time.UnixMilli(s.CloseRequestedAt.Int64).UTC()
		view.CloseRequestedAt, view.State = &t, hubapi.SessionClosing
	}
	if s.ClosedAt.Valid {
		t := time.UnixMilli(s.ClosedAt.Int64).UTC()
		view.ClosedAt, view.State = &t, hubapi.SessionClosed
	}
	return view, nil
}
