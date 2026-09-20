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

// closedUnstartedReason is what a run that never started says when its
// session was closed before any runner held it.
const closedUnstartedReason = "its session was closed before a runner started it"

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
				if err := closeUnbound(ctx, q, sess.ID, now); err != nil {
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

// closeUnbound closes a session no runner has claimed a run of: nothing is
// on any runner's disk, so the hub alone decides, and the runs waiting in it
// are cancelled rather than left to start a session that is closed.
func closeUnbound(ctx context.Context, q *db.Queries, id string, now time.Time) error {
	if _, err := q.CloseUnboundSession(ctx, db.CloseUnboundSessionParams{Now: sql.NullInt64{Int64: store.Ms(now), Valid: true}, ID: id}); err != nil {
		return err
	}
	runs, err := q.UnstartedRunsInSession(ctx, id)
	if err != nil {
		return err
	}
	for _, r := range runs {
		// An offered run keeps its runner, which hears cancel at its next
		// sync and withdraws it.
		if _, err := q.CancelUnstartedRun(ctx, db.CancelUnstartedRunParams{
			Reason: sql.NullString{String: closedUnstartedReason, Valid: true}, UpdatedAt: store.Ms(now), ID: r,
		}); err != nil {
			return err
		}
	}
	return nil
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
