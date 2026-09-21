package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub/store/db"
)

func (f *fixture) deregister(t *testing.T, runner, cred, reason string) (int, v1.ErrorEnvelope) {
	t.Helper()
	b, err := json.Marshal(v1.DeregisterRequest{Reason: reason})
	if err != nil {
		t.Fatal(err)
	}
	res, env := post(t, f.hub, "/v1/runners/"+runner+"/deregister", string(b), headers(cred))
	return res.StatusCode, env
}

func continues(id, session string) v1.Run {
	r := run(id, session)
	r.Session.New = false
	return r
}

// A deregistered runner leaves nothing stranded. The run it held is lost; the
// offer it never claimed goes back in the queue for another runner; the
// session it held closes, bound to it still; and the run queued behind in
// that session fails, naming what to do instead — where before it stayed
// queued for good, offerable only to the credential just retired.
func TestDeregisterClosesTheRunnersSessions(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.claimedBy(t, "r1", cred, "a1")      // binds session s-a1 to r1
	f.enqueue(t, continues("a2", "s-a1")) // waits behind a1, for r1 alone
	f.enqueue(t, run("b1", "s-b1"))       // a new session nobody holds yet
	res := f.mustSync(t, "r1", cred, req("r1", 1, claimed("a1")...))
	if len(res.Runs) != 1 || res.Runs[0].RunID != "b1" {
		t.Fatalf("offered %v, want b1", ids(res.Runs))
	}

	if code, env := f.deregister(t, "r1", cred, "retiring\nthis box"); code != http.StatusOK {
		t.Fatalf("deregister: %d %+v", code, env)
	}

	ctx := context.Background()
	for _, tc := range []struct {
		run, state, reason string
	}{
		{"a1", "lost", "runner r1 deregistered while it held this run: retiring this box"},
		{"a2", "failed", runnerDeregistered.reason},
		{"b1", "queued", ""},
	} {
		r, err := f.store.GetRun(ctx, tc.run)
		if err != nil {
			t.Fatal(err)
		}
		if r.State != tc.state || r.Reason.String != tc.reason {
			t.Errorf("%s: %s %q, want %s %q", tc.run, r.State, r.Reason.String, tc.state, tc.reason)
		}
	}
	sess, err := f.store.GetSession(ctx, "s-a1")
	if err != nil {
		t.Fatal(err)
	}
	if !sess.ClosedAt.Valid || sess.CloseReason.String != string(v1.SessionClosedByOwner) {
		t.Errorf("session s-a1: closed %v (%q), want closed_by_owner", sess.ClosedAt.Valid, sess.CloseReason.String)
	}
	if sess.RunnerID.String != "r1" {
		t.Errorf("session s-a1 is bound to %q: its transcript is on r1, so it must stay r1's", sess.RunnerID.String)
	}

	if r, _ := f.sync(t, "r1", cred, req("r1", 1)); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("sync with the retired credential: %d, want 401", r.StatusCode)
	}
	other := f.register(t, "r2")
	if res := f.mustSync(t, "r2", other, first("r2", 2)); len(res.Runs) != 1 || res.Runs[0].RunID != "b1" {
		t.Errorf("another runner is offered %v, want the requeued b1 alone", ids(res.Runs))
	}

	// The id comes back with a token issued for it, and takes new work.
	cred = f.register(t, "r1")
	f.enqueue(t, run("c1", "s-c1"))
	if res := f.mustSync(t, "r1", cred, first("r1", 1)); len(res.Runs) != 1 || res.Runs[0].RunID != "c1" {
		t.Errorf("re-registered r1 is offered %v, want c1", ids(res.Runs))
	}
}

