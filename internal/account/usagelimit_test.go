package account

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// limitEnv is a store with one claude account whose home is on disk, ready for
// a limit to be written against it.
func limitEnv(t *testing.T) (context.Context, *store.Store, string, config.Config) {
	t.Helper()
	data := t.TempDir()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := Ensure(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"work"}}}
	return ctx, st, data, cfg
}

// The rule the package states once: limited_until is the authority and the
// state is derived from it. A reset is a fact about time, so nothing has to
// come along and write the account free when its moment passes.
func TestStateOfDerivesLimitedFromItsReset(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Second)
	for _, c := range []struct {
		name  string
		state v1.AccountState
		until *time.Time
		want  v1.AccountState
	}{
		{"a limit still to come", v1.AccountLimited, &future, v1.AccountLimited},
		{"a limit whose reset has passed", v1.AccountLimited, &past, v1.AccountFree},
		{"a limit resetting exactly now", v1.AccountLimited, &now, v1.AccountFree},
		// SetLimit dates every limit it writes, so this is a row from another
		// version. Reading it free would send runs at an account that cannot
		// take them, and nothing here knows when it ends.
		{"a limit with no reset at all", v1.AccountLimited, nil, v1.AccountLimited},
		// Neither says anything about time, and a stale reset beside one must
		// not move it.
		{"needs login is never timed out", v1.AccountNeedsLogin, &past, v1.AccountNeedsLogin},
		{"free stays free", v1.AccountFree, &past, v1.AccountFree},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := stateOf(c.state, c.until, now); got != c.want {
				t.Errorf("stateOf(%q, %v) = %q, want %q", c.state, c.until, got, c.want)
			}
		})
	}
}

