package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/store/db"
)

// accountRow is one account's stored state, reset and windows, as the runner
// left them after a turn.
func accountRow(t *testing.T, e *env, label string) (v1.AccountState, *time.Time) {
	t.Helper()
	rows, err := e.store.ListAllAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Harness != "claude" || r.Label != label {
			continue
		}
		var until *time.Time
		if r.LimitedUntil.Valid {
			at := time.UnixMilli(r.LimitedUntil.Int64).UTC()
			until = &at
		}
		return v1.AccountState(r.State), until
	}
	return "", nil
}

// The distinction the whole task turns on (DOMAIN.md, "Usage limit"): a usage
// limit is an exhausted subscription window and parks the account until its
// reset; a rate limit is transient API throttling the harness retried by
// itself, and costs the account nothing.
//
// Both rows matter. Without the first, "never marks it limited" would pass on
// a runner that cannot mark anything limited at all; without the second, a
// runner that parks an account for five hours because the API hiccuped once
// would pass too.
func TestAUsageLimitParksTheAccountAndATransientRetryDoesNot(t *testing.T) {
	// Relative to now, not a date written into the source. A fixed date is in
	// the future when it is typed and in the past forever after, and a limit
	// dated in the past is one SetLimit refuses - so a literal here would
	// quietly stop testing what this test is named for, and then start
	// failing. It asserted the defect round 1 found, and passed, for exactly
	// that reason.
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	for _, c := range []struct {
		name  string
		out   adapter.Outcome
		want  v1.AccountState
		until *time.Time
	}{
		{
			"a usage limit parks the account until its reset",
			adapter.Outcome{
				State: v1.RunFailed,
				Error: &v1.RunError{Class: adapter.ClassUsageLimit, Message: "you've hit your usage limit"},
				Limit: &adapter.Limit{Window: "five_hour", ResetAt: reset},
			},
			v1.AccountLimited, &reset,
		},
		{
			"a transient 429 the harness retried leaves the account free",
			adapter.Outcome{State: v1.RunSucceeded, FinalText: "done", APIRetries: 3},
			v1.AccountFree, nil,
		},
		{
			// A run can retry transiently and still fail for something else
			// entirely. The retries are not what failed it, and not the
			// account's problem either.
			"a turn that retried and then failed for another reason leaves it free",
			adapter.Outcome{
				State:      v1.RunFailed,
				Error:      &v1.RunError{Class: adapter.ClassPromptTooLong, Message: "prompt is too long"},
				APIRetries: 5,
			},
			v1.AccountFree, nil,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			plantCredential(t, e.paths.Data, "work")
			l := e.loop(t, 1)
			e.enqueue(t, testRun("a", "s1"))
			x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{Outcome: c.out}))
			claimAndRun(t, l, x)

			state, until := accountRow(t, e, "work")
			if state == "" {
				state = v1.AccountFree // no row written is the account untouched
			}
			if state != c.want {
				t.Errorf("account state = %q, want %q", state, c.want)
			}
			switch {
			case c.until == nil && until != nil:
				t.Errorf("account limited until %s, want no reset recorded", until)
			case c.until != nil && (until == nil || !until.Equal(*c.until)):
				t.Errorf("account limited until %v, want %s", until, c.until)
			}
		})
	}
}

