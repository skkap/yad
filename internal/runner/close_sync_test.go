package runner

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	hubstore "github.com/skkap/yad/internal/hub/store"
	hubdb "github.com/skkap/yad/internal/hub/store/db"
	"github.com/skkap/yad/internal/store/db"
)

// collector gives the loop a collector over the env's store, on its clock and
// a disk that always has room.
func (e *env) collector(l *Loop) *Collector {
	c := &Collector{
		Store: e.store, Workdirs: filepath.Join(e.paths.Data, "workdirs"), Clock: e.clock,
		DiskFree: func(string) (int64, error) { return 42 << 30, nil },
	}
	l.Sessions = c
	return c
}

func hubSession(t *testing.T, e *env, id string) (closedAt sql.NullInt64, reason, requested bool) {
	t.Helper()
	s, err := e.hubStore.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s.ClosedAt, s.CloseReason.String == string(v1.SessionClosed), s.CloseRequestedAt.Valid
}

func controlsOf(res v1.SyncResponse, kind v1.ControlKind) []string {
	var ids []string
	for _, c := range res.Controls {
		if c.Kind == kind {
			ids = append(ids, c.SessionID+c.RunID)
		}
	}
	return ids
}

// The hub's close, end to end in process: yad hub repeats close_session until
// the runner reports the session closed; the runner closes it, reclaims the
// workdir, and refuses a continuation the hub had already queued; the hub
// records the close and refuses the next continuation at submit.
func TestTheHubClosesASession(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	c := e.collector(l)
	ctx := context.Background()
	x := e.executor(fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"}}))
	runOne(t, e, l, x, testRun("a", "s1"))
	workdir := session(t, e, "s1").Workdir
	if workdir == "" || gone(workdir) {
		t.Fatalf("no workdir after the first run: %q", workdir)
	}

	e.enqueue(t, continued("b", "s1"))
	if err := e.hubStore.RequestSessionClose(ctx, dbHubClose("s1")); err != nil {
		t.Fatal(err)
	}
	if err := e.hubStore.EnqueueRun(ctx, continued("c", "s1"), time.Now()); !errors.Is(err, hubstore.ErrSessionClosed) {
		t.Errorf("a continuation of a closing session was queued: %v", err)
	}

	res := mustSync(t, l)
	if got := controlsOf(res, v1.ControlCloseSession); !slices.Equal(got, []string{"s1"}) {
		t.Fatalf("close_session controls = %v", got)
	}
	if s := session(t, e, "s1"); s.State != "closed" || s.CloseReason.String != "closed" {
		t.Fatalf("runner session after the control = %+v", s)
	}
	if len(res.Runs) != 1 || res.Runs[0].RunID != "b" {
		t.Fatalf("offered %+v, want b", res.Runs)
	}
	l.sendRefusals(ctx)
	if r := hubResult(t, e, "b"); r.State != v1.RunFailed || r.Error == nil || r.Error.Class != ClassSessionClosed {
		t.Errorf("the queued continuation ended %+v (%+v), want failed with %s", r, r.Error, ClassSessionClosed)
	}

	if err := c.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if !gone(workdir) {
		t.Error("the workdir is still there after the sweep")
	}

	// The sync after the close carries it; the hub records it and stops
	// asking.
	res = mustSync(t, l)
	if got := controlsOf(res, v1.ControlCloseSession); len(got) != 0 {
		t.Errorf("the hub still asks to close %v after the runner reported it", got)
	}
	closedAt, byHub, requested := hubSession(t, e, "s1")
	if !closedAt.Valid || !byHub || requested {
		t.Errorf("hub session: closed %v, reason closed %v, still requested %v", closedAt, byHub, requested)
	}
	if s := session(t, e, "s1"); !s.ReportedAt.Valid {
		t.Errorf("the runner does not record the report as heard: %+v", s)
	}
	if err := e.hubStore.EnqueueRun(ctx, continued("d", "s1"), time.Now()); !errors.Is(err, hubstore.ErrSessionClosed) {
		t.Errorf("a continuation of a closed session was queued: %v", err)
	}
	if res := mustSync(t, l); len(res.Runs) != 0 {
		t.Errorf("offered %+v in a closed session", res.Runs)
	}
}

func dbHubClose(id string) hubdb.RequestSessionCloseParams {
	return hubdb.RequestSessionCloseParams{Now: sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true}, ID: id}
}

