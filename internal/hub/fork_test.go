package hub

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/protocol/hubapi"
)

func forkRun(id, session, from string) v1.Run {
	r := run(id, session)
	r.Session.ForkFrom = from
	return r
}

// A fork goes to the runner holding the session it forks and to no other,
// and only while that runner advertises fork — for as long as that takes,
// since it can go nowhere else (decision 0065). The offer opens the session
// and names the one forked. (That the fork's next run continues it naming
// none is TestE2EForkedSessionDiverges's.)
func TestAForkIsOfferedOnlyToItsSessionsRunnerAdvertisingFork(t *testing.T) {
	f := newFixture(t)
	r1 := f.register(t, "r1")
	f.held(t, "r1", r1, "a", first("r1", 2))
	r2 := f.register(t, "r2")
	f.mustSync(t, "r2", r2, first("r2", 2))
	f.enqueue(t, forkRun("b", "s-b", "s-a"))

	if got := ids(f.mustSync(t, "r2", r2, req("r2", 2)).Runs); len(got) != 0 {
		t.Fatalf("offered %v to a runner that does not hold the forked session", got)
	}
	if got := ids(f.mustSync(t, "r1", r1, downgraded("r1", 1, capability.FeatureFork)).Runs); len(got) != 0 {
		t.Fatalf("offered %v to the forked session's runner without %q", got, capability.FeatureFork)
	}
	res := f.mustSync(t, "r1", r1, first("r1", 1))
	if got := ids(res.Runs); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("offered %v to the forked session's runner with %q, want [b]", got, capability.FeatureFork)
	}
	if s := res.Runs[0].Session; !s.New || s.ForkFrom != "s-a" {
		t.Errorf("the fork is offered as %+v, want new and forking s-a", s)
	}
}

// A runner that goes away takes with it the forks of its sessions no claim
// has bound: only it could ever open one, and a run queued in it would wait
// for ever.
func TestAnUnboundForkClosesWithItsSessionsRunner(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.held(t, "r1", cred, "a", first("r1", 1))
	f.enqueue(t, forkRun("b", "s-b", "s-a"))
	if code, env := f.deregister(t, "r1", cred, "retiring"); code != http.StatusOK {
		t.Fatalf("deregister: %d %+v", code, env)
	}
	if s := f.state(t, "b"); !v1.RunState(s).IsTerminal() {
		t.Errorf("the fork's run is %s after its only possible runner went, want it ended", s)
	}
	sess, err := f.store.GetSession(t.Context(), "s-b")
	if err != nil || !sess.ClosedAt.Valid {
		t.Errorf("the fork's session = %+v, %v; want it closed", sess, err)
	}
}

