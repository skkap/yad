package runner

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/store/db"
)

// forkOf is a run opening session as a fork of from.
func forkOf(id, session, from string) v1.Run {
	r := testRun(id, session)
	r.Session.ForkFrom = from
	return r
}

// A fork, through the hub in process (decision 0065): the run opening it is
// handed the forked session's native id to fork and none of its own, in a
// workdir of its own; the fork then pins its own id and its next run resumes
// that; and the forked session's next run resumes its own conversation, as
// if the fork had never happened.
func TestAForkStartsFromTheForkedConversationAndDiverges(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	n := 0
	h := &fake.Adapter{ID: "claude", Next: func(spec adapter.Spec) fake.Script {
		n++
		native := spec.NativeSessionID
		if native == "" {
			native = []string{"", "native-1", "native-2"}[n]
		}
		return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: native}}
	}}
	x := e.executor(h)

	runOne(t, e, l, x, testRun("a", "s1"))
	runOne(t, e, l, x, forkOf("b", "s2", "s1"))
	runOne(t, e, l, x, continued("c", "s2"))
	runOne(t, e, l, x, continued("d", "s1"))

	if len(h.Starts) != 4 {
		t.Fatalf("%d starts, want 4", len(h.Starts))
	}
	for i, want := range []struct{ native, fork string }{
		{"", ""}, {"", "native-1"}, {"native-2", ""}, {"native-1", ""},
	} {
		if got := h.Starts[i]; got.NativeSessionID != want.native || got.ForkFrom != want.fork {
			t.Errorf("start %d resumed %q forking %q; want %q forking %q", i, got.NativeSessionID, got.ForkFrom, want.native, want.fork)
		}
	}
	if h.Starts[1].Workdir == h.Starts[0].Workdir {
		t.Errorf("the fork ran in the forked session's workdir %s", h.Starts[0].Workdir)
	}
	if s := session(t, e, "s2"); s.NativeID.String != "native-2" || s.ForkFrom.String != "s1" {
		t.Errorf("the fork's session = %+v", s)
	}
	if s := session(t, e, "s1"); s.NativeID.String != "native-1" {
		t.Errorf("the forked session's native id moved to %q", s.NativeID.String)
	}
}

// A fork that fails as a resume does: a harness with no transcript to fork
// (session_not_found) is resume_rejected, a forked session with no native id
// yet ends the same way without spawning, and an adapter that cannot fork
// refuses the run rather than start the conversation empty.
func TestAForkThatCannotStartIsClassed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		firstID    string // the forked session's native id
		noFork     bool
		forkClass  string // what the fork's harness says, when it starts
		wantClass  string
		wantStarts int
	}{
		{name: "no transcript", firstID: "native-1", forkClass: adapter.ClassSessionNotFound, wantClass: ClassResumeRejected, wantStarts: 2},
		{name: "no conversation yet", wantClass: ClassResumeRejected, wantStarts: 1},
		{name: "an adapter that cannot fork", firstID: "native-1", noFork: true, wantClass: ClassRefused, wantStarts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			h := &fake.Adapter{ID: "claude", NoFork: tc.noFork, Next: func(spec adapter.Spec) fake.Script {
				if spec.ForkFrom == "" {
					return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: tc.firstID}}
				}
				err := &v1.RunError{Class: tc.forkClass, Message: "the harness said no"}
				return fake.Script{Outcome: adapter.Outcome{State: v1.RunFailed, Error: err, NativeSessionID: "native-2"}}
			}}
			x := e.executor(h)
			runOne(t, e, l, x, testRun("a", "s1"))
			e.enqueue(t, forkOf("b", "s2", "s1"))
			claimAndRun(t, l, x)

			res, ok := outboxResult(t, e, "b")
			if !ok || res.State != v1.RunFailed || res.Error == nil || res.Error.Class != tc.wantClass {
				t.Fatalf("result = %+v (error %+v), %v; want class %s", res, res.Error, ok, tc.wantClass)
			}
			if len(h.Starts) != tc.wantStarts {
				t.Errorf("%d starts, want %d", len(h.Starts), tc.wantStarts)
			}
		})
	}
}

// The runner checks a fork's session at the claim, whatever the hub checked:
// one it does not hold for this hub, one of another harness, and one closed
// or closing are refused, and nothing is recorded.
func TestAForkOfASessionTheRunnerCannotForkIsRefused(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	l := e.loop(t, 1)
	create := func(conn, id, harness string) {
		t.Helper()
		if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: conn, ID: id, Harness: harness, Workdir: "/w"}); err != nil {
			t.Fatal(err)
		}
	}
	create("hub", "open", "claude")
	create("other-hub", "elsewhere", "claude")
	create("hub", "codex-one", "codex")
	create("hub", "closed", "claude")
	if _, err := e.store.CloseSession(ctx, db.CloseSessionParams{State: "closed", Reason: sql.NullString{String: "closed", Valid: true}, Now: sql.NullInt64{Int64: 1, Valid: true}, Connection: "hub", ID: "closed"}); err != nil {
		t.Fatal(err)
	}
	create("hub", "closing", "claude")
	if err := e.store.RequestSessionClose(ctx, db.RequestSessionCloseParams{Reason: sql.NullString{String: "closed", Valid: true}, Now: sql.NullInt64{Int64: 1, Valid: true}, Connection: "hub", ID: "closing"}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ from, want string }{
		{"nowhere", "does not hold session nowhere"},
		{"elsewhere", "does not hold session elsewhere"},
		{"codex-one", "is a codex session"},
		{"closed", "was closed"},
		{"closing", "is closing"},
	} {
		t.Run(tc.from, func(t *testing.T) {
			_, err := l.record(ctx, forkOf("r-"+tc.from, "fork-of-"+tc.from, tc.from))
			var r refused
			if !errors.As(err, &r) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want a refusal saying %q", err, tc.want)
			}
			if _, err := e.store.GetSession(ctx, db.GetSessionParams{Connection: "hub", ID: "fork-of-" + tc.from}); err == nil {
				t.Error("the refused fork's session was recorded")
			}
		})
	}
	if _, err := l.record(ctx, forkOf("r-open", "fork-of-open", "open")); err != nil {
		t.Fatalf("a fork of an open session this runner holds: %v", err)
	}
}