// A close the runner made on its own — here the idle TTL — is reported with
// its reason in every sync until one is answered, and once only after that.
func TestClosesAreReportedUntilHeard(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	hub := &scriptedHub{fail: errors.New("the hub is down")}
	l := e.loop(t, 1)
	l.Hub = hub
	c := e.collector(l)
	c.IdleTTL = 14 * day
	for _, id := range []string{"s1", "s2"} {
		if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: id, Harness: "claude", CreatedAt: 1,
			LastUsedAt: e.clock.Now().Add(-15 * day).UnixMilli()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Sweep(ctx); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if _, err := l.SyncOnce(ctx); err == nil {
			t.Fatal("a sync to a hub that is down succeeded")
		}
	}
	hub.fail = nil
	mustSync(t, l)
	mustSync(t, l)
	var sent [][]string
	for _, req := range hub.syncs {
		var ids []string
		for _, cs := range req.ClosedSessions {
			if cs.Reason != v1.SessionExpired || cs.ClosedAt.IsZero() {
				t.Errorf("reported %+v", cs)
			}
			ids = append(ids, cs.SessionID)
		}
		sent = append(sent, ids)
	}
	want := [][]string{{"s1", "s2"}, {"s1", "s2"}, {"s1", "s2"}, nil}
	if !slices.EqualFunc(sent, want, slices.Equal) {
		t.Errorf("closed_sessions per sync = %v, want %v", sent, want)
	}
}

// A close_session for a session this runner never held, or one it reported
// closed before, is answered with a report anyway, so the hub stops asking —
// and only once.
func TestCloseOfWhatIsGoneIsEchoed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	hub := &scriptedHub{}
	l := e.loop(t, 1)
	l.Hub = hub
	c := e.collector(l)
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "old", Harness: "claude", CreatedAt: 1, LastUsedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Close(ctx, "hub", "old", v1.SessionClosedByOwner); err != nil {
		t.Fatal(err)
	}
	mustSync(t, l) // reports "old" and has it heard

	hub.controls = []v1.Control{{Kind: v1.ControlCloseSession, SessionID: "old"}, {Kind: v1.ControlCloseSession, SessionID: "never"}}
	mustSync(t, l)
	hub.controls = nil
	mustSync(t, l)
	mustSync(t, l)

	got := map[string]v1.SessionCloseReason{}
	for _, cs := range hub.syncs[2].ClosedSessions {
		got[cs.SessionID] = cs.Reason
	}
	if got["old"] != v1.SessionClosedByOwner || got["never"] != v1.SessionClosed || len(got) != 2 {
		t.Errorf("echoed %v, want old closed_by_owner and never closed", got)
	}
	if n := len(hub.syncs[3].ClosedSessions); n != 0 {
		t.Errorf("echoed again after the hub heard: %+v", hub.syncs[3].ClosedSessions)
	}
}

// A session whose close waits on its run takes no new run meanwhile.
func TestAClosingSessionTakesNoRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	hub := &scriptedHub{}
	l := e.loop(t, 2)
	l.Hub = hub
	c := e.collector(l)
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "s1", Harness: "claude", CreatedAt: 1, LastUsedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateRun(ctx, db.CreateRunParams{Connection: "hub", ID: "a", SessionID: "s1", Harness: "claude", Spec: "{}", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if res, err := c.Close(ctx, "hub", "s1", v1.SessionClosed); err != nil || res.Outcome != CloseWaiting {
		t.Fatalf("close = %+v, %v", res, err)
	}
	if err := e.store.SetRunState(ctx, db.SetRunStateParams{State: "succeeded", UpdatedAt: 2, Connection: "hub", ID: "a"}); err != nil {
		t.Fatal(err)
	}
	hub.offer = []v1.Run{continued("b", "s1")}
	mustSync(t, l)
	l.sendRefusals(ctx)
	if r, ok := hub.results["b"]; !ok || r.Error == nil || r.Error.Class != ClassSessionClosed {
		t.Errorf("b was not refused as session_closed: %+v", r)
	}
	if _, err := e.store.GetRun(ctx, db.GetRunParams{Connection: "hub", ID: "b"}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("b was recorded: %v", err)
	}
}

// Health carries the free space under the workdirs.
func TestHealthReportsDiskFree(t *testing.T) {
	e := newEnv(t)
	hub := &scriptedHub{}
	l := e.loop(t, 1)
	l.Hub = hub
	e.collector(l)
	mustSync(t, l)
	if got := hub.syncs[0].Health.DiskFreeBytes; got != 42<<30 {
		t.Errorf("disk_free_bytes = %d", got)
	}
}
