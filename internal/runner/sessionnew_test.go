package runner

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

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
		{"cancelled after the answer acknowledging its claim was lost", func(t *testing.T, e *env, l *Loop) *Loop {
			// DEV-143. The hub binds s1 at the listing whose answer never
			// arrives, so it holds a as claimed while the runner waits.
			h := &losesAnswer{Hub: l.Hub}
			l.Hub = h
			mustSync(t, l)
			h.arm()
			if _, err := l.SyncOnce(ctx); err == nil {
				t.Fatal("the armed sync's answer arrived")
			}
			if _, err := e.api(t).Cancel(ctx, "a"); err != nil {
				t.Fatal(err)
			}
			// The cancel reaches the runner before any acknowledgement: it
			// withdraws the claim and the session the claim opened. The next
			// sync leaves a out, and the hub ends it cancelled there.
			if res := mustSync(t, l); !slices.Equal(cancels(res), []string{"a"}) {
				t.Fatalf("cancels %v, want [a]", cancels(res))
			}
			if _, err := e.store.GetSession(ctx, db.GetSessionParams{Connection: "hub", ID: "s1"}); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("session s1 is still here after the claim that opened it was withdrawn: %v", err)
			}
			return l
		}},
		{"lost while the answer acknowledging its claim was lost", func(t *testing.T, e *env, l *Loop) *Loop {
			return lapseUnacknowledged(t, e, l, false, "lost")
		}},
		{"cancelled on its lapse while the answer acknowledging its claim was lost", func(t *testing.T, e *env, l *Loop) *Loop {
			return lapseUnacknowledged(t, e, l, true, "cancelled")
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
	a := testRun("a", "s1")
	a.Sources = []v1.Source{{Path: "/work/project"}}
	e.enqueue(t, a, continued("b", "s1"))
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
	// a never prepared, so s1 has no sources recorded and b, naming none,
	// is the run that builds its workdir: from a's, which the session still
	// is, or it would build it empty.
	x := &Exec{Store: e.store}
	sources, record, err := x.sessionSources(ctx, Claim{Connection: "hub", Run: res.Runs[0]})
	if err != nil || !record || len(sources) != 1 || sources[0].Path != "/work/project" {
		t.Errorf("b is prepared from %+v (record %v, %v), want a's sources, recorded now", sources, record, err)
	}
	mustSync(t, l2)
	if got := l2.Executor.(*executor).ids(); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("started %v, want [b]", got)
	}
}

// lapseUnacknowledged is DEV-148. The hub binds s1 at the listing that claims
// a, whose answer never arrives, and the runner is then silent past the
// lease: the sweep ends a lost, or cancelled when a cancel was asked for
// meanwhile (decision 0061). Back, the runner lists a as claimed, which the
// hub no longer holds, so the answer is a cancel; the runner, never told its
// claim was acknowledged, withdraws it and the session it opened.
func lapseUnacknowledged(t *testing.T, e *env, l *Loop, cancel bool, want string) *Loop {
	t.Helper()
	ctx := context.Background()
	h := &losesAnswer{Hub: l.Hub}
	l.Hub = h
	mustSync(t, l)
	h.arm()
	if _, err := l.SyncOnce(ctx); err == nil {
		t.Fatal("the armed sync's answer arrived")
	}
	if cancel {
		if _, err := e.api(t).Cancel(ctx, "a"); err != nil {
			t.Fatal(err)
		}
	}
	e.skew.Store(int64(10 * time.Minute))
	res := mustSync(t, l)
	if !slices.Equal(cancels(res), []string{"a"}) {
		t.Fatalf("cancels %v, want [a]", cancels(res))
	}
	if got := e.hubState(t, "a"); got != want {
		t.Fatalf("a is %s on the hub, want %s", got, want)
	}
	if _, err := e.store.GetSession(ctx, db.GetSessionParams{Connection: "hub", ID: "s1"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("session s1 is still here after the claim that opened it was withdrawn: %v", err)
	}
	return l
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
