package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// unstartedReason is what a run cancelled before any runner started it says.
const unstartedReason = "cancelled on the hub before a runner started it"

type (
	runInput struct {
		Run string `path:"run" doc:"The run id."`
	}
	steerInput struct {
		Run  string `path:"run" doc:"The run id."`
		Body hubapi.SteerRequest
	}
)

func (h *Hub) registerControls(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "cancelRun", Method: http.MethodPost, Path: "/runs/{run}/cancel",
		Summary: "Cancel a run",
		Description: "A run no runner has started — queued, or offered and not yet claimed — is cancelled here and now. " +
			"A run a runner holds is cancelled by that runner at its next sync, down the cancel ladder: the harness is " +
			"interrupted, then its process group gets SIGTERM, then SIGKILL; cancel_requested_at is set until then, and the " +
			"result's cancel_latency_ms says how long the harness took to stop. A harness that finished before the cancel " +
			"reached it keeps its own result. Cancelling a cancelled run answers it as it is; any other finished run is 409.",
		Security: adminSecurity, Errors: []int{401, 404, 409},
	}, func(ctx context.Context, in *runInput) (*runOutput, error) {
		return h.control(ctx, in.Run, v1.ControlCancel, "")
	})

	huma.Register(api, huma.Operation{
		OperationID: "interruptRun", Method: http.MethodPost, Path: "/runs/{run}/interrupt",
		Summary: "Interrupt a run's turn",
		Description: "Ends the harness's current turn and keeps its session, so the next run in the session resumes it; the run " +
			"ends cancelled unless the turn finished first. Delivered at the runner's next sync. Unlike a cancel it asks only: " +
			"a harness that ignores it keeps going. A run that has not started is 409 — cancel it instead.",
		Security: adminSecurity, Errors: []int{401, 404, 409},
	}, func(ctx context.Context, in *runInput) (*runOutput, error) {
		return h.control(ctx, in.Run, v1.ControlInterrupt, "")
	})

	huma.Register(api, huma.Operation{
		OperationID: "steerRun", Method: http.MethodPost, Path: "/runs/{run}/steer",
		Summary: "Add input to a running turn",
		Description: "Delivered once, at the runner's next sync. The harness reads it at its next tool boundary, or answers it " +
			"after the turn in the same run. A steer the harness would not take appears in the run's events as an error with " +
			"class steer_failed. A run that has not started is 409: put the text in the brief of a new run instead.",
		Security: adminSecurity, Errors: []int{400, 401, 404, 409},
	}, func(ctx context.Context, in *steerInput) (*runOutput, error) {
		return h.control(ctx, in.Run, v1.ControlSteer, in.Body.Text)
	})
}

// control records what the service API asked of a run and answers the run as
// it is now.
func (h *Hub) control(ctx context.Context, runID string, kind v1.ControlKind, text string) (*runOutput, error) {
	now := store.Ms(h.now())
	var view hubapi.Run
	err := h.store.Tx(ctx, func(q *db.Queries) error {
		run, err := q.GetRun(ctx, runID)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = runView(ctx, q, runID) // the service API's own 404
		}
		if err != nil {
			return err
		}
		switch state := v1.RunState(run.State); {
		case kind == v1.ControlCancel && (run.State == "queued" || run.State == "offered"):
			if _, err := q.CancelUnstartedRun(ctx, db.CancelUnstartedRunParams{
				Reason: sql.NullString{String: unstartedReason, Valid: true}, UpdatedAt: now, ID: run.ID,
			}); err != nil {
				return err
			}
		case run.State == "queued" || run.State == "offered":
			return Fail(http.StatusConflict, v1.CodeConflict,
				fmt.Sprintf("run %s has not started; there is no turn to %s", run.ID, kind),
				notStartedAction(kind))
		case kind == v1.ControlCancel && state == v1.RunCancelled:
			// A retried cancel: the run is already what was asked.
		case state.IsTerminal():
			return Fail(http.StatusConflict, v1.CodeConflict,
				fmt.Sprintf("run %s has already ended %s", run.ID, run.State),
				"nothing to do: a finished run stays as it ended")
		default:
			if err := queue(ctx, q, run.ID, kind, text, now); err != nil {
				return err
			}
		}
		view, err = runView(ctx, q, runID)
		return err
	})
	if err != nil {
		return nil, err
	}
	h.bell.ring()
	return &runOutput{Body: view}, nil
}

func notStartedAction(kind v1.ControlKind) string {
	if kind == v1.ControlSteer {
		return "put the text in the brief of a new run, or steer once this one is running"
	}
	return "cancel it instead: `yad hub cancel`, or POST /runs/{run}/cancel"
}

// queue adds a control for the runner holding the run. A cancel or an
// interrupt already queued is not queued twice: both are repeated on every
// sync anyway.
func queue(ctx context.Context, q *db.Queries, runID string, kind v1.ControlKind, text string, now int64) error {
	if kind != v1.ControlSteer {
		_, err := q.FirstControl(ctx, db.FirstControlParams{RunID: runID, Kind: string(kind)})
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	return q.AddControl(ctx, db.AddControlParams{RunID: runID, Kind: string(kind), Text: text, CreatedAt: now})
}

// deliver is the controls a sync answers for one run its runner listed. A
// cancel or an interrupt goes out on every sync until the run ends, because
// nothing acknowledges a control and a lost response must not lose one; the
// runner acts on the first. A steer goes out once and is gone: sent twice, the
// harness would read it twice.
func deliver(ctx context.Context, q *db.Queries, runID string) ([]v1.Control, error) {
	rows, err := q.ControlsFor(ctx, runID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]v1.Control, 0, len(rows))
	var lastSteer int64
	for _, r := range rows {
		out = append(out, v1.Control{Kind: v1.ControlKind(r.Kind), RunID: runID, Text: r.Text})
		if r.Kind == string(v1.ControlSteer) {
			lastSteer = r.ID
		}
	}
	if lastSteer > 0 {
		if err := q.DeleteSteersThrough(ctx, db.DeleteSteersThroughParams{RunID: runID, ID: lastSteer}); err != nil {
			return nil, err
		}
	}
	return out, nil
}
