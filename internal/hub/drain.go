package hub

import (
	"context"
	"database/sql"
	"encoding/json"
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

type (
	runnerInput struct {
		Runner string `path:"runner" doc:"The runner id, as its daemon prints it at start."`
	}
	runnerOutput struct{ Body hubapi.Runner }
)

func (h *Hub) registerDrain(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "drainRunner", Method: http.MethodPost, Path: "/runners/{runner}/drain",
		Summary: "Drain a runner",
		Description: "The runner hears it at its next sync: it stops taking runs, lets the ones it holds finish — up to its " +
			"owner's drain wait, after which it cancels them — and exits. It keeps syncing until then, so leases renew and " +
			"results land, and this hub offers it nothing. The request stands until a sync says the runner is draining. " +
			"A runner that does not advertise the drain feature is 409: it would ignore the control.",
		Security: adminSecurity, Errors: []int{401, 404, 409},
	}, func(ctx context.Context, in *runnerInput) (*runnerOutput, error) {
		var view hubapi.Runner
		err := h.store.Tx(ctx, func(q *db.Queries) error {
			r, err := q.GetRunner(ctx, in.Runner)
			if errors.Is(err, sql.ErrNoRows) {
				return Fail(http.StatusNotFound, v1.CodeNotFound, fmt.Sprintf("this hub has no runner %q", in.Runner),
					"check the runner id — `yad daemon start` prints it on the runner's machine")
			}
			if err != nil {
				return err
			}
			view, err = runnerView(r)
			if err != nil || view.Draining {
				return err
			}
			var doc v1.Capabilities
			if err := json.Unmarshal([]byte(r.Capabilities), &doc); err != nil {
				return fmt.Errorf("stored capability document for %s: %w", r.ID, err)
			}
			if !slices.Contains(doc.ProtocolFeatures, capability.FeatureDrain) {
				return Fail(http.StatusConflict, v1.CodeConflict,
					fmt.Sprintf("runner %s does not advertise the %q feature; it would ignore the control", r.ID, capability.FeatureDrain),
					"upgrade yad on that runner, or stop it on its machine with SIGTERM")
			}
			if err := q.RequestDrain(ctx, db.RequestDrainParams{Now: sql.NullInt64{Int64: store.Ms(h.now()), Valid: true}, ID: r.ID}); err != nil {
				return err
			}
			r, err = q.GetRunner(ctx, r.ID)
			if err != nil {
				return err
			}
			view, err = runnerView(r)
			return err
		})
		if err != nil {
			return nil, err
		}
		return &runnerOutput{Body: view}, nil
	})
}

// runnerView is a runner as the service API shows it. Draining comes from the
// runner's own last word, its health, not from the request.
func runnerView(r db.Runner) (hubapi.Runner, error) {
	view := hubapi.Runner{RunnerID: r.ID, Name: r.Name}
	if r.LastSyncAt.Valid {
		t := time.UnixMilli(r.LastSyncAt.Int64).UTC()
		view.LastSyncAt = &t
	}
	if r.DrainRequestedAt.Valid {
		t := time.UnixMilli(r.DrainRequestedAt.Int64).UTC()
		view.DrainRequestedAt = &t
	}
	if r.Health.Valid {
		var h v1.Health
		if err := json.Unmarshal([]byte(r.Health.String), &h); err != nil {
			return view, fmt.Errorf("stored health for %s: %w", r.ID, err)
		}
		view.Draining = h.Draining
	}
	return view, nil
}
