package account

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/store"
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
	if err := SetLimit(ctx, st.Queries, "claude", "work", past, time.Now()); err != nil {
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

	got, err := Load(ctx, st.Queries, data, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d accounts, want 1", len(got))
	}
	if got[0].State != v1.AccountFree {
		t.Errorf("Load says %q, want free — the reset has passed", got[0].State)
	}
	if _, ok := First(got, "claude"); !ok {
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
	got, err := Load(ctx, st.Queries, data, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != v1.AccountLimited {
		t.Fatalf("Load says %q, want limited", got[0].State)
	}
	if got[0].LimitedUntil == nil || !got[0].LimitedUntil.Equal(reset) {
		t.Errorf("limited until %v, want %s", got[0].LimitedUntil, reset)
	}
	if _, ok := First(got, "claude"); ok {
		t.Error("a limited account was offered a run")
	}
	rep := got[0].Report()
	if rep.State != v1.AccountLimited || rep.LimitedUntil == nil || !rep.LimitedUntil.Equal(reset) {
		t.Errorf("report %+v, want limited until %s", rep, reset)
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
	got, err := Load(ctx, st.Queries, data, cfg)
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
	got, err := Load(ctx, st.Queries, data, cfg)
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
	got, err := Load(ctx, st.Queries, data, cfg)
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
	got, err := Load(ctx, st.Queries, data, cfg)
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
func TestRefillAtTakesTheSoonestFullWindow(t *testing.T) {
	soon := time.Date(2026, 9, 20, 15, 0, 0, 0, time.UTC)
	later := time.Date(2026, 9, 23, 15, 0, 0, 0, time.UTC)
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
			[][]v1.AccountWindow{{{Name: "five_hour", UsedPercent: 62, ResetsAt: &soon}}}, time.Time{}},
		{"a full window the harness did not date",
			[][]v1.AccountWindow{{full("five_hour", nil)}}, time.Time{}},
		{"nothing at all", nil, time.Time{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := RefillAt(c.sets...)
			if !got.Equal(c.want) {
				t.Errorf("RefillAt = %s, want %s", got, c.want)
			}
		})
	}
}