// A session its runner reports closed takes no run, so the runs queued in it
// end with the report rather than being offered to be refused — cancelled
// when the hub asked for the close, failed with what to do instead when the
// runner closed it on its own. A runner that then deregisters finds nothing
// left in it to settle, and the session keeps the reason it closed with.
func TestARunnersCloseEndsTheRunsQueuedInIt(t *testing.T) {
	for _, tc := range []struct {
		reason v1.SessionCloseReason
		want   string
	}{
		{v1.SessionExpired, "failed"},
		{v1.SessionClosed, "cancelled"},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			f := newFixture(t)
			cred := f.register(t, "r1")
			f.claimedBy(t, "r1", cred, "a1")
			f.enqueue(t, continues("a2", "s-a1"))
			if code, env := f.result(t, cred, "a1", v1.Result{State: v1.RunSucceeded}); code != http.StatusOK {
				t.Fatalf("result: %d %+v", code, env)
			}
			closed := req("r1", 1)
			closed.ClosedSessions = []v1.ClosedSession{{SessionID: "s-a1", Reason: tc.reason, ClosedAt: f.clock.Now()}}
			if res := f.mustSync(t, "r1", cred, closed); len(res.Runs) != 0 {
				t.Errorf("offered %v in the answer to the close", ids(res.Runs))
			}
			if s := f.state(t, "a2"); s != tc.want {
				t.Errorf("a2 is %s after the close, want %s", s, tc.want)
			}

			if code, env := f.deregister(t, "r1", cred, ""); code != http.StatusOK {
				t.Fatalf("deregister: %d %+v", code, env)
			}
			if s := f.state(t, "a2"); s != tc.want {
				t.Errorf("a2 is %s after deregister, want %s still", s, tc.want)
			}
			sess, err := f.store.GetSession(context.Background(), "s-a1")
			if err != nil {
				t.Fatal(err)
			}
			if sess.CloseReason.String != string(tc.reason) {
				t.Errorf("session s-a1 closed %q, want the runner's own %q", sess.CloseReason.String, tc.reason)
			}
		})
	}
}

// Authenticating and the transaction that acts on it take the store's one
// connection separately, so a credential can be replaced between them. Once
// it has been, the transaction refuses — or a deregister would retire the
// credential a re-registration just issued, and a sync would offer runs to a
// credential already dead.
func TestAReplacedCredentialIsRefusedInsideTheTransaction(t *testing.T) {
	f := newFixture(t)
	f.register(t, "r1")
	ctx := context.Background()
	checked, err := f.store.GetRunner(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	f.register(t, "r1") // a new token for r1, and a new credential
	err = f.store.Tx(ctx, func(q *db.Queries) error {
		_, err := f.hub.current(ctx, q, checked)
		return err
	})
	var e *ErrorResponse
	if !errors.As(err, &e) || e.status != http.StatusUnauthorized {
		t.Errorf("err = %v, want a 401", err)
	}
}

// Deregistering is the runner's own act: another runner's credential cannot
// retire it, and neither can no credential at all.
func TestDeregisterNeedsTheRunnersOwnCredential(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	other := f.register(t, "r2")
	for _, tc := range []struct {
		name, cred string
		want       int
	}{
		{"no credential", "", http.StatusUnauthorized},
		{"another runner's", other, http.StatusForbidden},
	} {
		if code, _ := f.deregister(t, "r1", tc.cred, ""); code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, code, tc.want)
		}
	}
	f.mustSync(t, "r1", cred, first("r1", 1))
}