// The row where the stored column and the truth disagree: state says limited,
// the reset has passed. Load, Report and the health report must all say free,
// because that pair is what DEV-28 decides whether to claim from and what a
// hub renders. A hub shown "limited until a time in the past" would be reading
// a contradiction.
func TestAnExpiredLimitReadsFreeEverywhere(t *testing.T) {
	ctx, st, data, cfg := limitEnv(t)
	past := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	// Written through the store rather than through SetLimit, which now
	// refuses to date a limit in the past. The row is still reachable - an
	// older yad wrote one, or the limit simply ran out while the row sat
	// there - and how it reads is the whole point of this test.
	err := st.SetAccountLimit(ctx, db.SetAccountLimitParams{
		Harness: "claude", Label: "work",
		LimitedUntil: sql.NullInt64{Int64: past.UnixMilli(), Valid: true},
		UpdatedAt:    time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The row itself still says limited: nothing writes the expiry back.
	rows, err := st.ListAllAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].State != string(v1.AccountLimited) {
		t.Fatalf("stored rows %+v, want one limited row", rows)
	}

	loaded, err := Load(ctx, st.Queries, data, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got := loaded
	if len(got) != 1 {
		t.Fatalf("got %d accounts, want 1", len(got))
	}
	if got[0].State != v1.AccountFree {
		t.Errorf("Load says %q, want free — the reset has passed", got[0].State)
	}
	if _, ok := Soonest(got, "claude", time.Now()); !ok {
		t.Error("an account whose limit has passed is not offered a run")
	}
	rep := got[0].Report()
	if rep.State != v1.AccountFree {
		t.Errorf("Report says %q, want free", rep.State)
	}
	if rep.LimitedUntil != nil {
		t.Errorf("a free account reports limited_until %s; a hub would read a contradiction", rep.LimitedUntil)
	}
}

// A limit still to come reads limited everywhere, with its reset, and is
// skipped for runs.
func TestALiveLimitReadsLimitedEverywhere(t *testing.T) {
	ctx, st, data, cfg := limitEnv(t)
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Millisecond)
	if err := SetLimit(ctx, st.Queries, "claude", "work", reset, time.Now()); err != nil {
		t.Fatal(err)
	}
	got, err := Load(ctx, st.Queries, data, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != v1.AccountLimited {
		t.Fatalf("Load says %q, want limited", got[0].State)
	}
	if got[0].LimitedUntil == nil || !got[0].LimitedUntil.Equal(reset) {
		t.Errorf("limited until %v, want %s", got[0].LimitedUntil, reset)
	}
	if _, ok := Soonest(got, "claude", time.Now()); ok {
		t.Error("a limited account was offered a run")
	}
	rep := got[0].Report()
	if rep.State != v1.AccountLimited || rep.LimitedUntil == nil || !rep.LimitedUntil.Equal(reset) {
		t.Errorf("report %+v, want limited until %s", rep, reset)
	}
}

// Load judges a limit against the moment it is given and nothing else, and
// Report does not judge it again. The moments are decades from the wall in
// both directions, so a wall-clock read anywhere on the way flips the answer
// rather than agreeing with it by luck (DEV-85).
func TestALimitIsJudgedAgainstTheMomentLoadIsGiven(t *testing.T) {
	const decades = 30 * 365 * 24 * time.Hour
	for _, c := range []struct {
		name   string
		offset time.Duration // of the moment from the wall
		reset  time.Duration // of the limit from the moment
		want   v1.AccountState
	}{
		{"a live limit at a moment behind the wall", -decades, time.Hour, v1.AccountLimited},
		{"an expired limit at a moment ahead of the wall", decades, -time.Hour, v1.AccountFree},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, st, data, cfg := limitEnv(t)
			now := time.Now().Add(c.offset).UTC().Truncate(time.Millisecond)
			reset := now.Add(c.reset)
			if err := SetLimit(ctx, st.Queries, "claude", "work", reset, reset.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			got, err := Load(ctx, st.Queries, data, cfg, now)
			if err != nil {
				t.Fatal(err)
			}
			if got[0].State != c.want {
				t.Errorf("Load at %s says %q, want %q", now, got[0].State, c.want)
			}
			if rep := got[0].Report(); rep.State != c.want {
				t.Errorf("Report says %q where Load said %q", rep.State, got[0].State)
			}
		})
	}
}

// A usage limit the harness could not date is still a limit and is still
// dated, because a limit nothing ever ends is a park. The account comes back
// after limitWithoutReset and the harness gets to say again.
func TestAnUndatedLimitIsDatedRatherThanPermanent(t *testing.T) {
	ctx, st, data, cfg := limitEnv(t)
	now := time.Now()
	if err := SetLimit(ctx, st.Queries, "claude", "work", time.Time{}, now); err != nil {
		t.Fatal(err)
	}
	got, err := Load(ctx, st.Queries, data, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != v1.AccountLimited {
		t.Fatalf("state %q, want limited", got[0].State)
	}
	if got[0].LimitedUntil == nil {
		t.Fatal("an undated limit was stored undated; nothing would ever end it")
	}
	want := now.Add(limitWithoutReset)
	if d := got[0].LimitedUntil.Sub(want); d > time.Second || d < -time.Second {
		t.Errorf("limited until %s, want about %s", got[0].LimitedUntil, want)
	}
}

// Windows are kept per name and merged across runs: a turn that heard about
// one window must not erase what an earlier turn knew about another. Codex's
// updates are sparse by design, so this is the ordinary case, not an edge.
func TestWindowsAreMergedPerNameAcrossRuns(t *testing.T) {
	ctx, st, data, cfg := limitEnv(t)
	first := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	second := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Millisecond)
	err := SetWindows(ctx, st.Queries, "claude", "work", []v1.AccountWindow{
		{Name: "five_hour", UsedPercent: 33, ResetsAt: &first},
		{Name: "seven_day", UsedPercent: 47, ResetsAt: &second},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// A later turn heard only about the five-hour window.
	err = SetWindows(ctx, st.Queries, "claude", "work", []v1.AccountWindow{
		{Name: "five_hour", UsedPercent: 81, ResetsAt: &first},
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Load(ctx, st.Queries, data, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ws := got[0].Report().Windows
	if len(ws) != 2 {
		t.Fatalf("windows %+v, want both", ws)
	}
	// Name order, so two syncs of an unchanged account read the same.
	if ws[0].Name != "five_hour" || ws[1].Name != "seven_day" {
		t.Errorf("window order %q, %q", ws[0].Name, ws[1].Name)
	}
	if ws[0].UsedPercent != 81 {
		t.Errorf("five_hour used %v%%, want the later turn's 81%%", ws[0].UsedPercent)
	}
	if ws[1].UsedPercent != 47 {
		t.Errorf("seven_day used %v%%, want the earlier turn's 47%% kept", ws[1].UsedPercent)
	}
}

// A window whose reset the harness did not give is reported without one,
// rather than as refilling at the epoch.
func TestAWindowWithNoResetReportsNone(t *testing.T) {
	ctx, st, data, cfg := limitEnv(t)
	err := SetWindows(ctx, st.Queries, "claude", "work",
		[]v1.AccountWindow{{Name: "five_hour", UsedPercent: 12}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	got, err := Load(ctx, st.Queries, data, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ws := got[0].Report().Windows
	if len(ws) != 1 || ws[0].ResetsAt != nil {
		t.Errorf("windows %+v, want one with no reset", ws)
	}
}

// An account no run has been through reports no windows at all: absence is
// data, and a window at zero is a claim nothing made.
func TestAnAccountWithNoRunsReportsNoWindows(t *testing.T) {
	ctx, st, data, cfg := limitEnv(t)
	got, err := Load(ctx, st.Queries, data, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if ws := got[0].Report().Windows; ws != nil {
		t.Errorf("windows %+v, want none", ws)
	}
}

// An undated limit is dated from the account's own windows before any constant
// is reached: a window the harness called full says when the account comes
// back, and that is a fact rather than a guess.
func TestRefillAtTakesTheSoonestFullWindowStillToCome(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	soon := now.Add(3 * time.Hour)
	later := now.Add(3 * 24 * time.Hour)
	gone := now.Add(-time.Minute)
	zero := time.Time{}
	full := func(name string, at *time.Time) v1.AccountWindow {
		return v1.AccountWindow{Name: name, UsedPercent: 100, ResetsAt: at}
	}
	for _, c := range []struct {
		name string
		sets [][]v1.AccountWindow
		want time.Time
	}{
		{"both windows full: the soonest is when it could work again",
			[][]v1.AccountWindow{{full("five_hour", &soon), full("seven_day", &later)}}, soon},
		{"the later set still counts",
			[][]v1.AccountWindow{nil, {full("seven_day", &later)}}, later},
		// Only a full window says the account is out. One with headroom
		// carries a reset too, and reading it would date the limit from a
		// window that is not the reason for it.
		{"a window with headroom is not a reason",
			[][]v1.AccountWindow{{{Name: "five_hour", UsedPercent: 62, ResetsAt: &soon}}}, zero},
		{"a full window the harness did not date",
			[][]v1.AccountWindow{{full("five_hour", nil)}}, zero},
		// The case the whole clock argument exists for: windows outlive the
		// limits they explain and nothing ages them out, so a full window
		// whose reset has passed is the ordinary residue of a limit already
		// over. Dating a new limit from it parks the account for no time.
		{"a full window whose reset has passed",
			[][]v1.AccountWindow{{full("five_hour", &gone)}}, zero},
		{"a full window resetting exactly now",
			[][]v1.AccountWindow{{full("five_hour", &now)}}, zero},
		{"a stale window does not win over a live one",
			[][]v1.AccountWindow{{full("five_hour", &gone), full("seven_day", &later)}}, later},
		{"a full window dated at the zero time",
			[][]v1.AccountWindow{{full("five_hour", &zero)}}, zero},
		{"nothing at all", nil, zero},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := RefillAt(now, c.sets...)
			if !got.Equal(c.want) {
				t.Errorf("RefillAt = %s, want %s", got, c.want)
			}
		})
	}
}

// The writer refuses to date a limit in the past, whichever way one reaches
// it: a reset the harness itself reported behind this machine's clock, or a
// stale window RefillAt did not catch. A row dated in the past reads free the
// instant it is written, so the account is offered, fails, and is offered
// again for every run the hub submits.
func TestSetLimitRefusesAResetThatHasAlreadyPassed(t *testing.T) {
	for _, c := range []struct {
		name   string
		offset time.Duration
	}{
		{"a reset a minute ago", -time.Minute},
		{"a reset exactly now", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, st, data, cfg := limitEnv(t)
			now := time.Now()
			if err := SetLimit(ctx, st.Queries, "claude", "work", now.Add(c.offset), now); err != nil {
				t.Fatal(err)
			}
			got, err := Load(ctx, st.Queries, data, cfg, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if got[0].State != v1.AccountLimited {
				t.Fatalf("state %q, want limited - the account was parked for no time at all", got[0].State)
			}
			want := now.Add(limitWithoutReset)
			if got[0].LimitedUntil == nil {
				t.Fatal("no reset recorded")
			}
			if d := got[0].LimitedUntil.Sub(want); d > time.Second || d < -time.Second {
				t.Errorf("limited until %s, want about %s - the fallback did not fire", got[0].LimitedUntil, want)
			}
		})
	}
}

// Two turns observe an account at different moments and can reach the store in
// either order. The row keeps what the harness said most recently, not what
// arrived most recently: an older snapshot landing second would make health
// report use and a reset that have already moved on, which is what the
// migration's concurrency comment promises it does not.
func TestAnOlderWindowObservationDoesNotOverwriteANewerOne(t *testing.T) {
	ctx, st, data, cfg := limitEnv(t)
	early := time.Now().Add(-time.Minute)
	late := time.Now()
	earlyReset := late.Add(time.Hour).UTC().Truncate(time.Millisecond)
	lateReset := late.Add(2 * time.Hour).UTC().Truncate(time.Millisecond)

	// The later observation lands first, as the slower of two turns does.
	err := SetWindows(ctx, st.Queries, "claude", "work",
		[]v1.AccountWindow{{Name: "five_hour", UsedPercent: 80, ResetsAt: &lateReset}}, late)
	if err != nil {
		t.Fatal(err)
	}
	// Then the earlier one arrives and must not win.
	err = SetWindows(ctx, st.Queries, "claude", "work",
		[]v1.AccountWindow{{Name: "five_hour", UsedPercent: 20, ResetsAt: &earlyReset}}, early)
	if err != nil {
		t.Fatal(err)
	}

	got, err := Load(ctx, st.Queries, data, cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ws := got[0].Report().Windows
	if len(ws) != 1 {
		t.Fatalf("windows %+v, want one", ws)
	}
	if ws[0].UsedPercent != 80 {
		t.Errorf("five_hour used %v%%, want the later observation's 80%%", ws[0].UsedPercent)
	}
	if ws[0].ResetsAt == nil || !ws[0].ResetsAt.Equal(lateReset) {
		t.Errorf("five_hour resets %v, want the later observation's %s", ws[0].ResetsAt, lateReset)
	}

	// An observation at the same moment still writes: two turns can share a
	// millisecond, and refusing both would leave the window at neither.
	err = SetWindows(ctx, st.Queries, "claude", "work",
		[]v1.AccountWindow{{Name: "five_hour", UsedPercent: 91, ResetsAt: &lateReset}}, late)
	if err != nil {
		t.Fatal(err)
	}
	if got, err = Load(ctx, st.Queries, data, cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	if ws := got[0].Report().Windows; ws[0].UsedPercent != 91 {
		t.Errorf("five_hour used %v%%, want 91%% from the same-moment write", ws[0].UsedPercent)
	}
}
