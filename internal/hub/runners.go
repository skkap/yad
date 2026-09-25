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

	"github.com/skkap/yad/internal/hub/store/db"
)

type runnerListOutput struct{ Body hubapi.RunnerList }

// registerRunners is the read side of the fleet: what every runner said about
// itself at its last sync. Without it a hub receives health on every sync and
// has no way to show it, which is the question an operator asks first — why is
// this runner slow, or idle.
func (h *Hub) registerRunners(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "listRunners", Method: http.MethodGet, Path: "/runners",
		Summary: "Every runner this hub knows, with its last health",
		Description: "The ones still syncing come first. Health is the runner's own, as it sent it, and is as old as its last sync: " +
			"a machine that has gone away still shows the health of its last word. A runner that registered and never synced has none.",
		Security: adminSecurity, Errors: []int{401},
	}, func(ctx context.Context, _ *struct{}) (*runnerListOutput, error) {
		out := runnerListOutput{Body: hubapi.RunnerList{Runners: []hubapi.Runner{}}}
		err := h.store.Tx(ctx, func(q *db.Queries) error {
			rows, err := q.ListRunners(ctx)
			if err != nil {
				return err
			}
			for _, r := range rows {
				view, err := runnerView(r)
				if err != nil {
					return err
				}
				out.Body.Runners = append(out.Body.Runners, view)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		return &out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getRunner", Method: http.MethodGet, Path: "/runners/{runner}",
		Summary:     "One runner, with its last health",
		Description: "The same view as the list, for a runner whose id is already known.",
		Security:    adminSecurity, Errors: []int{401, 404},
	}, func(ctx context.Context, in *runnerInput) (*runnerOutput, error) {
		var view hubapi.Runner
		err := h.store.Tx(ctx, func(q *db.Queries) error {
			r, err := q.GetRunner(ctx, in.Runner)
			if errors.Is(err, sql.ErrNoRows) {
				return Fail(http.StatusNotFound, v1.CodeNotFound, fmt.Sprintf("this hub has no runner %q", in.Runner),
					noRunnerAction())
			}
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
