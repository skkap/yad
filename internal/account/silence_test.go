package account

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

func silenceStore(t *testing.T) *db.Queries {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st.Queries
}

// Three silent runs in a row leave a home alone for a quarter of an hour, each
// one after that for twice as long as the last, and never for more than four
// hours: a not-ready home gets no run to prove itself with, so only the clock
// can bring it back, and it always does (decision 0077).
func TestTheCoolDownStartsAtTheThirdAndDoublesToItsCap(t *testing.T) {
	q := silenceStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for i, want := range []time.Duration{0, 0, 15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour} {
		s, err := NoteSilent(ctx, q, "claude", "", now)
		if err != nil {
			t.Fatal(err)
		}
		if s.Runs != i+1 {
			t.Fatalf("run %d counted as %d", i+1, s.Runs)
		}
		got := time.Duration(0)
		if !s.Until.IsZero() {
			got = s.Until.Sub(now)
		}
		if got != want {
			t.Errorf("after %d silent runs the cool-down is %s, want %s", s.Runs, got, want)
		}
		if s.Holds(now) != (want > 0) || s.Holds(now.Add(want)) {
			t.Errorf("after %d silent runs, holds now %v and at its end %v", s.Runs, s.Holds(now), s.Holds(now.Add(want)))
		}
	}
}

// A turn that says anything clears the record, so the next three are counted
// from nothing; and a daemon start ends the cool-down but keeps the count, so
// a home still wedged trips again on its very next silent run.
func TestARunThatSpeaksClearsItAndAStartEndsTheCoolDown(t *testing.T) {
	q := silenceStore(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	for range 3 {
		if _, err := NoteSilent(ctx, q, "claude", "work", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := EndCoolDowns(ctx, q, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	all, err := LoadSilences(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	s := all[Ref{"claude", "work"}]
	if s.Runs != 3 || s.Holds(now.Add(time.Minute)) {
		t.Fatalf("after a start: %+v, want the count kept and the cool-down over", s)
	}
	if s, err = NoteSilent(ctx, q, "claude", "work", now.Add(2*time.Minute)); err != nil || !s.Holds(now.Add(2*time.Minute)) {
		t.Fatalf("one more silent run after a start: %+v, %v; want not ready at once", s, err)
	}
	if err := NoteSpoke(ctx, q, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if all, _ := LoadSilences(ctx, q); len(all) != 0 {
		t.Errorf("a run that spoke left %+v", all)
	}
	if s, _ := NoteSilent(ctx, q, "claude", "work", now); s.Runs != 1 {
		t.Errorf("counted from %d after a run that spoke, want 1", s.Runs)
	}
}

// An account's record is what Load reads it by, and what Soonest skips it for;
// removing the account forgets the record, and a default login's record is no
// account's to prune.
func TestASilentAccountIsSkippedAndForgottenWithTheAccount(t *testing.T) {
	q := silenceStore(t)
	ctx := context.Background()
	data := t.TempDir()
	for _, l := range []string{"work", "spare"} {
		if _, err := Ensure(data, "claude", l); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	for range SilentRuns {
		for _, l := range []string{"work", ""} {
			if _, err := NoteSilent(ctx, q, "claude", l, now); err != nil {
				t.Fatal(err)
			}
		}
	}
	lists := Lists{"claude": {"work", "spare"}}
	accounts, err := Load(ctx, q, data, lists, now)
	if err != nil {
		t.Fatal(err)
	}
	if accounts[0].SilentUntil == nil || accounts[0].SilentRuns != SilentRuns || accounts[1].SilentUntil != nil {
		t.Fatalf("accounts = %+v", accounts)
	}
	if r := accounts[0].Report(); r.State != v1.AccountFree || r.SilentUntil == nil {
		t.Errorf("report = %+v, want free with silent_until", r)
	}
	if a, ok := Soonest(accounts, "claude", now); !ok || a.Label != "spare" {
		t.Errorf("Soonest = %q, %v; want spare", a.Label, ok)
	}
	// Past its cool-down it is free to choose again.
	later, err := Load(ctx, q, data, lists, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if later[0].SilentUntil != nil {
		t.Errorf("an hour on, %+v is still silent", later[0])
	}
	if _, err := Prune(ctx, q, Lists{"claude": {"spare"}}); err != nil {
		t.Fatal(err)
	}
	all, err := LoadSilences(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := all[Ref{"claude", "work"}]; ok {
		t.Error("the removed account's record survived the prune")
	}
	if _, ok := all[Ref{Harness: "claude"}]; !ok {
		t.Error("the prune took the default login's record")
	}
}
