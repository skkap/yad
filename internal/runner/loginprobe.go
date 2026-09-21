package runner

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/store"
	v1 "github.com/skkap/yad/protocol/v1"
)

// LoginProbe is the way back out of needs-login.
//
// An account is marked needs_login by the run that found the login gone, and
// then never given another run — so no run can ever find it working again. The
// owner who runs `claude auth login` in that home by hand would otherwise find
// the account parked for ever, with nothing saying why, and `yad account add`
// the only way back. This task owns the sentence "the runner stops claiming
// for a harness whose accounts are all limited or need login, and says when
// that ends": for a limited account, when that ends is the reset; for a
// needs-login one, it is this.
//
// The probe costs nothing worth budgeting: `claude auth status` and
// `codex login status` read a file in the home they are pointed at and print a
// line. No token, no model, no network of YAD's.
//
// One rule, from DEV-26 and unchanged here: a check that could not answer
// leaves the account's state exactly as it is. `codex login status` exits 1
// both for "not logged in" and for "could not read this home's config", so an
// error is an unanswered question, and an unanswered question is not a "yes"
// any more than it is a "no". Only a definite answer moves anything, and the
// only move this makes is needs_login to free.
type LoginProbe struct {
	Store  *store.Store
	Config config.Config
	// Data is the profile's data directory, where the account homes live.
	Data string
	// Binary resolves a harness to its executable; nil is harness.Locate.
	Binary func(harness string) (string, bool)
	// Every is how often each needs-login account is asked again; zero is
	// probeEvery.
	Every time.Duration
	Clock Clock
	Log   *slog.Logger

	once sync.Once
}

// probeEvery is how often a needs-login account is asked whether it is logged
// in again. The owner who just finished a login by hand is the person waiting
// on it, so the answer wants to be minutes rather than hours; the cost is one
// short-lived process per parked account per interval, which is why it is not
// seconds. It is deliberately not configurable: nothing an owner would tune
// here beats "soon enough to notice, rare enough to ignore".
const probeEvery = 5 * time.Minute

func (p *LoginProbe) init() {
	p.once.Do(func() {
		if p.Binary == nil {
			p.Binary = harness.Locate
		}
		if p.Every <= 0 {
			p.Every = probeEvery
		}
		if p.Clock == nil {
			p.Clock = realClock{}
		}
		if p.Log == nil {
			p.Log = slog.New(slog.DiscardHandler)
		}
	})
}

// Run probes until ctx ends: at once, and then every interval. A nil probe
// returns at once.
func (p *LoginProbe) Run(ctx context.Context) {
	if p == nil {
		return
	}
	p.init()
	for {
		p.Sweep(ctx)
		select {
		case <-ctx.Done():
			return
		case <-p.Clock.After(p.Every):
		}
	}
}

// Sweep asks every needs-login account whether its login is back, and frees
// the ones that say yes. It returns how many it freed.
func (p *LoginProbe) Sweep(ctx context.Context) int {
	p.init()
	accounts, err := account.Load(ctx, p.Store.Queries, p.Data, p.Config, p.Clock.Now())
	if err != nil {
		p.Log.Warn("could not read account states; needs-login accounts are asked again at the next sweep", "err", err)
		return 0
	}
	freed := 0
	for _, a := range accounts {
		if ctx.Err() != nil {
			return freed
		}
		if p.probe(ctx, a) {
			freed++
		}
	}
	return freed
}

// probe asks about one account and reports whether it went back into service.
func (p *LoginProbe) probe(ctx context.Context, a account.Account) bool {
	if a.State != v1.AccountNeedsLogin || !account.CanLogIn(a.Harness) {
		return false
	}
	// A home that is not on disk is what `yad account remove` looks like to a
	// daemon still holding the config it started with (internal/account.Load).
	// There is nothing to ask a harness about, and asking would make the home
	// the owner deleted, so this one waits for `yad account add`.
	if _, err := os.Stat(a.Home); err != nil {
		return false
	}
	bin, ok := p.Binary(a.Harness)
	if !ok {
		return false
	}
	log := p.Log.With("harness", a.Harness, "account", a.Label)
	in, err := account.LoggedIn(ctx, a.Harness, bin, a.Home)
	if err != nil {
		// Debug, not warn: a harness that is installed but cannot answer for
		// this home would otherwise write a line every interval for as long
		// as the account is parked, and nothing here is new information. The
		// run that parked the account said what it could not tell.
		log.Debug("the account still cannot say whether it holds a login; it stays as it is", "err", err)
		return false
	}
	if !in {
		return false
	}
	if err := account.SetState(ctx, p.Store.Queries, a.Harness, a.Label, v1.AccountFree, time.Now()); err != nil {
		log.Warn("the account is logged in again and could not be recorded as free; it is asked again at the next sweep", "err", err)
		return false
	}
	// Nothing is told. A run parked on a reset hours away comes back
	// through its own connection's next sync, which reads account state
	// anyway, so the account is noticed within one sync interval without
	// anything here having to know that parked runs exist.
	log.Info("the account has been logged in again and is back in service")
	return true
}