// The acceptance criterion a hub actually sees: after a run hits a usage
// limit, the next sync's health carries that account's state, its reset, and
// every window's use and reset — so a hub can see why the runner stopped
// claiming (decision 0039).
//
// Driven through a whole run rather than by writing rows, because health and
// the claim read the same call and the point is that they agree.
func TestHealthCarriesTheAccountsStateWindowsAndReset(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	weekly := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	l.Data = e.paths.Data
	l.Config = accountConfig("work")
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{
			State: v1.RunFailed,
			Error: &v1.RunError{Class: adapter.ClassUsageLimit, Message: "you've hit your 5-hour limit"},
			Limit: &adapter.Limit{Window: "five_hour", ResetAt: reset},
			Windows: []adapter.Window{
				{Name: "five_hour", UsedPercent: 100, ResetAt: reset},
				{Name: "seven_day", UsedPercent: 62, ResetAt: weekly},
			},
		},
	}))
	claimAndRun(t, l, x)

	h := l.health(ctx, offerable{l.Pool.Reserve(l.Connection)})
	if len(h.Harnesses) != 1 || len(h.Harnesses[0].Accounts) != 1 {
		t.Fatalf("health harnesses = %+v", h.Harnesses)
	}
	if h.Harnesses[0].Ready {
		t.Error("a harness whose only account is at a usage limit is reported ready")
	}
	got := h.Harnesses[0].Accounts[0]
	if got.State != v1.AccountLimited {
		t.Errorf("health says the account is %q, want limited", got.State)
	}
	if got.LimitedUntil == nil || !got.LimitedUntil.Equal(reset) {
		t.Errorf("health says limited until %v, want %s", got.LimitedUntil, reset)
	}
	if len(got.Windows) != 2 {
		t.Fatalf("health windows %+v, want both", got.Windows)
	}
	for i, w := range []v1.AccountWindow{
		{Name: "five_hour", UsedPercent: 100, ResetsAt: &reset},
		{Name: "seven_day", UsedPercent: 62, ResetsAt: &weekly},
	} {
		if got.Windows[i].Name != w.Name || got.Windows[i].UsedPercent != w.UsedPercent {
			t.Errorf("window %d = %+v, want %+v", i, got.Windows[i], w)
		}
		if got.Windows[i].ResetsAt == nil || !got.Windows[i].ResetsAt.Equal(*w.ResetsAt) {
			t.Errorf("window %s resets %v, want %s", w.Name, got.Windows[i].ResetsAt, w.ResetsAt)
		}
	}
	// The home and everything the harness wrote in it stay out of health.
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), e.paths.Data) || strings.Contains(string(b), sentinel) {
		t.Errorf("health carries something from the account's home: %s", b)
	}
}

// A run that succeeds still reports what it heard, so a hub sees an account
// running low rather than only one that has run out. Nothing about the account
// moves off free.
func TestASucceededRunStillRecordsItsWindows(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	l.Data = e.paths.Data
	l.Config = accountConfig("work")
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{
			State: v1.RunSucceeded, FinalText: "done",
			Windows: []adapter.Window{{Name: "five_hour", UsedPercent: 96, ResetAt: reset}},
		},
	}))
	claimAndRun(t, l, x)

	h := l.health(ctx, offerable{l.Pool.Reserve(l.Connection)})
	got := h.Harnesses[0].Accounts[0]
	if got.State != v1.AccountFree {
		t.Errorf("a succeeded run left the account %q, want free", got.State)
	}
	if !h.Harnesses[0].Ready {
		t.Error("a harness with a free account is reported not ready")
	}
	if len(got.Windows) != 1 || got.Windows[0].UsedPercent != 96 {
		t.Fatalf("health windows %+v, want five_hour at 96%%", got.Windows)
	}
}

// A run offered while every account of its harness is at a usage limit is
// not claimed at all: the offer goes back in the hub's queue for a runner
// that can take it, and this runner's health says until when. Refusing would
// be worse — a refusal is a terminal result and ends the run everywhere,
// and there is nothing wrong with the run.
func TestARunIsNotClaimedWhileEveryAccountIsLimited(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	if err := account.SetLimit(ctx, e.store.Queries, "claude", "work", reset, time.Now()); err != nil {
		t.Fatal(err)
	}
	l := e.loop(t, 1)
	l.Data, l.Config = e.paths.Data, accountConfig("work")
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded},
	}))
	claimAndRun(t, l, x)

	if _, err := e.store.GetRun(ctx, db.GetRunParams{Connection: "hub", ID: "a"}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the run was claimed (%v); every account is limited until %s", err, reset)
	}
	if _, ok := outboxResult(t, e, "a"); ok {
		t.Error("the runner owes the hub a result for a run it never took")
	}
	// And the hub was told why, with the moment it ends.
	h := l.health(ctx, offerable{l.Pool.Reserve(l.Connection)})
	if len(h.Harnesses) != 1 || h.Harnesses[0].Ready {
		t.Fatalf("health harnesses = %+v, want claude not ready", h.Harnesses)
	}
	if got := h.Harnesses[0].Accounts[0].LimitedUntil; got == nil || !got.Equal(reset) {
		t.Errorf("health says limited until %v, want %s", got, reset)
	}
}

