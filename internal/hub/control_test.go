package hub

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"
)

func controls(res v1.SyncResponse) []string {
	var out []string
	for _, c := range res.Controls {
		if c.RunID == "" {
			continue
		}
		s := string(c.Kind) + " " + c.RunID
		if c.Text != "" {
			s += " " + c.Text
		}
		out = append(out, s)
	}
	return out
}

// A run no runner has started ends on the hub without a round trip. One that
// was offered is withdrawn from its runner by the cancel its next sync hears.
func TestCancelBeforeAnyRunnerStartsIt(t *testing.T) {
	for _, offered := range []bool{false, true} {
		t.Run(fmt.Sprintf("offered %v", offered), func(t *testing.T) {
			f := newFixture(t)
			tok := f.admin(t, "cli")
			cred := f.register(t, "r1")
			f.enqueue(t, run("a", "s1"))
			if offered {
				if res := f.mustSync(t, "r1", cred, first("r1", 1)); !slices.Equal(ids(res.Runs), []string{"a"}) {
					t.Fatalf("offered %v", ids(res.Runs))
				}
			}
			var view hubapi.Run
			if code, e := f.api(t, "POST", "/runs/a/cancel", tok, nil, &view); code != 200 {
				t.Fatalf("cancel: %d %s", code, e.Message)
			}
			if view.State != hubapi.RunState(v1.RunCancelled) || view.Reason != cancelledBeforeStart.reason || view.CancelRequestedAt != nil {
				t.Fatalf("view %+v", view)
			}
			// The runner lists the offer it took, as it would have claimed it,
			// and is told to drop it; nothing is offered in its place.
			held := []v1.HeldRun(nil)
			if offered {
				held = claimed("a")
			}
			res := f.mustSync(t, "r1", cred, req("r1", 1, held...))
			if len(res.Runs) != 0 || offered != slices.Equal(cancels(res), []string{"a"}) {
				t.Errorf("runs %v cancels %v", ids(res.Runs), cancels(res))
			}
			if got := f.state(t, "a"); got != "cancelled" {
				t.Errorf("state %s", got)
			}
			// Asking again answers the run as it is.
			if code, _ := f.api(t, "POST", "/runs/a/cancel", tok, nil, &view); code != 200 || view.State != hubapi.RunState(v1.RunCancelled) {
				t.Errorf("second cancel: %d %+v", code, view)
			}
		})
	}
}

