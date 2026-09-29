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

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// cancelledBeforeStart is what a run cancelled before any runner started it
// says.
var cancelledBeforeStart = unstartedEnd{v1.RunCancelled, "cancelled on the hub before a runner started it"}

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
			"result's cancel_latency_ms says how long the harness took to stop. A claim the runner had not yet heard confirmed is " +
			"withdrawn instead, unstarted: it ends cancelled with no result, when the runner's next sync leaves it out or its lease " +
			"lapses. A harness that finished before the cancel " +
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
			"a harness that ignores it keeps going. A run that has not started is 409 — cancel it instead. So is a run held by a " +
			"runner that does not advertise the interrupt feature: it would ignore the control, and nothing acknowledges one.",
		Security: adminSecurity, Errors: []int{401, 404, 409},
	}, func(ctx context.Context, in *runInput) (*runOutput, error) {
		return h.control(ctx, in.Run, v1.ControlInterrupt, "")
	})

	huma.Register(api, huma.Operation{
		OperationID: "steerRun", Method: http.MethodPost, Path: "/runs/{run}/steer",
		Summary: "Add input to a running turn",
		Description: "Delivered once, at the runner's next sync. The harness reads it at its next tool boundary, or answers it " +
			"after the turn in the same run. A steer the harness would not take appears in the run's events as an error with " +
			"class steer_failed. A run that has not started is 409: put the text in the brief of a new run instead. So is a run " +
			"held by a runner that does not advertise the steer feature: it would ignore the control, and the text would be lost.",
		Security: adminSecurity, Errors: []int{400, 401, 404, 409},
	}, func(ctx context.Context, in *steerInput) (*runOutput, error) {
		return h.control(ctx, in.Run, v1.ControlSteer, in.Body.Text)
	})
}

// control records what the service API asked of a run and answers the run as
// it is now.
func (h *Hub) control(ctx context.Context, runID string, kind v1.ControlKind, text string) (*runOutput, error) {
	at := h.now()
	now := store.Ms(at)
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
			if err := endUnstarted(ctx, q, run.ID, cancelledBeforeStart, at); err != nil {
				return err
			}
		case run.State == "queued" || run.State == "offered":
			return Fail(http.StatusConflict, v1.CodeConflict,
				fmt.Sprintf("run %s has not started; there is no turn to %s", run.ID, kind),
				notStartedAction(kind, run.ID))
		case kind == v1.ControlCancel && state == v1.RunCancelled:
			// A retried cancel: the run is already what was asked.
		case state.IsTerminal():
			return Fail(http.StatusConflict, v1.CodeConflict,
				fmt.Sprintf("run %s has already ended %s", run.ID, run.State),
				"nothing to do: a finished run stays as it ended")
		default:
			if feature, alternative := controlFeature(kind, run.ID); feature != "" && run.RunnerID.Valid {
				holder, err := q.GetRunner(ctx, run.RunnerID.String)
				if err != nil {
					return err
				}
				if err := refuseUnadvertised(holder, run.Harness, kind, feature, alternative); err != nil {
					return err
				}
			}
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

func notStartedAction(kind v1.ControlKind, runID string) string {
	if kind == v1.ControlSteer {
		return "put the text in the brief of a new run, or steer once this one is running"
	}
	return "cancel it instead: `" + serviceCommand("cancel", runID) + "`, or POST /runs/{run}/cancel"
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
//
// doc is what this sync says the runner acts on, which the control was queued
// against but need no longer match — a runner restarted under an older binary
// keeps its credential. One it would now ignore stays queued rather than being
// spent on it: a steer held back can still reach the runner it was meant for,
// while a steer deleted here is gone and its caller was told it landed. The
// same holds when described is false and doc is known to be out of date.
func deliver(ctx context.Context, q *db.Queries, runID, harness string, doc v1.Capabilities, described bool) ([]v1.Control, error) {
	rows, err := q.ControlsFor(ctx, runID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	out := make([]v1.Control, 0, len(rows))
	var lastSteer int64
	for _, r := range rows {
		kind := v1.ControlKind(r.Kind)
		if feature, _ := controlFeature(kind, runID); feature != "" && !(described && capability.RunMayUse(doc, harness, feature)) {
			continue
		}
		out = append(out, v1.Control{Kind: kind, RunID: runID, Text: r.Text})
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
