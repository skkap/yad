package hub

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// Hub logins (decision 0055), as yad hub keeps them: a row per login, the
// controls that carry it to the runner, and the runner's reports that move
// it. The code and the token are stored only until the runner has them — a
// trigger in the schema blanks each — and no answer of this API carries
// either.

const (
	// loginDeliverWithin is how long a login may wait for its runner's next
	// sync. A runner syncs every few seconds; one that has not in this long
	// is not running, and a token must not sit here until it is.
	loginDeliverWithin = 10 * time.Minute
	// loginFinishWithin is how long a login a runner took may go without an
	// end: past every deadline a runner holds one to (ten minutes for the
	// code, a minute for the rest), with room for syncs.
	loginFinishWithin = 30 * time.Minute
	// maxLoginReport bounds what the hub keeps of a runner's url and error,
	// both the runner's to choose.
	maxLoginURL   = 4096
	maxLoginError = 2048
)

type (
	startLoginInput struct {
		Runner string `path:"runner" doc:"The runner id."`
		Body   hubapi.LoginRequest
	}
	loginInput struct {
		Runner string `path:"runner" doc:"The runner id."`
		Login  string `path:"login" doc:"The login id."`
	}
	loginCodeInput struct {
		Runner string `path:"runner" doc:"The runner id."`
		Login  string `path:"login" doc:"The login id."`
		Body   hubapi.LoginCodeRequest
	}
	loginOutput struct{ Body hubapi.Login }
)

func (h *Hub) registerLogins(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "startLogin", Method: http.MethodPost, Path: "/runners/{runner}/logins",
		Summary: "Log an account in on a runner, by link or by token",
		Description: "Without a token, the runner runs the harness's own login and reports the link to sign in at: poll the login " +
			"until it is waiting, show url, and send the code the provider gives with POST .../code. With a token, the runner stores " +
			"it as the account's login; the token is blanked here as soon as the runner reports the login. Either way the login " +
			"ends succeeded only when the harness's own check on the machine says so. The account must be one the runner lists, " +
			"or with add a new one, which the runner lists once the login takes; without one, the harness's own default login is " +
			"logged in, by link only. A newer login for the same account replaces an older one. A runner that does not advertise " +
			"the login feature is 409, and an add to one that does not advertise accounts.",
		DefaultStatus: http.StatusCreated,
		Security:      adminSecurity, Errors: []int{400, 401, 404, 409},
	}, h.startLogin)

	huma.Register(api, huma.Operation{
		OperationID: "getLogin", Method: http.MethodGet, Path: "/runners/{runner}/logins/{login}",
		Summary:  "Read a login's state, and its link while it waits for a code",
		Security: adminSecurity, Errors: []int{401, 404},
	}, func(ctx context.Context, in *loginInput) (*loginOutput, error) {
		var view hubapi.Login
		err := h.store.Tx(ctx, func(q *db.Queries) error {
			l, err := loginOf(ctx, q, in.Runner, in.Login)
			if err != nil {
				return err
			}
			view = loginView(l)
			return nil
		})
		if err != nil {
			return nil, err
		}
		return &loginOutput{Body: view}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "sendLoginCode", Method: http.MethodPost, Path: "/runners/{runner}/logins/{login}/code",
		Summary: "Send the code the owner got after signing in",
		Description: "Delivered at the runner's next sync, which writes it to the harness's login; the login then moves to checking " +
			"and ends by the harness's own check. Only a login that is waiting takes a code: before its link is out, or once it " +
			"has ended, it is 409. A second code before the runner has taken the first replaces it.",
		Security: adminSecurity, Errors: []int{400, 401, 404, 409},
	}, h.sendLoginCode)

	huma.Register(api, huma.Operation{
		OperationID: "cancelLogin", Method: http.MethodPost, Path: "/runners/{runner}/logins/{login}/cancel",
		Summary: "Cancel a login",
		Description: "The runner ends it at its next sync, stopping the harness's login, and reports it cancelled; " +
			"cancel_requested_at is set until then. A login that has ended is answered as it is.",
		Security: adminSecurity, Errors: []int{401, 404},
	}, h.cancelLogin)
}

