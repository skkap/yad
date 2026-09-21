package runner

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/store/db"
	v1 "github.com/skkap/yad/protocol/v1"
)

// pickAccount is the account a run takes: of the harness's free accounts, the
// one whose usage window resets soonest (decision 0039). False and no error is
// a harness the owner gave no accounts, whose runs use the harness's own
// default home.
//
// Every account being limited or needing login is an error rather than a
// silent fall back to the default home: running a hub's work on whatever login
// happens to sit in ~/.claude is the one thing an owner who configured accounts
// did not ask for. What the caller does with the error depends on what is in
// the way — a limited account is a wait, a login is the owner's to finish.
func (e *Exec) pickAccount(ctx context.Context, harness string) (account.Account, bool, error) {
	if !account.Supported(harness) {
		return account.Account{}, false, nil
	}
	// One moment for the whole choice: the states Load derives and the
	// resets Soonest ranks are then judged against the same instant.
	now := time.Now()
	accounts, err := account.Load(ctx, e.Store.Queries, e.Data, e.Config, now)
	if err != nil {
		return account.Account{}, false, err
	}
	all := account.For(accounts, harness)
	if len(all) == 0 {
		return account.Account{}, false, nil
	}
	if a, ok := account.Soonest(accounts, harness, now); ok {
		return a, true, nil
	}
	return account.Account{}, false, &noFreeAccountError{paths: e.Paths, harness: harness, accounts: all}
}

// noFreeAccountError says which accounts were in the way and what ends it, so
// the message names the next action rather than the fact.
type noFreeAccountError struct {
	paths    config.Paths
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
			// The reset is the next action for a limited account: nobody has
			// to do anything, and this says when it ends. A limit with no
			// reset is one nothing dated, and says so rather than a time.
			at := "at an undated usage limit"
			if a.LimitedUntil != nil {
				at = "at a usage limit until " + a.LimitedUntil.UTC().Format(time.RFC3339)
			}
			limited = append(limited, a.Label+" "+at)
		}
	}
	msg := "no free " + e.harness + " account on this runner"
	if len(needsLogin) > 0 {
		msg += " — " + list(needsLogin) + " need login: run `" + e.paths.RemoteCommand("account", "add", e.harness, needsLogin[0]) + "` at the machine"
	}
	if len(limited) > 0 {
		msg += " — " + list(limited)
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
	// No deadline here: account.LoggedIn bounds its own subprocess, and more
	// tightly than this ever did. A second, looser one around it could never
	// fire, and the comment that justified it reasoned about a case it had
	// made impossible. The state write below must not be under a deadline
	// anyway — a check that answered just before one would leave the account
	// unparked and the next run would ask all over again.
	in, err := account.LoggedIn(ctx, a.Harness, binary, a.Home)
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
			"next_action", e.Paths.Command("account", "add", a.Harness, a.Label))
		return
	}
	log.Info("the account is logged in again")
}

// maybeAuth is the classes that could be a login and could be anything else.
// The rest say what went wrong themselves: a prompt that does not fit, a usage
// limit, a session that is not here, a line that could not be read.
func maybeAuth(class string) bool {
	return class == adapter.ClassHarness || class == adapter.ClassHarnessExited
}

// setRunAccount records which account runs the run's next turn and how many
// moves it has made, for `yad status` and for a restart that has to say what
// it lost. The count is written here, at the move, rather than at the end of
// the turn on the new account: a run lost during that turn has still moved.
func (e *Exec) setRunAccount(ctx context.Context, c Claim, label string, switches int) {
	err := e.Store.SetRunAccount(ctx, db.SetRunAccountParams{
		Account: sql.NullString{String: label, Valid: true}, AccountSwitches: int64(switches),
		UpdatedAt: time.Now().UnixMilli(), Connection: c.Connection, ID: c.Run.RunID,
	})
	if err != nil {
		e.Log.Warn("could not record the run's account", "connection", c.Connection, "run", c.Run.RunID, "err", err)
	}
}

// recordUsage writes what the turn learned about the account's subscription
// windows, and parks the account when the turn stopped because one of them was
// exhausted (decision 0039, "limits are reported").
//
// The distinction it exists to keep (DOMAIN.md, "Usage limit"): only a usage
// limit — a window the account has spent, with a reset — reaches Limit and
// parks anything. Transient API throttling the harness retried by itself is a
// rate limit; it arrives as Outcome.APIRetries, is reported as a metric, and
// is deliberately not read here. Parking a working account for five hours
// because the API answered 429 once and the harness carried on is the failure
// this function is shaped to avoid.
//
// It marks the account and no more. The executor's turn loop reads the same
// Limit and decides what becomes of the run — another account, or a wait —
// and it reads the account's state back from the store afterwards, so the
// account this parks is the one the next pick cannot choose.
func (e *Exec) recordUsage(ctx context.Context, a account.Account, out adapter.Outcome, log *slog.Logger) {
	now := time.Now()
	// Windows first, and whatever the turn's outcome: a turn that succeeded
	// still heard how much of each window it left, which is what lets a hub
	// see an account running low rather than only one that has run out.
	if len(out.Windows) > 0 {
		if err := account.SetWindows(ctx, e.Store.Queries, a.Harness, a.Label, accountWindows(out.Windows), now); err != nil {
			log.Warn("could not record the account's usage windows", "err", err)
		}
	}
	if out.Limit == nil {
		return
	}
	// A harness that reported a limit without a reset is dated from its own
	// windows where it can be: the turn's, then what earlier runs recorded for
	// this account. A window at 100% says when the account comes back, and
	// that beats any constant. account.SetLimit dates what is left.
	reset := out.Limit.ResetAt
	if reset.IsZero() {
		reset = account.RefillAt(now, accountWindows(out.Windows), a.Windows)
	}
	if err := account.SetLimit(ctx, e.Store.Queries, a.Harness, a.Label, reset, now); err != nil {
		log.Warn("could not record the account's usage limit", "err", err)
		return
	}
	// The window and the reset, never the account's contents. A reset the
	// harness did not give is logged as absent rather than as the zero time,
	// which would read as 1970 to whoever is looking.
	attrs := []any{"window", out.Limit.Window}
	if out.Limit.ResetAt.IsZero() {
		attrs = append(attrs, "resets_at", "not reported")
	} else {
		attrs = append(attrs, "resets_at", out.Limit.ResetAt.UTC())
	}
	log.Info("the account is at a usage limit; it is skipped until its window resets", attrs...)
}

// accountWindows is the adapter's windows as the protocol reports them. The
// adapters already normalise each harness's scale to 0-100, so this only moves
// a zero reset time to an absent one: a window whose reset the harness did not
// give must not read as refilling at the epoch.
func accountWindows(ws []adapter.Window) []v1.AccountWindow {
	out := make([]v1.AccountWindow, 0, len(ws))
	for _, w := range ws {
		aw := v1.AccountWindow{Name: w.Name, UsedPercent: w.UsedPercent}
		if !w.ResetAt.IsZero() {
			at := w.ResetAt.UTC()
			aw.ResetsAt = &at
		}
		out = append(out, aw)
	}
	return out
}
