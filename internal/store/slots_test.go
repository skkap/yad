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
