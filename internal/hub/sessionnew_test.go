package hub

import (
	"context"
	"net/http"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// session.new is decided when a run is offered: true while no claim has bound
// its session, false after (decision 0047). A session whose first run never
// bound it — refused, cancelled on its claim, withdrawn — exists on no runner,
// so the run after it goes out new, carrying the sources the session was
// submitted with; once a claim binds the session, every later run goes out
// continuing it.
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
			// The sources go with the run that opens the session, and only
			// with it: a continuing run named none and is sent none.
			if wantSources := tc.wantNew; (len(got.Sources) == 1 && got.Sources[0].Path == src[0].Path) != wantSources {
				t.Errorf("b went out with sources %+v; opening the session it carries its first run's, continuing it none", got.Sources)
			}
		})
	}
}