func (h *Hub) startLogin(ctx context.Context, in *startLoginInput) (*loginOutput, error) {
	req := in.Body
	method := v1.LoginByLink
	if req.Token != "" {
		method = v1.LoginByToken
	}
	id := req.LoginID
	if id == "" {
		id = newID("lgn_")
	}
	switch {
	case !runnerIDPattern.MatchString(id):
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid,
			"login_id must be 1-128 letters, digits, dots, dashes or underscores, starting with a letter or digit",
			"pick another id, or leave it out and the hub generates one")
	case req.Account != "" && config.ValidName(req.Account) != nil:
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid,
			fmt.Sprintf("account %q is not an account label: labels are lowercase letters, digits, dashes and underscores", req.Account),
			"name the account as `"+runnerCommand("account", "list")+"` shows it on the runner's machine")
	case req.Add && req.Account == "":
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid,
			"add creates an account, and this request names none",
			"name the new account's label in account: lowercase letters, digits, dashes and underscores")
	case method == v1.LoginByToken && req.Account == "":
		// A token is always an account's (decision 0054); the runner would
		// refuse it, and the token would have been stored here for nothing.
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid,
			"a token logs in an account, and this request names none",
			"name an account the runner's owner listed, or log the harness's own default login in by link, without a token")
	case method == v1.LoginByToken && account.CheckToken(req.Token) != nil:
		// The check's own words describe the paste, never repeat it.
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid, account.CheckToken(req.Token).Error(),
			"paste the token `claude setup-token` printed, on one line")
	}
	now := h.now()
	var view hubapi.Login
	err := h.store.Tx(ctx, func(q *db.Queries) error {
		r, err := q.GetRunner(ctx, in.Runner)
		if errors.Is(err, sql.ErrNoRows) {
			return noRunner(in.Runner)
		}
		if err != nil {
			return err
		}
		if err := refuseUnadvertised(r, v1.ControlStartLogin, capability.FeatureLogin, loginAtTheMachine(req.Harness, req.Account)); err != nil {
			return err
		}
		if req.Add {
			if err := refuseNoAccounts(r, "an add", addAtTheMachine(req.Harness, req.Account)); err != nil {
				return err
			}
			// Adding it again is the owner's newer word. A removal still
			// waiting would otherwise go out as soon as the account is back
			// in the runner's reports, and take away what was just added.
			if err := q.EndAccountRemoval(ctx, db.EndAccountRemovalParams{RunnerID: r.ID, Harness: req.Harness, Account: req.Account}); err != nil {
				return err
			}
		}
		if _, err := q.GetLogin(ctx, id); err == nil {
			return Fail(http.StatusConflict, v1.CodeConflict, fmt.Sprintf("the hub already has login %q", id),
				"use a new login_id, or leave it out and the hub generates one")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		// One login per account (decision 0055). One no answer has carried
		// yet is ended here, and never sent. One that went out may already
		// be the runner's — a token stored, a login taken — so it gets a
		// cancel_login and stays open until the runner says how it ended,
		// which it does whether or not it had it.
		older, err := q.OpenLoginsFor(ctx, db.OpenLoginsForParams{RunnerID: r.ID, Harness: req.Harness, Account: req.Account})
		if err != nil {
			return err
		}
		for _, o := range older {
			if err := h.endOrCancel(ctx, q, o, fmt.Sprintf("login %s for the same account replaced it before it reached the runner", id), now); err != nil {
				return err
			}
		}
		if err := q.CreateLogin(ctx, db.CreateLoginParams{
			ID: id, RunnerID: r.ID, Harness: req.Harness, Account: req.Account, Method: string(method),
			Token: req.Token, AddAccount: boolInt(req.Add), CreatedAt: store.Ms(now), UpdatedAt: store.Ms(now),
		}); err != nil {
			return err
		}
		l, err := q.GetLogin(ctx, id)
		if err != nil {
			return err
		}
		view = loginView(l)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &loginOutput{Body: view}, nil
}

func (h *Hub) sendLoginCode(ctx context.Context, in *loginCodeInput) (*loginOutput, error) {
	var view hubapi.Login
	err := h.store.Tx(ctx, func(q *db.Queries) error {
		l, err := loginOf(ctx, q, in.Runner, in.Login)
		if err != nil {
			return err
		}
		switch {
		case l.Method != string(v1.LoginByLink):
			return Fail(http.StatusConflict, v1.CodeConflict, fmt.Sprintf("login %s is by token, and takes no code", l.ID),
				"nothing to send: a token login ends by the runner's own check")
		case hubapi.LoginState(l.State).Terminal():
			return Fail(http.StatusConflict, v1.CodeConflict, fmt.Sprintf("login %s has already ended %s", l.ID, l.State),
				"start a new login; a code is good for the login whose link it came from, and only once")
		case l.State != string(v1.LoginWaiting):
			return Fail(http.StatusConflict, v1.CodeConflict, fmt.Sprintf("login %s is %s, and has no link out yet to have got a code from", l.ID, l.State),
				"wait until the login is waiting and shows its url, sign in there, and send the code it gives")
		case l.CancelRequestedAt.Valid:
			return Fail(http.StatusConflict, v1.CodeConflict, fmt.Sprintf("login %s is being cancelled", l.ID),
				"start a new login")
		}
		if err := q.SetLoginCode(ctx, db.SetLoginCodeParams{Code: in.Body.Code, ID: l.ID}); err != nil {
			return err
		}
		if l, err = q.GetLogin(ctx, l.ID); err != nil {
			return err
		}
		view = loginView(l)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &loginOutput{Body: view}, nil
}

func (h *Hub) cancelLogin(ctx context.Context, in *loginInput) (*loginOutput, error) {
	var view hubapi.Login
	err := h.store.Tx(ctx, func(q *db.Queries) error {
		l, err := loginOf(ctx, q, in.Runner, in.Login)
		if err != nil {
			return err
		}
		if err := h.endOrCancel(ctx, q, l, "cancelled before it reached the runner", h.now()); err != nil {
			return err
		}
		if l, err = q.GetLogin(ctx, l.ID); err != nil {
			return err
		}
		view = loginView(l)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &loginOutput{Body: view}, nil
}

// endOrCancel ends a login the hub means to stop. One no answer has carried
// to its runner ends here, and is never sent. Once one has, 'requested' says
// only that no report has come back: the runner may have stored the token or
// taken the login already, and an end written here would be the hub's guess
// against the machine's fact. That one gets a cancel_login, and stays open
// until the runner reports how it ended.
func (h *Hub) endOrCancel(ctx context.Context, q *db.Queries, l db.Login, why string, now time.Time) error {
	if l.State == string(hubapi.LoginRequested) && !l.SentAt.Valid {
		_, err := q.EndLogin(ctx, db.EndLoginParams{State: string(v1.LoginCancelled), Error: why, Now: store.Ms(now), ID: l.ID})
		return err
	}
	return q.RequestLoginCancel(ctx, db.RequestLoginCancelParams{Now: sql.NullInt64{Int64: store.Ms(now), Valid: true}, ID: l.ID})
}

// loginOf is one of a runner's logins, or the service API's own 404.
func loginOf(ctx context.Context, q *db.Queries, runnerID, loginID string) (db.Login, error) {
	l, err := q.GetLogin(ctx, loginID)
	if errors.Is(err, sql.ErrNoRows) || err == nil && l.RunnerID != runnerID {
		return db.Login{}, Fail(http.StatusNotFound, v1.CodeNotFound, fmt.Sprintf("runner %q has no login %q on this hub", runnerID, loginID),
			"check the runner and login ids: the answer that started the login carries both")
	}
	return l, err
}

func noRunner(id string) error {
	return Fail(http.StatusNotFound, v1.CodeNotFound, fmt.Sprintf("this hub has no runner %q", id), noRunnerAction())
}

// loginAtTheMachine is how the owner logs the account in at the runner's
// machine instead, for a runner this hub cannot ask: `yad account add` for an
// account, and for the harness's own default login `yad doctor`, which names
// the harness's own login command as that machine resolves it.
func loginAtTheMachine(harness, label string) string {
	if label == "" {
		return ", or at the machine, `" + runnerCommand("doctor") + "` names the harness's own login command"
	}
	return ", or log the account in at the machine with `" + runnerCommand("account", "add", harness, label) + "`"
}

// loginView is a login as the service API shows it: never its code or token.
func loginView(l db.Login) hubapi.Login {
	view := hubapi.Login{
		LoginID: l.ID, RunnerID: l.RunnerID, Harness: l.Harness, Account: l.Account, Add: l.AddAccount != 0, Method: v1.LoginMethod(l.Method),
		State: hubapi.LoginState(l.State), URL: l.Url, UserCode: l.UserCode, Error: l.Error,
		CodeSent:  l.Code != "",
		CreatedAt: time.UnixMilli(l.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(l.UpdatedAt).UTC(),
	}
	if l.CancelRequestedAt.Valid && !view.State.Terminal() {
		t := time.UnixMilli(l.CancelRequestedAt.Int64).UTC()
		view.CancelRequestedAt = &t
	}
	return view
}

// syncLogins records what a sync reports of this runner's logins and answers
// the controls each open one still needs. Every control is repeated until the
// reports answer it, since nothing acknowledges a control: start_login and
// login_token until the login is reported, login_code while it is reported
// waiting, cancel_login until it is reported over.
//
// A login the runner reported, and leaves out now while it is not over, is
// gone: the runner reports every login until its end is answered, so only a
// restart loses one. It ends here, failed, rather than waiting for ever.
//
// gated is whether the runner's current document advertises login, and adds
// whether it advertises accounts too (decision 0057). An add not yet sent to
// a runner whose document has arrived and says no accounts will never be
// taken — its owner turned adding off for this hub, or it runs an older yad —
// so it ends here, never sent, rather than waiting out its delivery.
func syncLogins(ctx context.Context, q *db.Queries, runnerID string, reports []v1.LoginReport, gated, adds, described bool, now time.Time) ([]v1.Control, error) {
	reported := map[string]bool{}
	for _, r := range reports {
		reported[r.LoginID] = true
		if _, err := q.RecordLoginReport(ctx, db.RecordLoginReportParams{
			State: string(r.State), Url: bounded(r.URL, maxLoginURL), UserCode: bounded(r.UserCode, maxLoginURL),
			Error: bounded(r.Error, maxLoginError), Now: store.Ms(now), ID: r.LoginID, RunnerID: runnerID,
			Terminal: boolInt(r.State.IsTerminal()),
		}); err != nil {
			return nil, err
		}
	}
	open, err := q.OpenLogins(ctx, runnerID)
	if err != nil {
		return nil, err
	}
	var out []v1.Control
	for _, l := range open {
		if l.State != string(hubapi.LoginRequested) && !reported[l.ID] {
			if _, err := q.EndLogin(ctx, db.EndLoginParams{State: string(v1.LoginFailed),
				Error: "the runner no longer has this login — it restarted, and a login in flight does not survive that; start a new login",
				Now:   store.Ms(now), ID: l.ID}); err != nil {
				return nil, err
			}
			continue
		}
		// Held back, not dropped, while the runner does not say it acts on
		// them: nothing acknowledges a control, and one it ignored would look
		// to the owner exactly like one that is taking a while.
		if !gated {
			continue
		}
		switch {
		case l.CancelRequestedAt.Valid:
			out = append(out, v1.Control{Kind: v1.ControlCancelLogin, LoginID: l.ID})
		case l.State == string(hubapi.LoginRequested) && l.AddAccount != 0 && !adds:
			if !described {
				continue
			}
			if _, err := q.EndLogin(ctx, db.EndLoginParams{State: string(v1.LoginFailed),
				Error: "the runner does not let this hub add accounts: its owner has turned it off for this hub, or it runs a yad from before adding — " +
					addAtTheMachine(l.Harness, l.Account),
				Now: store.Ms(now), ID: l.ID}); err != nil {
				return nil, err
			}
		case l.State == string(hubapi.LoginRequested):
			// From this answer on the runner may have it, and only its
			// report can end it (endOrCancel).
			if err := q.MarkLoginSent(ctx, db.MarkLoginSentParams{Now: sql.NullInt64{Int64: store.Ms(now), Valid: true}, ID: l.ID}); err != nil {
				return nil, err
			}
			c := v1.Control{Kind: v1.ControlStartLogin, LoginID: l.ID, Harness: l.Harness, Account: l.Account, Add: l.AddAccount != 0}
			if l.Method == string(v1.LoginByToken) {
				c.Kind, c.Token = v1.ControlLoginToken, l.Token
			}
			out = append(out, c)
		case l.State == string(v1.LoginWaiting) && l.Code != "":
			out = append(out, v1.Control{Kind: v1.ControlLoginCode, LoginID: l.ID, Code: l.Code})
		}
	}
	return out, nil
}

// sweepLogins ends the logins time alone decides: one no answer ever carried
// to its runner, and one sent that its runner has not finished. A runner's
// own report of an end still replaces the second, should one come.
func sweepLogins(ctx context.Context, q *db.Queries, now time.Time) error {
	if _, err := q.ExpireUndeliveredLogins(ctx, db.ExpireUndeliveredLoginsParams{
		Error: fmt.Sprintf("the runner did not sync within %s of the login starting, so it never heard of it — check it is running, and start a new login", loginDeliverWithin),
		Now:   store.Ms(now), Cutoff: store.Ms(now.Add(-loginDeliverWithin)),
	}); err != nil {
		return err
	}
	_, err := q.ExpireStaleLogins(ctx, db.ExpireStaleLoginsParams{
		Error: fmt.Sprintf("the runner was sent the login and did not say how it ended within %s — start a new login", loginFinishWithin),
		Now:   store.Ms(now), Cutoff: store.Ms(now.Add(-loginFinishWithin)),
	})
	return err
}

// bounded is s cut to at most n bytes, on a rune boundary.
func bounded(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