// A fork waiting to open ends when the session it forks closes, or is asked
// to close, however that happens: the runner refuses to fork a session it has
// closed or is closing, and the fork can go to no other runner, so it would
// otherwise be offered to be refused or wait for ever (DEV-151). It ends
// failed, with nothing to copy, and its session closes. A fork a claim has
// already bound has its own conversation, and stays open.
func TestAForkWaitingToOpenEndsWhenItsSourceCloses(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(t *testing.T, f *fixture, cred string)
	}{
		{"asked to close through the service API", func(t *testing.T, f *fixture, cred string) {
			if code, e := f.api(t, "POST", "/sessions/s-a/close", f.admin(t, "closer"), nil, nil); code != http.StatusOK {
				t.Fatalf("close: %d %+v", code, e)
			}
		}},
		{"closed by the runner's owner", func(t *testing.T, f *fixture, cred string) {
			if code, env := f.result(t, cred, "a", v1.Result{State: v1.RunSucceeded}); code != http.StatusOK {
				t.Fatalf("result: %d %+v", code, env.Error)
			}
			report := req("r1", 0, claimed("c")...)
			report.ClosedSessions = []v1.ClosedSession{{SessionID: "s-a", Reason: v1.SessionClosedByOwner, ClosedAt: f.clock.Now()}}
			f.mustSync(t, "r1", cred, report)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			cred := f.register(t, "r1")
			f.held(t, "r1", cred, "a", first("r1", 1))
			// c opens fork s-c and is claimed; b waits to open fork s-b.
			f.enqueue(t, forkRun("c", "s-c", "s-a"))
			if got := ids(f.mustSync(t, "r1", cred, req("r1", 1, claimed("a")...)).Runs); !slices.Equal(got, []string{"c"}) {
				t.Fatalf("offered %v, want [c]", got)
			}
			f.mustSync(t, "r1", cred, req("r1", 0, claimed("a", "c")...))
			f.enqueue(t, forkRun("b", "s-b", "s-a"))
			tc.close(t, f, cred)

			r, err := f.store.GetRun(t.Context(), "b")
			if err != nil {
				t.Fatal(err)
			}
			if r.State != string(v1.RunFailed) || !strings.Contains(r.Reason.String, "no conversation to copy") {
				t.Errorf("the waiting fork is %s (%q), want failed with nothing to copy", r.State, r.Reason.String)
			}
			if s, err := f.store.GetSession(t.Context(), "s-b"); err != nil || !s.ClosedAt.Valid {
				t.Errorf("the waiting fork's session = %+v, %v; want it closed", s, err)
			}
			if s, err := f.store.GetSession(t.Context(), "s-c"); err != nil || s.ClosedAt.Valid || s.CloseRequestedAt.Valid {
				t.Errorf("the bound fork's session = %+v, %v; want it open", s, err)
			}
			if got := f.state(t, "c"); got != "claimed" {
				t.Errorf("the bound fork's run is %s, want claimed", got)
			}
		})
	}
}

// A fork its claim bound is spared when its source is asked to close, since it
// has a conversation of its own — until that claim is withdrawn and the hub
// unbinds it. Then it has none, and its source can no longer be forked, so it
// closes and the run waiting in it ends rather than being offered to a runner
// that would refuse it (DEV-151).
func TestAForkUnboundAfterItsSourceClosesEnds(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.held(t, "r1", cred, "a", first("r1", 1))
	f.enqueue(t, forkRun("c", "s-c", "s-a"))
	if got := ids(f.mustSync(t, "r1", cred, req("r1", 1, claimed("a")...)).Runs); !slices.Equal(got, []string{"c"}) {
		t.Fatalf("offered %v, want [c]", got)
	}
	f.mustSync(t, "r1", cred, req("r1", 0, claimed("a", "c")...))
	d := run("d", "s-c")
	d.Session.New = false
	f.enqueue(t, d)
	tok := f.admin(t, "cli")
	if code, e := f.api(t, "POST", "/sessions/s-a/close", tok, nil, nil); code != http.StatusOK {
		t.Fatalf("close: %d %+v", code, e)
	}
	if s, err := f.store.GetSession(t.Context(), "s-c"); err != nil || s.ClosedAt.Valid {
		t.Fatalf("the bound fork = %+v, %v; want it open while its claim stands", s, err)
	}
	if code, e := f.api(t, "POST", "/runs/c/cancel", tok, nil, nil); code != http.StatusOK {
		t.Fatalf("cancel: %d %+v", code, e)
	}
	// The runner withdraws the claim on hearing the cancel, and leaves it out.
	f.mustSync(t, "r1", cred, req("r1", 0, claimed("a")...))

	if got := f.state(t, "c"); got != "cancelled" {
		t.Fatalf("c is %s, want cancelled", got)
	}
	if s, err := f.store.GetSession(t.Context(), "s-c"); err != nil || !s.ClosedAt.Valid {
		t.Errorf("the fork unbound after its source closed = %+v, %v; want it closed", s, err)
	}
	r, err := f.store.GetRun(t.Context(), "d")
	if err != nil {
		t.Fatal(err)
	}
	if r.State != string(v1.RunFailed) || !strings.Contains(r.Reason.String, "no conversation to copy") {
		t.Errorf("the run waiting in it is %s (%q), want failed with nothing to copy", r.State, r.Reason.String)
	}
}

