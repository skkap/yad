package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub/store/db"
)

var t0 = time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "hub.db")
	s, err := Open(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, file
}

func run(id, session string) v1.Run {
	return v1.Run{RunID: id, Session: v1.SessionRef{ID: session, New: true}, Harness: "claude", Model: "opus", Brief: v1.Brief{Instruction: "x"}}
}

func seedRunner(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.UpsertRunner(context.Background(), db.UpsertRunnerParams{ID: id, Name: id, CredentialHash: "h-" + id, Capabilities: "{}", RegisteredAt: 1}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenIsPrivate(t *testing.T) {
	_, file := open(t)
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("hub.db is %v, want 0600", fi.Mode().Perm())
	}
}

// A token burns once, and only while it is alive.
func TestTokenSingleUse(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if err := s.CreateRegistrationToken(ctx, db.CreateRegistrationTokenParams{Hash: "live", CreatedAt: 0, ExpiresAt: 100}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRegistrationToken(ctx, db.CreateRegistrationTokenParams{Hash: "old", CreatedAt: 0, ExpiresAt: 10}); err != nil {
		t.Fatal(err)
	}
	burn := func(hash string, now int64) int64 {
		n, err := s.BurnRegistrationToken(ctx, db.BurnRegistrationTokenParams{Now: sql.NullInt64{Int64: now, Valid: true}, RunnerID: sql.NullString{String: "r", Valid: true}, Hash: hash})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, tc := range []struct {
		name, hash string
		now, want  int64
	}{
		{"first use", "live", 50, 1},
		{"second use", "live", 51, 0},
		{"expired", "old", 10, 0},
		{"unknown", "nope", 1, 0},
	} {
		if got := burn(tc.hash, tc.now); got != tc.want {
			t.Errorf("%s: burned %d rows, want %d", tc.name, got, tc.want)
		}
	}
}

// (run, seq) is the protocol's idempotency key: a resent event is absorbed,
// and the first body is the one kept.
func TestEventsUniqueByRunAndSeq(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if err := s.EnqueueRun(ctx, run("r1", "s1"), t0); err != nil {
		t.Fatal(err)
	}
	for i, body := range []string{"first", "resent"} {
		n, err := s.AppendEvent(ctx, db.AppendEventParams{RunID: "r1", Seq: 1, Body: body, ReceivedAt: 1})
		if err != nil {
			t.Fatal(err)
		}
		if want := int64(1 - i); n != want {
			t.Errorf("append %q stored %d rows, want %d", body, n, want)
		}
	}
	rows, err := s.EventsAfter(ctx, db.EventsAfterParams{RunID: "r1", Seq: 0, Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Body != "first" {
		t.Errorf("events = %+v, %v", rows, err)
	}
	if _, err := s.AppendEvent(ctx, db.AppendEventParams{RunID: "ghost", Seq: 1, Body: "x", ReceivedAt: 1}); err == nil {
		t.Error("an event for a run the hub never had was stored")
	}
}

func TestOneResultPerRun(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if err := s.EnqueueRun(ctx, run("r1", "s1"), t0); err != nil {
		t.Fatal(err)
	}
	for i, state := range []string{"succeeded", "failed"} {
		n, err := s.PutResult(ctx, db.PutResultParams{RunID: "r1", State: state, Body: "{}", ReceivedAt: 1})
		if err != nil || n != int64(1-i) {
			t.Errorf("result %s: %d rows, %v", state, n, err)
		}
	}
	if r, _ := s.GetResult(ctx, "r1"); r.State != "succeeded" {
		t.Errorf("kept %s, want the first", r.State)
	}
}

// Leases lapse exactly at their expiry: offers go back in the queue, held runs
// are lost, and nothing else moves.
func TestLeaseExpiryQueries(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	seedRunner(t, s, "rn")
	me := sql.NullString{String: "rn", Valid: true}
	for _, id := range []string{"offered-lapsed", "offered-live", "held-lapsed", "held-live", "queued", "done"} {
		if err := s.EnqueueRun(ctx, run(id, "s-"+id), t0); err != nil {
			t.Fatal(err)
		}
	}
	set := func(id, state string, lease int64) {
		if _, err := s.DB.Exec(`UPDATE runs SET state = ?, runner_id = ?, lease_expires_at = ? WHERE id = ?`, state, me, lease, id); err != nil {
			t.Fatal(err)
		}
	}
	set("offered-lapsed", "offered", 100)
	set("offered-live", "offered", 101)
	set("held-lapsed", "running", 100)
	set("held-live", "waiting", 101)
	set("done", "succeeded", 1)

	if n, err := s.RequeueWithdrawnOffers(ctx, 100); err != nil || n != 1 {
		t.Errorf("requeued %d, %v", n, err)
	}
	if n, err := s.LoseLapsedRuns(ctx, 100); err != nil || n != 1 {
		t.Errorf("lost %d, %v", n, err)
	}
	for id, want := range map[string]string{
		"offered-lapsed": "queued", "offered-live": "offered",
		"held-lapsed": "lost", "held-live": "waiting",
		"queued": "queued", "done": "succeeded",
	} {
		r, err := s.GetRun(ctx, id)
		if err != nil || r.State != want {
			t.Errorf("%s = %s (%v), want %s", id, r.State, err, want)
		}
		if id == "offered-lapsed" && r.RunnerID.Valid {
			t.Error("a withdrawn offer still names its runner")
		}
	}
}

func TestEnqueueRefusesAnotherHarnessInOneSession(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if err := s.EnqueueRun(ctx, run("r1", "s1"), t0); err != nil {
		t.Fatal(err)
	}
	other := run("r2", "s1")
	other.Harness = "codex"
	if err := s.EnqueueRun(ctx, other, t0); err == nil {
		t.Error("a codex run joined a claude session")
	}
	if err := s.EnqueueRun(ctx, v1.Run{RunID: "bad"}, t0); err == nil {
		t.Error("an invalid run was queued")
	}
}

// The session flag on a run is what the runner acts on — create or resume —
// so the store holds it to the truth instead of trusting it.
func TestEnqueueHoldsTheSessionFlagToTheTruth(t *testing.T) {
	cont := func(id, session string) v1.Run {
		r := run(id, session)
		r.Session.New = false
		return r
	}
	for _, tc := range []struct {
		name string
		runs []v1.Run
		want error
	}{
		{"new session", []v1.Run{run("r1", "s1")}, nil},
		{"continue a session", []v1.Run{run("r1", "s1"), cont("r2", "s1")}, nil},
		{"start a session twice", []v1.Run{run("r1", "s1"), run("r2", "s1")}, ErrSessionExists},
		{"continue no session", []v1.Run{cont("r1", "s1")}, ErrNoSession},
		{"same run id twice", []v1.Run{run("r1", "s1"), cont("r1", "s1")}, ErrRunExists},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := open(t)
			var err error
			for _, r := range tc.runs {
				if err = s.EnqueueRun(context.Background(), r, t0); err != nil {
					break
				}
			}
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}