// A runner's reason reaches every run it held, so it is one printable line
// and bounded however it arrives.
func TestCleanReason(t *testing.T) {
	long := strings.Repeat("é", maxDeregisterReason)
	for _, tc := range []struct {
		name, in, want string
	}{
		{"empty", "  ", ""},
		{"lines joined", "one\ntwo\tthree", "one two three"},
		{"controls dropped", "a\x00b\x1bc\x7fd", "abcd"},
		{"bounded on a rune", long, strings.Repeat("é", maxDeregisterReason/2) + "…"},
	} {
		if got := cleanReason(tc.in); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// Every way a run ends on this hub leaves it holding the grants' names and
// none of their values (decision 0041) — the schema's trigger does it, and
// this walks each path in to prove none of them goes around it. A run that is
// only waiting keeps its values, because its resume is built from them.
func TestEveryEndForgetsTheGrantValues(t *testing.T) {
	const secret = "grant-secret-value"
	granted := func(id string) v1.Run {
		r := run(id, "s-"+id)
		r.Grants = []v1.Grant{{Name: "ZUMINO_TOKEN", Value: secret, As: v1.GrantEnv}}
		return r
	}
	// held has runner r1 claim the run, as the paths that need a holder want.
	held := func(t *testing.T, f *fixture, id string) string {
		t.Helper()
		cred := f.register(t, "r1")
		f.enqueue(t, granted(id))
		f.mustSync(t, "r1", cred, first("r1", 1))
		f.mustSync(t, "r1", cred, req("r1", 1, claimed(id)...))
		return cred
	}
	for _, tc := range []struct {
		name   string
		end    func(t *testing.T, f *fixture)
		state  string
		forget bool
	}{
		{"a result", func(t *testing.T, f *fixture) {
			cred := held(t, f, "x")
			if code, env := f.result(t, cred, "x", v1.Result{State: v1.RunSucceeded}); code != http.StatusOK {
				t.Fatalf("result: %d %+v", code, env)
			}
		}, "succeeded", true},
		{"a refusal before the claim", func(t *testing.T, f *fixture) {
			cred := f.register(t, "r1")
			f.enqueue(t, granted("x"))
			f.mustSync(t, "r1", cred, first("r1", 1))
			refusal := v1.Result{State: v1.RunFailed, Error: &v1.RunError{Class: "refused", Message: "no"}}
			if code, env := f.result(t, cred, "x", refusal); code != http.StatusOK {
				t.Fatalf("result: %d %+v", code, env)
			}
		}, "failed", true},
		{"a cancel before any runner took it", func(t *testing.T, f *fixture) {
			f.enqueue(t, granted("x"))
			if code, e := f.api(t, "POST", "/runs/x/cancel", f.admin(t, "cli"), nil, nil); code != http.StatusOK {
				t.Fatalf("cancel: %d %s", code, e.Message)
			}
		}, "cancelled", true},
		{"a session closed before any runner took it", func(t *testing.T, f *fixture) {
			f.enqueue(t, granted("x"))
			if code, e := f.api(t, "POST", "/sessions/s-x/close", f.admin(t, "cli"), nil, nil); code != http.StatusOK {
				t.Fatalf("close: %d %s", code, e.Message)
			}
		}, "cancelled", true},
		{"a lease that lapsed", func(t *testing.T, f *fixture) {
			held(t, f, "x")
			f.clock.Advance(time.Hour)
			if err := f.hub.Sweep(context.Background()); err != nil {
				t.Fatal(err)
			}
		}, "lost", true},
		{"its runner deregistering while it held it", func(t *testing.T, f *fixture) {
			cred := held(t, f, "x")
			if code, env := f.deregister(t, "r1", cred, ""); code != http.StatusOK {
				t.Fatalf("deregister: %d %+v", code, env)
			}
		}, "lost", true},
		{"its session's runner deregistering while it waited", func(t *testing.T, f *fixture) {
			cred := held(t, f, "first")
			next := granted("x")
			next.Session = v1.SessionRef{ID: "s-first"}
			f.enqueue(t, next)
			if code, env := f.deregister(t, "r1", cred, ""); code != http.StatusOK {
				t.Fatalf("deregister: %d %+v", code, env)
			}
		}, "failed", true},
		{"nothing: parked on a usage limit", func(t *testing.T, f *fixture) {
			cred := held(t, f, "x")
			at := f.clock.Now().Add(time.Hour)
			f.mustSync(t, "r1", cred, req("r1", 1, v1.HeldRun{RunID: "x", State: v1.RunWaiting, ResumesAt: &at, Reason: "usage limit"}))
		}, "waiting", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.end(t, f)
			stored, err := f.store.GetRun(context.Background(), "x")
			if err != nil {
				t.Fatal(err)
			}
			if stored.State != tc.state {
				t.Fatalf("state %s, want %s", stored.State, tc.state)
			}
			var spec v1.Run
			if err := json.Unmarshal([]byte(stored.Spec), &spec); err != nil {
				t.Fatalf("spec is not a run: %v\n%s", err, stored.Spec)
			}
			if len(spec.Grants) != 1 || spec.Grants[0].Name != "ZUMINO_TOKEN" || spec.Grants[0].As != v1.GrantEnv {
				t.Errorf("grants %+v: the name and delivery are the record of what the run was given", spec.Grants)
			}
			if kept := strings.Contains(stored.Spec, secret); kept == tc.forget {
				t.Errorf("value kept = %v after %s, want %v", kept, tc.name, !tc.forget)
			}
		})
	}
}
