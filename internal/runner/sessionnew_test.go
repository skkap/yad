package runner

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	hubdb "github.com/skkap/yad/internal/hub/store/db"
	"github.com/skkap/yad/internal/store/db"
)

// Through yad hub: a session whose first run never bound it opens with the
// next run (decision 0047). The hub sends that run as new, because no claim
// bound the session; the runner has no such session, because the first run's
// claim left none behind; so the run is taken, and starts. Before, the hub
// sent it as continuing and the runner refused it — and every later run in
// the session with it.
func TestASessionWhoseFirstRunNeverBoundItOpensWithTheNext(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// end takes the session's first run, a, from offered to ended
		// without a claim the hub acknowledged, and returns the loop that
		// goes on.
		end func(t *testing.T, e *env, l *Loop) *Loop
	}{
		{"refused", func(t *testing.T, e *env, l *Loop) *Loop {
			// Invalid as offered, so the runner refuses it at the claim.
			if _, err := e.hubStore.DB.Exec(`UPDATE runs SET spec = json_set(spec, '$.model', '') WHERE id = 'a'`); err != nil {
				t.Fatal(err)
			}
			mustSync(t, l)
			if got := e.hubState(t, "a"); got != "failed" {
				t.Fatalf("a is %s on the hub after the runner refused it", got)
			}
			return l
		}},
		{"cancelled on its claim", func(t *testing.T, e *env, l *Loop) *Loop {
			mustSync(t, l) // claimed, not yet listed
			cancelOnHub(t, e, "a")
			// The listing is answered with a cancel, and the claim withdrawn
			// with the session it opened.
			res := mustSync(t, l)
			if !slices.ContainsFunc(res.Controls, func(c v1.Control) bool { return c.Kind == v1.ControlCancel && c.RunID == "a" }) {
				t.Fatalf("the claim of a cancelled run was answered with %+v", res.Controls)
			}
			return l
		}},
		{"withdrawn", func(t *testing.T, e *env, l *Loop) *Loop {
			mustSync(t, l)
			// The connection stops with the claim pending, then the hub's
			// user cancels the run it still has as offered.
			l.WithdrawPending(ctx)
			cancelOnHub(t, e, "a")
			return restarted(l, e)
		}},
		{"its runner stopped before the claim was acknowledged", func(t *testing.T, e *env, l *Loop) *Loop {
			mustSync(t, l)
			// The process dies holding the claim; the next one withdraws it.
			l2 := restarted(l, e)
			if err := l2.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := e.store.GetSession(ctx, db.GetSessionParams{Connection: "hub", ID: "s1"}); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("session s1 is still here after the restart: %v; a claim the hub never acknowledged leaves no session", err)
			}
			cancelOnHub(t, e, "a")
			return l2
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			e.enqueue(t, testRun("a", "s1"), continued("b", "s1"))
			l = tc.end(t, e, l)

			// Offered as new, claimed, and at the next sync started.
			mustSync(t, l)
			mustSync(t, l)
			if got := l.Executor.(*executor).ids(); !slices.Equal(got, []string{"b"}) {
				t.Fatalf("started %v, want [b]: the run after a first run that never bound its session must open it", got)
			}
			if got := e.hubState(t, "b"); got != "claimed" {
				t.Errorf("b is %s on the hub", got)
			}
			if s := session(t, e, "s1"); s.State != "open" {
				t.Errorf("session s1 is %s", s.State)
			}
		})
	}
}

// Through yad hub: a claim the hub acknowledged that had not begun to prepare
// when its process died — waiting for its start_at, say — is reported lost at
// the next start, and its session stays. The hub bound the session at that
// acknowledgement and sends the next run in it as continuing, which the
// runner can take only if it kept the session. Before, the restart withdrew
// the claim and deleted the session, and every later run in it was refused.
func TestAnAcknowledgedClaimKeepsItsSessionAtRestart(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"), continued("b", "s1"))
	mustSync(t, l)
	mustSync(t, l)
	if got := e.exec.ids(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("started %v", got)
	}
	if r := localRun(t, e, "a"); r.State != string(v1.RunClaimed) || r.Acknowledged != 1 {
		t.Fatalf("a is %s, acknowledged %d: want a claim acknowledged and not yet preparing", r.State, r.Acknowledged)
	}

	l2 := restarted(l, e)
	if err := l2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if res, ok := outboxResult(t, e, "a"); !ok || res.State != v1.RunLost {
		t.Fatalf("a's result owed: %+v %v, want lost", res, ok)
	}
	if s := session(t, e, "s1"); s.State != "open" {
		t.Fatalf("session s1 is %s after the restart", s.State)
	}
	e.reporter(l2).Flush(ctx)
	if got := e.hubState(t, "a"); got != "lost" {
		t.Fatalf("a is %s on the hub after its lost result", got)
	}

	res := mustSync(t, l2)
	if len(res.Runs) != 1 || res.Runs[0].RunID != "b" || res.Runs[0].Session.New {
		t.Fatalf("offered %+v, want b continuing s1", res.Runs)
	}
	mustSync(t, l2)
	if got := l2.Executor.(*executor).ids(); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("started %v, want [b]", got)
	}
}

// A claim whose acknowledgement cannot be written does not start: started
// unrecorded, a restart before it prepared would withdraw the session the hub
// bound. It stays pending, is listed again, and starts once the write lands.
func TestAClaimStartsOnlyOnceItsAcknowledgementIsRecorded(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	mustSync(t, l)
	if _, err := e.store.DB.Exec(`CREATE TRIGGER no_ack BEFORE UPDATE OF acknowledged ON runs BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	mustSync(t, l)
	if got := e.exec.ids(); len(got) != 0 {
		t.Fatalf("started %v with its acknowledgement unrecorded", got)
	}
	if _, err := e.store.DB.Exec(`DROP TRIGGER no_ack`); err != nil {
		t.Fatal(err)
	}
	res := mustSync(t, l)
	if slices.ContainsFunc(res.Controls, func(c v1.Control) bool { return c.RunID == "a" }) {
		t.Fatalf("the claim listed again was answered with %+v", res.Controls)
	}
	if got := e.exec.ids(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("started %v, want [a] once the acknowledgement was recorded", got)
	}
	if r := localRun(t, e, "a"); r.Acknowledged != 1 {
		t.Errorf("a is acknowledged %d", r.Acknowledged)
	}
}

// restarted is the loop a new process of the same runner makes, with an
// executor of its own so what it starts is told apart from what the last
// process did.
func restarted(l *Loop, e *env) *Loop {
	return &Loop{
		Connection: l.Connection, RunnerID: l.RunnerID, Hub: l.Hub, Store: e.store, Pool: NewPool(v1.Capacity{Total: 1}),
		Capabilities: l.Capabilities, Executor: &executor{}, Clock: e.clock, Rand: l.Rand,
	}
}

// cancelOnHub is the hub's user cancelling a run no runner has started, as
// the service API does it.
func cancelOnHub(t *testing.T, e *env, runID string) {
	t.Helper()
	if _, err := e.hubStore.EndUnstartedRun(context.Background(), hubdb.EndUnstartedRunParams{
		State: string(v1.RunCancelled), Reason: sql.NullString{String: "cancelled on the hub before a runner started it", Valid: true}, ID: runID,
	}); err != nil {
		t.Fatal(err)
	}
}