// yad hub refuses at submit a fork that could never be offered, each with
// what to do instead, and queues one that can.
func TestSubmittingAFork(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	f.held(t, "r1", cred, "a", first("r1", 1))
	f.enqueue(t, run("u", "s-unbound"))
	old := f.register(t, "old")
	f.held(t, "old", old, "o", downgraded("old", 1, capability.FeatureFork))
	f.enqueue(t, run("x", "s-closed"))
	if code, e := f.api(t, "POST", "/sessions/s-closed/close", tok, nil, nil); code != http.StatusOK {
		t.Fatalf("close: %d %+v", code, e)
	}

	submit := func(session hubapi.SessionChoice, harness string) (int, v1.Error) {
		t.Helper()
		return f.api(t, "POST", "/runs", tok, hubapi.SubmitRequest{
			Session: &session, Harness: harness, Model: "opus", Brief: v1.Brief{Instruction: "fork it"},
		}, nil)
	}
	for _, tc := range []struct {
		name    string
		session hubapi.SessionChoice
		harness string
		code    int
		says    string
	}{
		{"unknown", hubapi.SessionChoice{ID: "f1", New: true, ForkFrom: "s-nowhere"}, "claude", http.StatusNotFound, "no session \"s-nowhere\" to fork"},
		{"on no runner yet", hubapi.SessionChoice{ID: "f2", New: true, ForkFrom: "s-unbound"}, "claude", http.StatusConflict, "hub watch"},
		{"on a runner without fork", hubapi.SessionChoice{ID: "f3", New: true, ForkFrom: "s-o"}, "claude", http.StatusConflict, "upgrade yad on that runner"},
		{"closed", hubapi.SessionChoice{ID: "f4", New: true, ForkFrom: "s-closed"}, "claude", http.StatusConflict, "closed"},
		{"another harness", hubapi.SessionChoice{ID: "f5", New: true, ForkFrom: "s-a"}, "codex", http.StatusConflict, "claude session"},
		{"on a continuing run", hubapi.SessionChoice{ID: "s-a", ForkFrom: "s-o"}, "claude", http.StatusBadRequest, "session.new true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, e := submit(tc.session, tc.harness)
			if code != tc.code || !strings.Contains(e.Message+" "+e.NextAction, tc.says) {
				t.Errorf("%d %+v; want %d saying %q", code, e, tc.code, tc.says)
			}
		})
	}
	var view hubapi.Run
	ok := hubapi.SubmitRequest{
		RunID:   "fork-ok",
		Session: &hubapi.SessionChoice{ID: "f-ok", New: true, ForkFrom: "s-a"}, Harness: "claude", Model: "opus",
		Brief: v1.Brief{Instruction: "fork it"},
	}
	if code, e := f.api(t, "POST", "/runs", tok, ok, &view); code != http.StatusCreated {
		t.Fatalf("a fork of an open session on a runner advertising fork: %d %+v", code, e)
	}
	// A retry is answered with the run queued, as every retry is, even once
	// the forked session's runner no longer advertises fork.
	f.mustSync(t, "r1", cred, downgraded("r1", 1, capability.FeatureFork))
	if code, e := f.api(t, "POST", "/runs", tok, ok, &view); code != http.StatusCreated || view.RunID != "fork-ok" {
		t.Fatalf("a retried fork: %d %+v %+v", code, e, view)
	}
	var s hubapi.Session
	if code, e := f.api(t, "GET", "/sessions/f-ok", tok, nil, &s); code != http.StatusOK || s.ForkFrom != "s-a" {
		t.Errorf("the fork's session: %d %+v %+v", code, e, s)
	}
}