// Controls for a held run go to its runner in the next sync. A cancel and an
// interrupt go on every sync until the run ends, since a lost response must
// not lose them; a steer goes once, since the harness would read it twice.
func TestControlsReachTheHolder(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	f.enqueue(t, run("a", "s1"))
	f.mustSync(t, "r1", cred, first("r1", 1))
	running := []v1.HeldRun{{RunID: "a", State: v1.RunRunning}}
	if res := f.mustSync(t, "r1", cred, req("r1", 0, running...)); len(res.Controls) != 0 {
		t.Fatalf("controls before any were asked for: %+v", res.Controls)
	}

	for _, c := range []struct {
		path string
		body any
	}{
		{"/runs/a/interrupt", nil},
		{"/runs/a/steer", hubapi.SteerRequest{Text: "use tabs"}},
		{"/runs/a/interrupt", nil},
		{"/runs/a/steer", hubapi.SteerRequest{Text: "and spaces"}},
		{"/runs/a/cancel", nil},
		{"/runs/a/cancel", nil},
	} {
		var view hubapi.Run
		if code, e := f.api(t, "POST", c.path, tok, c.body, &view); code != 200 {
			t.Fatalf("%s: %d %s", c.path, code, e.Message)
		}
		if view.State != hubapi.RunState(v1.RunRunning) {
			t.Errorf("%s: state %s", c.path, view.State)
		}
	}
	var view hubapi.Run
	f.api(t, "GET", "/runs/a", tok, nil, &view)
	if view.CancelRequestedAt == nil || !view.CancelRequestedAt.Equal(f.clock.Now()) {
		t.Errorf("cancel_requested_at %v", view.CancelRequestedAt)
	}

	want := []string{"interrupt a", "steer a use tabs", "steer a and spaces", "cancel a"}
	if got := controls(f.mustSync(t, "r1", cred, req("r1", 0, running...))); !slices.Equal(got, want) {
		t.Errorf("first delivery %q, want %q", got, want)
	}
	want = []string{"interrupt a", "cancel a"}
	if got := controls(f.mustSync(t, "r1", cred, req("r1", 0, running...))); !slices.Equal(got, want) {
		t.Errorf("second delivery %q, want %q", got, want)
	}
	// A steer after the first delivery goes out on its own.
	f.api(t, "POST", "/runs/a/steer", tok, hubapi.SteerRequest{Text: "late"}, nil)
	want = []string{"interrupt a", "cancel a", "steer a late"}
	if got := controls(f.mustSync(t, "r1", cred, req("r1", 0, running...))); !slices.Equal(got, want) {
		t.Errorf("third delivery %q, want %q", got, want)
	}

	// The run ends; the cancel no longer shows, and asking again is answered
	// by what it ended as.
	latency := int64(40)
	if code, env := f.result(t, cred, "a", v1.Result{State: v1.RunCancelled, Metrics: v1.Metrics{CancelLatencyMS: &latency}}); code != 200 {
		t.Fatalf("result: %d %+v", code, env)
	}
	view = hubapi.Run{}
	f.api(t, "GET", "/runs/a", tok, nil, &view)
	if view.CancelRequestedAt != nil || view.Result == nil || view.Result.Metrics.CancelLatencyMS == nil || *view.Result.Metrics.CancelLatencyMS != 40 {
		t.Errorf("view after the end %+v", view)
	}
	if code, _ := f.api(t, "POST", "/runs/a/cancel", tok, nil, nil); code != 200 {
		t.Errorf("cancel of a cancelled run: %d", code)
	}
	for _, path := range []string{"/runs/a/interrupt", "/runs/a/steer"} {
		if code, e := f.api(t, "POST", path, tok, hubapi.SteerRequest{Text: "x"}, nil); code != http.StatusConflict || e.Code != v1.CodeConflict {
			t.Errorf("%s on a finished run: %d %+v", path, code, e)
		}
	}
}

// What cannot be done to a run says so, with the way out.
func TestControlsRefused(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	f.enqueue(t, run("done", "s1"), run("queued", "s2"))
	f.mustSync(t, "r1", cred, first("r1", 1))
	f.mustSync(t, "r1", cred, req("r1", 0, claimed("done")...))
	if code, env := f.result(t, cred, "done", v1.Result{State: v1.RunSucceeded}); code != 200 {
		t.Fatalf("result: %d %+v", code, env)
	}
	f.clock.Advance(time.Second)
	for _, c := range []struct {
		path string
		body any
		code int
	}{
		{"/runs/queued/interrupt", nil, http.StatusConflict},
		{"/runs/queued/steer", hubapi.SteerRequest{Text: "x"}, http.StatusConflict},
		{"/runs/done/cancel", nil, http.StatusConflict},
		{"/runs/nope/cancel", nil, http.StatusNotFound},
		{"/runs/nope/steer", hubapi.SteerRequest{Text: "x"}, http.StatusNotFound},
		{"/runs/queued/steer", hubapi.SteerRequest{}, http.StatusUnprocessableEntity},
	} {
		code, e := f.api(t, "POST", c.path, tok, c.body, nil)
		if code != c.code {
			t.Errorf("%s: %d %+v, want %d", c.path, code, e, c.code)
		}
	}
	if got := f.state(t, "queued"); got != "queued" {
		t.Errorf("a refused control moved the run: %s", got)
	}
}
