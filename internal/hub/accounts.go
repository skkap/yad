package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// A hub adding and removing a runner's accounts (decision 0057), as yad hub
// does it. An add is a login carrying add (logins.go). A removal is a row
// here, and the remove_account control each answer to the runner's syncs
// carries until its health leaves the account out.

type (
	accountInput struct {
		Runner  string `path:"runner" doc:"The runner id."`
		Harness string `path:"harness" doc:"The harness."`
		Account string `path:"account" doc:"The account label."`
	}
	accountOutput struct{ Body hubapi.Account }
)

func (h *Hub) registerAccounts(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "removeAccount", Method: http.MethodPost, Path: "/runners/{runner}/accounts/{harness}/{account}/remove",
		Summary: "Remove an account from a runner",
		Description: "The runner hears it at its next sync and removes the account as its owner's yad account remove does: listed " +
			"nowhere from then on, runs already on it finish there, and its home — the login — is deleted once the last of them " +
			"has ended. A login in flight on it ends cancelled. The request stands until the runner's health leaves the account " +
			"out, or the runner stops advertising the accounts feature; a repeat is the same request. A runner that does not " +
			"advertise accounts to this hub — its owner has turned it off for it, or it runs an older yad — is 409.",
		Security: adminSecurity, Errors: []int{400, 401, 404, 409},
	}, func(ctx context.Context, in *accountInput) (*accountOutput, error) {
		if err := checkAccountName(in.Harness, in.Account); err != nil {
			return nil, err
		}
		var view hubapi.Account
		err := h.store.Tx(ctx, func(q *db.Queries) error {
			r, err := q.GetRunner(ctx, in.Runner)
			if errors.Is(err, sql.ErrNoRows) {
				return noRunner(in.Runner)
			}
			if err != nil {
				return err
			}
			if err := refuseNoAccounts(r, "remove_account", "at the machine, `"+runnerCommand("account", "remove", in.Harness, in.Account)+"` removes it"); err != nil {
				return err
			}
			if err := q.RequestAccountRemoval(ctx, db.RequestAccountRemovalParams{
				RunnerID: r.ID, Harness: in.Harness, Account: in.Account, RequestedAt: store.Ms(h.now()),
			}); err != nil {
				return err
			}
			view, err = accountOf(ctx, q, r, in.Harness, in.Account)
			return err
		})
		if err != nil {
			return nil, err
		}
		return &accountOutput{Body: view}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "getAccount", Method: http.MethodGet, Path: "/runners/{runner}/accounts/{harness}/{account}",
		Summary:  "Read an account as the runner's last health has it, and a removal asked for",
		Security: adminSecurity, Errors: []int{400, 401, 404},
	}, func(ctx context.Context, in *accountInput) (*accountOutput, error) {
		if err := checkAccountName(in.Harness, in.Account); err != nil {
			return nil, err
		}
		var view hubapi.Account
		err := h.store.Tx(ctx, func(q *db.Queries) error {
			r, err := q.GetRunner(ctx, in.Runner)
			if errors.Is(err, sql.ErrNoRows) {
				return noRunner(in.Runner)
			}
			if err != nil {
				return err
			}
			view, err = accountOf(ctx, q, r, in.Harness, in.Account)
			return err
		})
		if err != nil {
			return nil, err
		}
		return &accountOutput{Body: view}, nil
	})
}

func checkAccountName(harness, label string) error {
	if config.ValidName(harness) != nil || config.ValidName(label) != nil {
		return Fail(http.StatusBadRequest, v1.CodeInvalid,
			fmt.Sprintf("%q account %q is not a harness and an account label: each is lowercase letters, digits, dashes and underscores", harness, label),
			"name the account as `"+runnerCommand("account", "list")+"` shows it on the runner's machine")
	}
	return nil
}

// refuseNoAccounts is the answer to an add or a removal for a runner that
// does not advertise accounts to this hub, which would ignore either.
func refuseNoAccounts(r db.Runner, what, alternative string) error {
	doc, err := storedDoc(r)
	if err != nil {
		return err
	}
	if advertises(doc, capability.FeatureAccounts) {
		return nil
	}
	return Fail(http.StatusConflict, v1.CodeConflict,
		fmt.Sprintf("runner %s does not advertise the %q feature to this hub, so it would ignore %s: its owner has turned adding and removing accounts off for this hub (manage_accounts = false), or it runs an older yad", r.ID, capability.FeatureAccounts, what),
		alternative)
}

// addAtTheMachine is how the owner adds the account at the runner's machine
// instead.
func addAtTheMachine(harness, label string) string {
	if config.ValidName(label) != nil {
		label = "<label>"
	}
	return "at the machine, `" + runnerCommand("account", "add", harness, label) + "` adds it"
}

// accountOf is an account as the runner's last health has it.
func accountOf(ctx context.Context, q *db.Queries, r db.Runner, harness, label string) (hubapi.Account, error) {
	view := hubapi.Account{RunnerID: r.ID, Harness: harness, Account: label}
	if r.Health.Valid {
		var h v1.Health
		if err := json.Unmarshal([]byte(r.Health.String), &h); err != nil {
			return view, fmt.Errorf("stored health for %s: %w", r.ID, err)
		}
		if a, ok := healthAccount(h, harness, label); ok {
			view.Listed, view.State = true, a.State
		}
	}
	rm, err := q.GetAccountRemoval(ctx, db.GetAccountRemovalParams{RunnerID: r.ID, Harness: harness, Account: label})
	switch {
	case err == nil:
		t := time.UnixMilli(rm.RequestedAt).UTC()
		view.RemoveRequestedAt = &t
	case !errors.Is(err, sql.ErrNoRows):
		return view, err
	}
	return view, nil
}

func healthAccount(h v1.Health, harness, label string) (v1.AccountReport, bool) {
	for _, hh := range h.Harnesses {
		if hh.ID != harness {
			continue
		}
		for _, a := range hh.Accounts {
			if a.Label == label {
				return a, true
			}
		}
	}
	return v1.AccountReport{}, false
}

// goneFrom is whether a sync's health leaves the account out. Only a health
// that names the harness can say so: one that names none is a runner that
// could not read its accounts this time (or runs no harness it can drive),
// and says nothing about this one.
func goneFrom(h v1.Health, harness, label string) bool {
	for _, hh := range h.Harnesses {
		if hh.ID == harness {
			_, listed := healthAccount(h, harness, label)
			return !listed
		}
	}
	return false
}

// syncRemovals answers the removals asked of this runner: remove_account for
// each until this sync's health leaves the account out. One for a runner whose
// document has arrived and does not advertise accounts ends, since that runner
// will not act on it (decision 0057); one whose document has not arrived is
// held back, as every gated control is.
func syncRemovals(ctx context.Context, q *db.Queries, runnerID string, health v1.Health, doc v1.Capabilities, described bool) ([]v1.Control, error) {
	pending, err := q.AccountRemovals(ctx, runnerID)
	if err != nil {
		return nil, err
	}
	var out []v1.Control
	for _, rm := range pending {
		switch {
		case goneFrom(health, rm.Harness, rm.Account), described && !advertises(doc, capability.FeatureAccounts):
			if err := q.EndAccountRemoval(ctx, db.EndAccountRemovalParams{RunnerID: runnerID, Harness: rm.Harness, Account: rm.Account}); err != nil {
				return nil, err
			}
		case described:
			out = append(out, v1.Control{Kind: v1.ControlRemoveAccount, Harness: rm.Harness, Account: rm.Account})
		}
	}
	return out, nil
}
