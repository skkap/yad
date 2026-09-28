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
// since it can go nowhere else (decision 0064). The offer opens the session
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
		{"on no runner yet", hubapi.SessionChoice{ID: "f2", New: true, ForkFrom: "s-unbound"}, "claude", http.StatusConflict, "once one of its runs has started"},
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
	if code, e := f.api(t, "POST", "/runs", tok, hubapi.SubmitRequest{
		Session: &hubapi.SessionChoice{ID: "f-ok", New: true, ForkFrom: "s-a"}, Harness: "claude", Model: "opus",
		Brief: v1.Brief{Instruction: "fork it"},
	}, &view); code != http.StatusCreated {
		t.Fatalf("a fork of an open session on a runner advertising fork: %d %+v", code, e)
	}
	var s hubapi.Session
	if code, e := f.api(t, "GET", "/sessions/f-ok", tok, nil, &s); code != http.StatusOK || s.ForkFrom != "s-a" {
		t.Errorf("the fork's session: %d %+v %+v", code, e, s)
	}
}
