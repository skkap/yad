package runner

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
)

// probeFor is a LoginProbe over the env, pointed at the test binary standing
// in for claude.
func probeFor(t *testing.T, e *env, cfg config.Config, mode ...string) *LoginProbe {
	t.Helper()
	x := &Exec{}
	fakeClaudeBinary(t, x, mode...)
	return &LoginProbe{Store: e.store, Accounts: accountsOf(e.paths.Data, cfg), Binary: x.Binary, Log: slog.New(slog.DiscardHandler)}
}

// The trap DEV-26 left and this task closes: an account marked needs-login is
// never given a run, so no run can ever find it working again. An owner who
// runs the harness's own login by hand gets the account back without
// `yad account add` — and without the runner having to be restarted.
func TestANeedsLoginAccountReturnsToServiceOnceTheOwnerLogsIn(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	cfg := accountConfig("work")
	home, err := account.Ensure(e.paths.Data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if err := account.SetState(ctx, e.store.Queries, "claude", "work", v1.AccountNeedsLogin, time.Now()); err != nil {
		t.Fatal(err)
	}
	p := probeFor(t, e, cfg)

	// Still logged out: the probe finds nothing and changes nothing. Without
	// this the test would pass on a probe that frees every account it sees.
	if freed := p.Sweep(ctx); freed != 0 {
		t.Fatalf("the probe freed %d accounts while the home has no login", freed)
	}
	if got := accountState(t, e, "work"); got != v1.AccountNeedsLogin {
		t.Fatalf("the account is %q, want needs_login", got)
	}

	// The owner logs in by hand. Nothing tells YAD; the probe is what finds
	// out. (plantCredential is what the fake harness's login writes.)
	plantCredential(t, e.paths.Data, "work")

	if freed := p.Sweep(ctx); freed != 1 {
		t.Fatalf("the probe freed %d accounts, want 1", freed)
	}
	if got := accountState(t, e, "work"); got != v1.AccountFree {
		t.Errorf("the account is %q, want free", got)
	}
	// And a run would now take it.
	accounts, err := account.Load(ctx, e.store.Queries, e.paths.Data, account.ListsOf(cfg), e.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := account.Soonest(accounts, "claude", time.Now()); !ok || a.Home != home {
		t.Errorf("a run would take %+v (%v); the account is logged in again", a, ok)
	}
}

// The rule DEV-26 established, which holds here too: a check that could not
// answer leaves the account exactly as it is. `codex login status` exits 1
// both for "not logged in" and for "could not read this home's config", so an
// error is an unanswered question — and an unanswered question is not a yes.
func TestAProbeThatCannotAnswerLeavesTheAccountAsItIs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// Logged in as far as the home is concerned, so a probe that guessed
	// from the filesystem rather than asking the harness would free it.
	plantCredential(t, e.paths.Data, "work")
	if err := account.SetState(ctx, e.store.Queries, "claude", "work", v1.AccountNeedsLogin, time.Now()); err != nil {
		t.Fatal(err)
	}
	p := probeFor(t, e, accountConfig("work"), "broken")
	if freed := p.Sweep(ctx); freed != 0 {
		t.Fatalf("the probe freed %d accounts on an answer it did not get", freed)
	}
	if got := accountState(t, e, "work"); got != v1.AccountNeedsLogin {
		t.Errorf("the account is %q; a check that could not answer moved it", got)
	}
}

// Two things the probe must not do: touch an account that is not parked, and
// rebuild a home that is not there. A label in config.toml whose home is gone
// reads needs_login (internal/account.Load); asking the harness about it would
// make the directory again.
func TestTheProbeLeavesFreeAccountsAndMissingHomesAlone(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	cfg := accountConfig("work", "removed")
	p := probeFor(t, e, cfg)

	if freed := p.Sweep(ctx); freed != 0 {
		t.Errorf("the probe freed %d accounts; one is already free and one has no home", freed)
	}
	if _, err := os.Stat(account.HomeDir(e.paths.Data, "claude", "removed")); !os.IsNotExist(err) {
		t.Errorf("the probe made a home for an account the owner removed (%v)", err)
	}
	if got := accountState(t, e, "work"); got != "" {
		t.Errorf("the probe wrote state %q for an account that was already free", got)
	}
}
