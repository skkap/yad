package capability

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/probe"
	"github.com/skkap/yad/internal/shellword"
)

// loginRecheck is how long an answer about a default home's login is kept.
// The daemon re-probes every 15 seconds, and asking the harness that often
// would start a process four times a minute for an answer that changes when a
// person logs in; a minute is soon enough for the person waiting on it.
const loginRecheck = time.Minute

type loginAnswer struct {
	err, warning string
	until        time.Time
}

var (
	loginMu    sync.Mutex
	loginAsked = map[string]loginAnswer{}
	loginNow   = time.Now
)

// DefaultLogins asks each drivable harness the owner gave no accounts whether
// its own default home holds a login, and reports one that does not as unable
// to take runs (decision 0053). Without it such a harness is ready by every
// other test and fails every run it is given, and a hub routing on ready sends
// it all of them.
//
// A harness with accounts is left alone: its runs never use the default home,
// and each account's login is its own state (decision 0039). A check that
// could not answer is a warning, never an error — an unanswered question is
// not a "no", as the account probe has it.
func DefaultLogins(ctx context.Context, found []harness.Detected, cfg config.Config) {
	for i, d := range found {
		if !d.Ready() || !account.CanLogIn(d.ID) || len(cfg.Harness[d.ID].Accounts) > 0 {
			continue
		}
		e, w := defaultLogin(ctx, d)
		if e != "" {
			found[i].Error, found[i].NeedsLogin = e, true
		}
		if w != "" {
			found[i].Warnings = append(found[i].Warnings, w)
		}
	}
}

func defaultLogin(ctx context.Context, d harness.Detected) (errMsg, warning string) {
	key := d.ID + "\x00" + d.Path
	loginMu.Lock()
	a, ok := loginAsked[key]
	loginMu.Unlock()
	if ok && loginNow().Before(a.until) {
		return a.err, a.warning
	}
	in, err := account.LoggedIn(ctx, d.ID, d.Path, "")
	if ctx.Err() != nil {
		// The caller stopped asking; that says nothing about the login.
		return "", ""
	}
	a = loginAnswer{until: loginNow().Add(loginRecheck)}
	// Neither message quotes what the harness printed or names a path: both
	// travel to every hub (DEV-60, DEV-67).
	switch {
	case err != nil:
		a.warning = "yad could not tell whether it is logged in, and runs may fail — as the runner's user, " + probed(d).Try(command(d, account.StatusArgs(d.ID)), "what it says")
	case !in:
		a.err = "not logged in on this machine — as the runner's user, " + probed(d).Do(command(d, account.LoginArgs(d.ID)), "log it in")
	}
	loginMu.Lock()
	loginAsked[key] = a
	loginMu.Unlock()
	return a.err, a.warning
}

// ForgetDefaultLogin drops what is kept about a harness's default login, so
// the next document asks again: a hub has just logged it in (decision 0055),
// and the owner watching should not wait out loginRecheck to see it.
func ForgetDefaultLogin(id string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	for key := range loginAsked {
		if strings.HasPrefix(key, id+"\x00") {
			delete(loginAsked, key)
		}
	}
}

// probed is the binary detection probed, as probe.Find answers for it: the
// program a printed command names is the one the check ran.
func probed(d harness.Detected) probe.Found { return probe.Find(d.EnvPath, d.Binary, d.VersionArgs) }

// command is one of the harness's own commands, naming the binary detection
// probed the way every probe's advice does (probe.Found.Command): by its name
// when PATH found it, as "$YAD_<ID>_PATH" when an override did — never the
// path, which is the machine's and stays on it. A runner whose environment
// moves the harness's home carries that move as a placeholder: the value is a
// path, and the command without it would reach a home no run uses.
func command(d harness.Detected, args []string) string {
	return homePrefix(d.ID) + probed(d).Command(args...)
}

func homePrefix(id string) string {
	v := account.HomeVar(id)
	if v == "" || os.Getenv(v) == "" {
		return ""
	}
	return v + "=" + shellword.Quote("<runner "+v+">") + " "
}
