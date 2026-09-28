package hub

import (
	"context"
	"net/http"
	"slices"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// session.new is decided when a run is offered: true while no claim has bound
// its session, false after (decision 0047). A session whose first run never
// bound it — refused, cancelled on its claim, withdrawn — exists on no runner,
// so the run after it goes out new; once a claim binds the session, every
// later run goes out continuing it. A run opening the session that named no
// sources carries the ones its session was created with; a continuing one is
// sent as submitted, since the runner holds its session's sources.
func TestSessionNewIsDecidedAtOffer(t *testing.T) {
	src := []v1.Source{{Path: "/work/project"}}
	for _, tc := range []struct {
		name string
		// end ends run a, the session's first, having been offered to r1.
		end     func(t *testing.T, f *fixture, cred string)
		wantNew bool
	}{
		{"first run refused", func(t *testing.T, f *fixture, cred string) {
			refusal := v1.Result{State: v1.RunFailed, Error: &v1.RunError{Class: "refused", Message: "cannot drive it"}}
			if code, env := f.result(t, cred, "a", refusal); code != http.StatusOK {
				t.Fatalf("refusal: %d %+v", code, env.Error)
			}
		}, true},
		{"first run cancelled on its claim", func(t *testing.T, f *fixture, cred string) {
			if _, err := f.hub.control(context.Background(), "a", v1.ControlCancel, ""); err != nil {
				t.Fatal(err)
			}
			// The runner's claim arrives after the cancel and is answered
			// with one: nothing bound the session.
			res := f.mustSync(t, "r1", cred, req("r1", 0, claimed("a")...))
			if got := cancels(res); len(got) != 1 || got[0] != "a" {
				t.Fatalf("cancels %v, want [a]", got)
			}
		}, true},
		{"first run withdrawn, then cancelled while queued", func(t *testing.T, f *fixture, cred string) {
			f.mustSync(t, "r1", cred, req("r1", 0))
			if f.state(t, "a") != "queued" {
				t.Fatalf("a is %s after a sync left it out", f.state(t, "a"))
			}
			if _, err := f.hub.control(context.Background(), "a", v1.ControlCancel, ""); err != nil {
				t.Fatal(err)
			}
		}, true},
		{"first run claimed, then finished", func(t *testing.T, f *fixture, cred string) {
			f.mustSync(t, "r1", cred, req("r1", 0, claimed("a")...))
			if code, env := f.result(t, cred, "a", v1.Result{State: v1.RunSucceeded}); code != http.StatusOK {
				t.Fatalf("result: %d %+v", code, env.Error)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			cred := f.register(t, "r1")
			a := run("a", "s1")
			a.Sources = src
			b := run("b", "s1")
			b.Session.New = false
			f.enqueue(t, a, b)
			res := f.mustSync(t, "r1", cred, first("r1", 1))
			if len(res.Runs) != 1 || res.Runs[0].RunID != "a" || !res.Runs[0].Session.New {
				t.Fatalf("offered %+v, want a opening s1", res.Runs)
			}
			tc.end(t, f, cred)

			res = f.mustSync(t, "r1", cred, req("r1", 1))
			if len(res.Runs) != 1 || res.Runs[0].RunID != "b" {
				t.Fatalf("offered %v, want b", ids(res.Runs))
			}
			got := res.Runs[0]
			if got.Session.New != tc.wantNew {
				t.Errorf("b went out with session.new %v, want %v", got.Session.New, tc.wantNew)
			}
			carries := len(got.Sources) == 1 && got.Sources[0].Path == src[0].Path
			switch {
			case tc.wantNew && !carries:
				t.Errorf("b opens s1 with sources %+v, want the ones s1 was created with, %+v", got.Sources, src)
			case !tc.wantNew && len(got.Sources) != 0:
				t.Errorf("b continues s1 with sources %+v, want none, as submitted", got.Sources)
			}
		})
	}
}

// A continuing run is sent with the sources it was submitted with. A run
// between naming others — which the runner refuses — changes nothing for the
// runs after it.
func TestAContinuingRunIsSentAsSubmitted(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	stray := run("c", "s1")
	stray.Session.New = false
	stray.Sources = []v1.Source{{Path: "/elsewhere"}}
	next := run("b", "s1")
	next.Session.New = false
	f.enqueue(t, run("a", "s1"), stray, next)
	offered := map[string]v1.Run{}
	free := first("r1", 1)
	for range 3 {
		res := f.mustSync(t, "r1", cred, free)
		if len(res.Runs) != 1 {
			t.Fatalf("offered %v", ids(res.Runs))
		}
		r := res.Runs[0]
		offered[r.RunID] = r
		// Each is claimed and finishes, so the next in the session follows.
		f.mustSync(t, "r1", cred, req("r1", 0, claimed(r.RunID)...))
		if code, env := f.result(t, cred, r.RunID, v1.Result{State: v1.RunSucceeded}); code != http.StatusOK {
			t.Fatalf("result for %s: %d %+v", r.RunID, code, env.Error)
		}
		free = req("r1", 1)
	}
	if got := offered["b"].Sources; len(got) != 0 {
		t.Errorf("b went out with sources %+v, and was submitted with none", got)
	}
	if got := offered["c"].Sources; len(got) != 1 || got[0].Path != "/elsewhere" {
		t.Errorf("c went out with sources %+v, want its own", got)
	}
}

// A cancelled claim the runner withdrew takes the session it opened with it on
// the runner (decision 0061), so the session goes back to unbound when that
// claim is the one that bound it, and its next run goes out opening it
// (DEV-143). Before, the session stayed bound, and the next run went out
// continuing a session no runner held, to be refused. Nothing else unbinds a
// session: not a withdrawn claim after one that ran, since the runner keeps a
// session with a transcript (DEV-77); not a lapsed lease, which cannot tell a
// withdrawal from silence; not a departed runner, whose sessions close; and
// not a session with a close asked for, which the runner closes rather than
// deletes and the hub waits to hear closed.
func TestAWithdrawnClaimUnbindsTheSessionItBoundAlone(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		// arrange takes the session's runs to the one r1 holds as claimed,
		// and returns it.
		arrange func(t *testing.T, f *fixture, cred string) string
		// end follows the answer that carries the cancel.
		end func(t *testing.T, f *fixture, cred string)
		// want is s1 at the end: unbound, bound (to r1, by a) or closed.
		want string
	}{
		{"the claim that bound it, left out of the next sync", claimFirst, leaveOut, "unbound"},
		// The runner's owner closed s1 while the claim was held: the runner
		// closes it rather than deleting it, and reports the close in the
		// same sync, which is believed from the runner s1 was offered to.
		{"the claim that bound it, left out beside the close of its session", claimFirst, func(t *testing.T, f *fixture, cred string) {
			report := req("r1", 0)
			report.ClosedSessions = []v1.ClosedSession{{SessionID: "s1", Reason: v1.SessionClosedByOwner, ClosedAt: f.clock.Now()}}
			f.mustSync(t, "r1", cred, report)
		}, "closed"},
		{"a later claim, after one that ran", func(t *testing.T, f *fixture, cred string) string {
			f.mustSync(t, "r1", cred, req("r1", 0, claimed("a")...))
			if code, env := f.result(t, cred, "a", v1.Result{State: v1.RunSucceeded}); code != http.StatusOK {
				t.Fatalf("result: %d %+v", code, env.Error)
			}
			if res := f.mustSync(t, "r1", cred, req("r1", 1)); !slices.Equal(ids(res.Runs), []string{"b"}) || res.Runs[0].Session.New {
				t.Fatalf("offered %+v, want b continuing s1", res.Runs)
			}
			f.mustSync(t, "r1", cred, req("r1", 0, claimed("b")...))
			return "b"
		}, leaveOut, "bound"},
		{"the claim that bound it, with a close asked for", func(t *testing.T, f *fixture, cred string) string {
			claimFirst(t, f, cred)
			if code, e := f.api(t, "POST", "/sessions/s1/close", f.admin(t, "closer"), nil, nil); code != http.StatusOK {
				t.Fatalf("close: %d %+v", code, e)
			}
			return "a"
		}, leaveOut, "bound"},
		{"the claim that bound it, its lease lapsing", claimFirst, func(t *testing.T, f *fixture, cred string) {
			f.clock.Advance(4 * DefaultSyncInterval)
			if err := f.hub.Sweep(ctx); err != nil {
				t.Fatal(err)
			}
		}, "bound"},
		{"the claim that bound it, its runner deregistering", claimFirst, func(t *testing.T, f *fixture, cred string) {
			if code, env := f.deregister(t, "r1", cred, ""); code != http.StatusOK {
				t.Fatalf("deregister: %d %+v", code, env)
			}
		}, "bound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			cred := f.register(t, "r1")
			b := run("b", "s1")
			b.Session.New = false
			f.enqueue(t, run("a", "s1"), b)
			f.mustSync(t, "r1", cred, first("r1", 1))
			held := tc.arrange(t, f, cred)
			if code, e := f.api(t, "POST", "/runs/"+held+"/cancel", f.admin(t, "cli"), nil, nil); code != http.StatusOK {
				t.Fatalf("cancel: %d %s", code, e.Message)
			}
			if res := f.mustSync(t, "r1", cred, req("r1", 0, claimed(held)...)); !slices.Equal(cancels(res), []string{held}) {
				t.Fatalf("cancels %v, want [%s]", cancels(res), held)
			}
			tc.end(t, f, cred)
			if got := f.state(t, held); got != "cancelled" {
				t.Fatalf("%s is %s, want cancelled", held, got)
			}

			s, err := f.store.GetSession(ctx, "s1")
			if err != nil {
				t.Fatal(err)
			}
			switch tc.want {
			case "unbound":
				if s.RunnerID.Valid || s.BoundByRun.Valid {
					t.Fatalf("s1 is bound to %q by %q, want unbound", s.RunnerID.String, s.BoundByRun.String)
				}
				res := f.mustSync(t, "r1", cred, req("r1", 1))
				if !slices.Equal(ids(res.Runs), []string{"b"}) || !res.Runs[0].Session.New {
					t.Errorf("offered %+v, want b opening s1", res.Runs)
				}
			case "closed":
				if !s.ClosedAt.Valid || s.RunnerID.String != "r1" {
					t.Errorf("s1 is closed %v, bound to %q; want closed on r1", s.ClosedAt.Valid, s.RunnerID.String)
				}
				if got := f.state(t, "b"); got != "failed" {
					t.Errorf("b is %s, want failed with its session's close", got)
				}
			default:
				if s.RunnerID.String != "r1" || s.BoundByRun.String != "a" {
					t.Errorf("s1 is bound to %q by %q, want r1 by a", s.RunnerID.String, s.BoundByRun.String)
				}
			}
		})
	}
}

// claimFirst has r1 claim a, the session's first run: the listing binds s1.
func claimFirst(t *testing.T, f *fixture, cred string) string {
	f.mustSync(t, "r1", cred, req("r1", 0, claimed("a")...))
	return "a"
}

// leaveOut is the sync after a withdrawal: the runner no longer lists the
// claim.
func leaveOut(t *testing.T, f *fixture, cred string) {
	if res := f.mustSync(t, "r1", cred, req("r1", 0)); len(cancels(res)) != 0 {
		t.Errorf("cancels %v for a run the runner no longer lists", cancels(res))
	}
}