// A usage limit the harness could not date is dated from the windows the same
// turn reported, not from the constant. A bare 429 carries no reset; a window
// at 100% carries one, and it is the account's own word rather than a guess.
func TestAnUndatedLimitIsDatedFromTheTurnsOwnWindows(t *testing.T) {
	e := newEnv(t)
	plantCredential(t, e.paths.Data, "work")
	refill := time.Now().Add(90 * time.Minute).UTC().Truncate(time.Second)
	later := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{
			State: v1.RunFailed,
			Error: &v1.RunError{Class: adapter.ClassUsageLimit, Message: "429"},
			// What a bare 429 leaves: a limit with neither window nor reset.
			Limit: &adapter.Limit{},
			Windows: []adapter.Window{
				{Name: "five_hour", UsedPercent: 100, ResetAt: refill},
				{Name: "seven_day", UsedPercent: 100, ResetAt: later},
			},
		},
	}))
	claimAndRun(t, l, x)

	state, until := accountRow(t, e, "work")
	if state != v1.AccountLimited {
		t.Fatalf("account state = %q, want limited", state)
	}
	if until == nil || !until.Equal(refill) {
		t.Errorf("limited until %v, want the soonest full window's reset %s", until, refill)
	}
}

// The defect round 1 found, walked as it reported it: a limit whose reset the
// harness did not give must not be dated from a window whose reset has already
// passed.
//
// Windows outlive the limits they explain and nothing ages them out, so a full
// window with an elapsed reset is the ordinary residue of every limit that has
// already resolved. Dating a new limit from one writes a row that reads free
// the instant it is written: the account is offered again at once, fails after
// about three minutes of the harness's own retry ladder, and is offered again,
// for every run the hub submits.
//
// Every test on this branch used a future reset before this one, which is why
// the whole suite agreed with the code.
func TestAnUndatedLimitIsNotDatedFromAWindowThatHasAlreadyReset(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	// What an earlier limit left behind: the window that caused it, full, and
	// its reset now in the past.
	stale := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	err := account.SetWindows(ctx, e.store.Queries, "claude", "work",
		[]v1.AccountWindow{{Name: "five_hour", UsedPercent: 100, ResetsAt: &stale}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	started := time.Now()
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{
			State: v1.RunFailed,
			Error: &v1.RunError{Class: adapter.ClassUsageLimit, Message: "429"},
			// The bare-429 shape this branch recorded: no window, no reset.
			Limit: &adapter.Limit{},
		},
	}))
	claimAndRun(t, l, x)

	state, until := accountRow(t, e, "work")
	if state != v1.AccountLimited {
		t.Fatalf("account state = %q, want limited", state)
	}
	if until == nil {
		t.Fatal("no reset recorded")
	}
	if !until.After(started) {
		t.Fatalf("limited until %s, which is already past: the account is parked for no time and the hub loops through offer-and-fail", until)
	}
	// And it is the fallback rather than some other stale value.
	want := started.Add(30 * time.Minute)
	if d := until.Sub(want); d > time.Minute || d < -time.Minute {
		t.Errorf("limited until %s, want about %s", until, want)
	}
}

// A limit nothing dated is the one usage limit that does not become a wait:
// there is no moment to come back at, so parking the run would be a park
// nothing ends. It is refused instead, and the refusal names the state
// rather than inventing a time.
//
// Reachable, not dead. account.SetLimit dates every limit it writes — from
// the harness, from a full window, or from limitWithoutReset — so this is a
// row from another version of yad or a hand-edited database, and stateOf
// deliberately leaves such a row limited rather than reading it as free.
// Without this test the branch reads as unreachable and invites deletion.
func TestAnUndatedLimitRefusesTheRunRatherThanParkingIt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	// SetState, not SetLimit: the state without the reset is what a row
	// written by something other than SetLimit looks like.
	if err := account.SetState(ctx, e.store.Queries, "claude", "work", v1.AccountLimited, time.Now()); err != nil {
		t.Fatal(err)
	}
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded},
	}))
	claimAndRun(t, l, x)

	if got := localRun(t, e, "a").State; got == string(v1.RunWaiting) {
		t.Fatal("the run is waiting on a limit with no reset; nothing would ever end that wait")
	}
	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunFailed {
		t.Fatalf("result = %+v, %v, want failed", res, ok)
	}
	if res.Error.Class != ClassRefused {
		t.Errorf("error class = %q, want %q", res.Error.Class, ClassRefused)
	}
	if want := "work at an undated usage limit"; !strings.Contains(res.Error.Message, want) {
		t.Errorf("the refusal is %q, want it to contain %q", res.Error.Message, want)
	}
}
