package runner

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/store/db"
	v1 "github.com/skkap/yad/protocol/v1"
)

// pickAccount is the account a run takes: the first of the harness's, in the
// owner's order, that is free. False and no error is a harness the owner gave
// no accounts, whose runs use the harness's own default home.
//
// Every account being limited or needing login is reported as a failure of
// this run rather than silently falling back to the default home: running a
// hub's work on whatever login happens to sit in ~/.claude is the one thing an
// owner who configured accounts did not ask for. Not claiming at all while
// that is true is DEV-28's.
func (e *Exec) pickAccount(ctx context.Context, harness string) (account.Account, bool, error) {
	if !account.Supported(harness) {
		return account.Account{}, false, nil
	}
	accounts, err := account.Load(ctx, e.Store.Queries, e.Data, e.Config)
	if err != nil {
		return account.Account{}, false, err
	}
	all := account.For(accounts, harness)
	if len(all) == 0 {
		return account.Account{}, false, nil
	}
	if a, ok := account.First(accounts, harness); ok {
		return a, true, nil
	}
	return account.Account{}, false, &noFreeAccountError{harness: harness, accounts: all}
}

// noFreeAccountError says which accounts were in the way and what ends it, so
// the message names the next action rather than the fact.
type noFreeAccountError struct {
	harness  string
	accounts []account.Account
}

func (e *noFreeAccountError) Error() string {
	// Only the states that can actually be in the way are named. A state
	// added later and not handled here leaves the account out of the reason
	// rather than mislabelling it as something it is not.
	var limited, needsLogin []string
	for _, a := range e.accounts {
		switch a.State {
		case v1.AccountNeedsLogin:
			needsLogin = append(needsLogin, a.Label)
		case v1.AccountLimited:
			limited = append(limited, a.Label)
		}
	}
	msg := "no free " + e.harness + " account on this runner"
	if len(needsLogin) > 0 {
		msg += " — " + list(needsLogin) + " need login: run `yad account add " + e.harness + " " + needsLogin[0] + "` at the machine"
	}
	if len(limited) > 0 {
		msg += " — " + list(limited) + " at a usage limit"
	}
	return msg
}

func list(s []string) string {
	out := ""
	for i, v := range s {
		switch {
		case i == 0:
		case i == len(s)-1:
			out += " and "
		default:
			out += ", "
		}
		out += v
	}
	return out
}

// checkLogin decides whether a failed run means the account's login is gone.
//
// It never reads the harness's prose to find out. Claude reports a missing
// login with `is_error` true and `terminal_reason` "api_error" — and reports a
// bad model exactly the same way, in an object that also says
// `subtype: "success"` — so neither the class nor the message is proof of
// anything. Codex is worse: an auth failure is ten transport retries and
// several seconds of stderr before the error appears at all.
//
// So a failure of the two classes that mean "the harness did not say why" is
// treated as a question, and the harness's own login check answers it. That
// check costs no token and reaches no model, it is the same answer
// `yad account add` trusts, and it stays right across harness versions that
// change their wording.
func (e *Exec) checkLogin(ctx context.Context, a account.Account, binary string, res v1.Result, log *slog.Logger) {
	if res.Error == nil || !maybeAuth(res.Error.Class) || !account.CanLogIn(a.Harness) {
		return
	}
	// The deadline bounds the harness subprocess and nothing else. Writing the
	// state under it too would let a check that answered at 29.9s leave the
	// account unparked, and the next run would spend the same 30 seconds
	// asking again.
	checkCtx, cancel := context.WithTimeout(ctx, loginCheckTimeout)
	defer cancel()
	in, err := account.LoggedIn(checkCtx, a.Harness, binary, a.Home)
	if err != nil {
		log.Warn("could not check whether the account is still logged in", "err", err)
		return
	}
	state := v1.AccountFree
	if !in {
		state = v1.AccountNeedsLogin
	}
	if state == a.State {
		return
	}
	if err := account.SetState(ctx, e.Store.Queries, a.Harness, a.Label, state, time.Now()); err != nil {
		log.Warn("could not record the account's state", "state", state, "err", err)
		return
	}
	if state == v1.AccountNeedsLogin {
		log.Warn("the account has no working login; it is skipped until the owner logs it in again",
			"next_action", "yad account add "+a.Harness+" "+a.Label)
		return
	}
	log.Info("the account is logged in again")
}

// loginCheckTimeout bounds the harness's own login check. It reads a file and
// prints a line — a second is generous — but a harness that hangs must not
// hold a finished run's result behind it.
const loginCheckTimeout = 30 * time.Second

// maybeAuth is the classes that could be a login and could be anything else.
// The rest say what went wrong themselves: a prompt that does not fit, a usage
// limit, a session that is not here, a line that could not be read.
func maybeAuth(class string) bool {
	return class == adapter.ClassHarness || class == adapter.ClassHarnessExited
}

// setRunAccount records which account ran the run, for `yad status` and for a
// restart that has to say what it lost.
func (e *Exec) setRunAccount(ctx context.Context, c Claim, label string) {
	err := e.Store.SetRunAccount(ctx, db.SetRunAccountParams{
		Account: sql.NullString{String: label, Valid: true}, UpdatedAt: time.Now().UnixMilli(), Connection: c.Connection, ID: c.Run.RunID,
	})
	if err != nil {
		e.Log.Warn("could not record the run's account", "connection", c.Connection, "run", c.Run.RunID, "err", err)
	}
}
