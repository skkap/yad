package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

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

// `yad sessions` reads beside a running daemon: it sees what the daemon
// wrote, cannot write, and never creates or migrates the file.
func TestOpenReadOnly(t *testing.T) {
	ctx := context.Background()
	missing := filepath.Join(t.TempDir(), "state.db")
	if _, err := OpenReadOnly(ctx, missing); err != ErrNoState {
		t.Errorf("a missing database: %v, want ErrNoState", err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("OpenReadOnly created %s", missing)
	}

	s, file := open(t)
	seed(t, s) // s1 with r1 live
	if err := s.CreateSession(ctx, db.CreateSessionParams{ID: "s2", Connection: "hub", Harness: "claude", Workdir: "/w2", CreatedAt: 1, LastUsedAt: 5}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"r2", "r3"} {
		if err := s.CreateRun(ctx, db.CreateRunParams{ID: r, SessionID: "s2", Connection: "hub", Harness: "claude", Spec: "{}", CreatedAt: 2, UpdatedAt: 2}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetRunState(ctx, db.SetRunStateParams{State: "succeeded", UpdatedAt: 3, Connection: "hub", ID: r}); err != nil {
			t.Fatal(err)
		}
	}
	ro, err := OpenReadOnly(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	got, err := ro.ListSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "s2" || got[0].Runs != 2 || got[0].LiveRun != "" ||
		got[1].ID != "s1" || got[1].Runs != 1 || got[1].LiveRun != "r1" {
		t.Errorf("sessions = %+v; want s2 (used last, 2 runs, none live) then s1 (r1 live)", got)
	}
	if err := ro.TouchSession(ctx, db.TouchSessionParams{LastUsedAt: 9, Connection: "hub", ID: "s1"}); err == nil {
		t.Error("a read-only open wrote")
	}

	for _, v := range []int{0, 999} {
		if _, err := s.DB.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
			t.Fatal(err)
		}
		if ro, err := OpenReadOnly(ctx, file); err == nil {
			ro.Close()
			t.Errorf("opened a database at schema %d", v)
		}
	}
}

// Migrations are numbered in the order they merge, with no gap and no number
// used twice: 0002_session_sources (git sources) merged before
// 0003_session_collection, and a file numbered below the latest would be
// skipped by every database already past it.
func TestMigrationNumbering(t *testing.T) {
	if err := CheckNumbering(migrations); err != nil {
		t.Error(err)
	}
	for _, tc := range []struct {
		name    string
		files   []string
		retired []int
		ok      bool
	}{
		{"contiguous", []string{"0001_a.sql", "0002_b.sql"}, nil, true},
		{"a gap", []string{"0001_a.sql", "0003_b.sql"}, nil, false},
		{"a retired gap", []string{"0001_a.sql", "0003_b.sql"}, []int{2}, true},
		{"a retired number used", []string{"0001_a.sql", "0002_b.sql", "0003_c.sql"}, []int{2}, false},
		{"a number taken twice", []string{"0001_a.sql", "0002_b.sql", "0002_c.sql"}, nil, false},
	} {
		fsys := fstest.MapFS{}
		for _, f := range tc.files {
			fsys["migrations/"+f] = &fstest.MapFile{Data: []byte("SELECT 1;")}
		}
		if err := CheckNumbering(fsys, tc.retired...); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// SQLite creates the -wal and -shm sidecars with the database's own mode, so a
// database that has drifted to 0644 hands the same bits to the file holding
// everything not yet checkpointed — which for the hub's store is a run's
// grants. config.Exposures reports the sidecars for that reason, and this is
// where the reason is measured rather than asserted. A clean close removes
// them, so the window is exactly while a runner is up.
func TestSidecarsTakeTheDatabaseMode(t *testing.T) {
	s, file := open(t)
	s.Close()
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(context.Background(), file)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	for _, ext := range []string{"-wal", "-shm"} {
		fi, err := os.Stat(file + ext)
		if err != nil {
			t.Fatalf("no %s sidecar: %v", ext, err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("%s is %v; if SQLite has stopped copying the database's mode, config.Exposures no longer needs to report it", ext, fi.Mode().Perm())
		}
	}
}
