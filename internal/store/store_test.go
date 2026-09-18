package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/skkap/yad/internal/store/db"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, file
}

func seed(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateSession(ctx, db.CreateSessionParams{ID: "s1", Connection: "hub", Harness: "claude", Workdir: "/w", CreatedAt: 1, LastUsedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, db.CreateRunParams{ID: "r1", SessionID: "s1", Connection: "hub", Harness: "claude", Spec: "{}", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenIsIdempotentAndPrivate(t *testing.T) {
	s, file := open(t)
	s.Close()
	s2, err := Open(context.Background(), file)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	s2.Close()
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("state.db is %v, want 0600", fi.Mode().Perm())
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	s, file := open(t)
	if _, err := s.DB.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s2, err := Open(context.Background(), file); err == nil {
		s2.Close()
		t.Fatal("opened a database from the future")
	}
}

// (run, seq) is the protocol's idempotency key; the spool must refuse a
// duplicate rather than send it twice.
func TestDuplicateEventRejected(t *testing.T) {
	s, _ := open(t)
	seed(t, s)
	ctx := context.Background()
	if err := s.AppendEvent(ctx, db.AppendEventParams{Connection: "hub", RunID: "r1", Seq: 1, Body: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, db.AppendEventParams{Connection: "hub", RunID: "r1", Seq: 1, Body: "{}"}); err == nil {
		t.Error("duplicate (run, seq) accepted")
	}
	s.AppendEvent(ctx, db.AppendEventParams{Connection: "hub", RunID: "r1", Seq: 2, Body: "{}"})
	if err := s.AckEvents(ctx, db.AckEventsParams{Connection: "hub", RunID: "r1", AckedThrough: 1}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.UnackedEvents(ctx, db.UnackedEventsParams{Connection: "hub", RunID: "r1", Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Seq != 2 {
		t.Errorf("unacked after ack(1) = %+v, %v", rows, err)
	}
}

// Two live runs in one session corrupt its transcript; the database refuses it.
func TestOneLiveRunPerSession(t *testing.T) {
	s, _ := open(t)
	seed(t, s)
	ctx := context.Background()
	second := db.CreateRunParams{ID: "r2", SessionID: "s1", Connection: "hub", Harness: "claude", Spec: "{}", CreatedAt: 2, UpdatedAt: 2}
	if err := s.CreateRun(ctx, second); err == nil {
		t.Fatal("a second live run in one session was accepted")
	}
	if err := s.SetRunState(ctx, db.SetRunStateParams{State: "succeeded", UpdatedAt: 3, Connection: "hub", ID: "r1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(ctx, second); err != nil {
		t.Errorf("a run after the first finished was refused: %v", err)
	}
}

func TestOutboxKeepsFirstResult(t *testing.T) {
	s, _ := open(t)
	seed(t, s)
	ctx := context.Background()
	s.PutOutbox(ctx, db.PutOutboxParams{RunID: "r1", Connection: "hub", Body: "first", NextAttemptAt: 0})
	s.PutOutbox(ctx, db.PutOutboxParams{RunID: "r1", Connection: "hub", Body: "second", NextAttemptAt: 0})
	due, err := s.DueOutbox(ctx, db.DueOutboxParams{Connection: "hub", NextAttemptAt: 1})
	if err != nil || len(due) != 1 || due[0].Body != "first" {
		t.Errorf("outbox = %+v, %v", due, err)
	}
}

// Hubs choose session and run ids without coordinating, so two connections may
// use the same ids. Each keeps its own rows, and neither can reach the other's.
func TestConnectionsDoNotShareIDs(t *testing.T) {
	s, _ := open(t)
	seed(t, s) // hub: s1, r1
	ctx := context.Background()
	if err := s.CreateSession(ctx, db.CreateSessionParams{ID: "s1", Connection: "other", Harness: "codex", Workdir: "/o", CreatedAt: 1, LastUsedAt: 1}); err != nil {
		t.Fatalf("same session id on another connection refused: %v", err)
	}
	if err := s.CreateRun(ctx, db.CreateRunParams{ID: "r1", SessionID: "s1", Connection: "other", Harness: "codex", Spec: "{}", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatalf("same run id on another connection refused, or its session's live run blocked it: %v", err)
	}
	if err := s.AppendEvent(ctx, db.AppendEventParams{Connection: "hub", RunID: "r1", Seq: 1, Body: "hub"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent(ctx, db.AppendEventParams{Connection: "other", RunID: "r1", Seq: 1, Body: "other"}); err != nil {
		t.Fatalf("same (run, seq) on another connection refused: %v", err)
	}
	got, err := s.GetSession(ctx, db.GetSessionParams{Connection: "other", ID: "s1"})
	if err != nil || got.Harness != "codex" {
		t.Errorf("other's session = %+v, %v", got, err)
	}
	rows, _ := s.UnackedEvents(ctx, db.UnackedEventsParams{Connection: "hub", RunID: "r1", Limit: 10})
	if len(rows) != 1 || rows[0].Body != "hub" {
		t.Errorf("hub's spool = %+v, want only its own event", rows)
	}
	// A run naming a session that exists only on another connection is refused.
	if err := s.CreateRun(ctx, db.CreateRunParams{ID: "r9", SessionID: "s1", Connection: "third", Harness: "claude", Spec: "{}", CreatedAt: 1, UpdatedAt: 1}); err == nil {
		t.Error("a run on one connection attached to another connection's session")
	}
}
