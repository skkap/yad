package store

import (
	"context"
	"testing"

	"github.com/skkap/yad/internal/store/db"
)

// Slots are the smallest free per repository, kept by their session, and
// recycled once it lets them go.
func TestSlots(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := s.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: id, Harness: "claude", CreatedAt: 1, LastUsedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	slot := func(repo, session string) int64 {
		t.Helper()
		n, err := s.Slot(ctx, repo, "hub", session)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	steps := []struct {
		repo, session string
		want          int64
	}{
		{"r", "a", 1},
		{"r", "b", 2},
		{"r", "a", 1}, // kept by its session
		{"other", "c", 1},
		{"r", "c", 3},
	}
	for _, st := range steps {
		if got := slot(st.repo, st.session); got != st.want {
			t.Errorf("slot(%s, %s) = %d, want %d", st.repo, st.session, got, st.want)
		}
	}
	if err := s.ReleaseSlots(ctx, "hub", "b"); err != nil {
		t.Fatal(err)
	}
	if got := slot("r", "d"); got != 2 {
		t.Errorf("after b released its slot, d got %d; want 2, the smallest free", got)
	}
	if err := s.ReleaseSlots(ctx, "hub", "c"); err != nil {
		t.Fatal(err)
	}
	if got := slot("other", "d"); got != 1 {
		t.Errorf("c's slot in other was not recycled: d got %d", got)
	}
}

// HeldSlot reads a session's slot and never takes one: a teardown asking for
// a slot its session never held must not be handed a fresh one.
func TestHeldSlot(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if err := s.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "a", Harness: "claude", CreatedAt: 1, LastUsedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := s.HeldSlot(ctx, "r", "hub", "a"); err != nil || ok || n != 0 {
		t.Errorf("before any slot: %d, %v, %v; want none", n, ok, err)
	}
	if used, _ := s.SlotsInUse(ctx, "r"); len(used) != 0 {
		t.Errorf("HeldSlot allocated %v", used)
	}
	if _, err := s.Slot(ctx, "r", "hub", "a"); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := s.HeldSlot(ctx, "r", "hub", "a"); err != nil || !ok || n != 1 {
		t.Errorf("after Slot: %d, %v, %v; want 1", n, ok, err)
	}
	if _, ok, _ := s.HeldSlot(ctx, "other", "hub", "a"); ok {
		t.Error("a slot in r was reported held in another repository")
	}
}
