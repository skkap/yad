package hub

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
)

func closes(res v1.SyncResponse) []string {
	var ids []string
	for _, c := range res.Controls {
		if c.Kind == v1.ControlCloseSession {
			ids = append(ids, c.SessionID)
		}
	}
	return ids
}

// Closing a session a runner holds: refused for a runner that would ignore
// the control, repeated in every answer to the runner holding it until that
// runner reports it closed, and closed here only by that report. A closing or
// closed session takes no new run.
func TestCloseABoundSession(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	old := f.register(t, "old")
	f.mustSync(t, "old", old, first("old", 1))
	f.enqueue(t, run("o1", "on-old"))
	f.mustSync(t, "old", old, req("old", 1))
	f.mustSync(t, "old", old, req("old", 1, claimed("o1")...))

	cred := f.register(t, "r1")
	withClose := first("r1", 1)
	withClose.Capabilities.ProtocolFeatures = []string{capability.FeatureCloseSession}
	withClose.Fingerprint = "fp-r1-close"
	f.mustSync(t, "r1", cred, withClose)
	sync := func(r v1.SyncRequest) v1.SyncResponse {
		r.Fingerprint = withClose.Fingerprint
		return f.mustSync(t, "r1", cred, r)
	}
	f.enqueue(t, run("a", "s1"))
	sync(req("r1", 1))
	sync(req("r1", 1, claimed("a")...))

	for _, c := range []struct {
		path string
		code int
	}{
		{"/sessions/nope/close", http.StatusNotFound},
		{"/sessions/on-old/close", http.StatusConflict},
	} {
		if code, e := f.api(t, "POST", c.path, tok, nil, nil); code != c.code {
			t.Errorf("%s: %d %+v, want %d", c.path, code, e, c.code)
		}
	}

	var view hubapi.Session
	if code, e := f.api(t, "POST", "/sessions/s1/close", tok, nil, &view); code != http.StatusOK ||
		view.State != hubapi.SessionClosing || view.CloseRequestedAt == nil || view.RunnerID != "r1" {
		t.Fatalf("close: %d %+v %+v", code, e, view)
	}
	var cont hubapi.Run
	sub := submission("and then?")
	sub.Session = &hubapi.SessionChoice{ID: "s1"}
	if code, e := f.api(t, "POST", "/runs", tok, sub, &cont); code != http.StatusConflict {
		t.Errorf("a continuation of a closing session: %d %+v", code, e)
	}
	// Repeated while the run is held and after it ends, until the runner
	// reports the close: a lost answer must not lose it.
	for _, r := range []v1.SyncRequest{req("r1", 0, running("a")...), req("r1", 1)} {
		if got := closes(sync(r)); !slices.Equal(got, []string{"s1"}) {
			t.Fatalf("close_session controls = %v", got)
		}
	}
	// Another runner reporting the session is not the runner holding it.
	f.mustSync(t, "old", old, v1.SyncRequest{RunnerID: "old", Fingerprint: "fp-old", Health: v1.Health{FreeCapacity: v1.Capacity{Total: 1}},
		ClosedSessions: []v1.ClosedSession{{SessionID: "s1", Reason: v1.SessionExpired, ClosedAt: f.clock.Now()}}})
	var still hubapi.Session
	if f.api(t, "GET", "/sessions/s1", tok, nil, &still); still.State != hubapi.SessionClosing {
		t.Errorf("another runner's report closed it: %+v", still)
	}

	done := req("r1", 1)
	done.ClosedSessions = []v1.ClosedSession{{SessionID: "s1", Reason: v1.SessionClosed, ClosedAt: f.clock.Now()}}
	if got := closes(sync(done)); len(got) != 0 {
		t.Errorf("still asked to close %v in the answer to the report", got)
	}
	// A repeated report is the same news.
	sync(done)
	var closed hubapi.Session
	if code, _ := f.api(t, "GET", "/sessions/s1", tok, nil, &closed); code != http.StatusOK || closed.State != hubapi.SessionClosed ||
		closed.CloseReason != "closed" || closed.ClosedAt == nil || closed.CloseRequestedAt != nil {
		t.Errorf("after the report: %d %+v", code, closed)
	}
	if code, e := f.api(t, "POST", "/runs", tok, sub, &cont); code != http.StatusConflict {
		t.Errorf("a continuation of a closed session: %d %+v", code, e)
	}
	var again hubapi.Session
	if code, _ := f.api(t, "POST", "/sessions/s1/close", tok, nil, &again); code != http.StatusOK || again.State != hubapi.SessionClosed || again.CloseRequestedAt != nil {
		t.Errorf("closing a closed session: %d %+v", code, again)
	}
	if got := closes(sync(req("r1", 1))); len(got) != 0 {
		t.Errorf("a closed session is asked closed again: %v", got)
	}
}

// A runner's own closes — idle TTL, disk pressure, its owner — close the
// session here as well, with the runner's reason.
func TestARunnerReportsItsOwnClose(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	f.mustSync(t, "r1", cred, first("r1", 1))
	f.enqueue(t, run("a", "s1"))
	f.mustSync(t, "r1", cred, req("r1", 1))
	f.mustSync(t, "r1", cred, req("r1", 1, claimed("a")...))

	at := f.clock.Now().Add(-time.Minute)
	r := req("r1", 1)
	r.ClosedSessions = []v1.ClosedSession{{SessionID: "s1", Reason: v1.SessionDiskPressure, ClosedAt: at}, {SessionID: "never-here", Reason: v1.SessionExpired}}
	f.mustSync(t, "r1", cred, r)
	var view hubapi.Session
	if code, _ := f.api(t, "GET", "/sessions/s1", tok, nil, &view); code != http.StatusOK || view.State != hubapi.SessionClosed ||
		view.CloseReason != string(v1.SessionDiskPressure) || view.ClosedAt == nil || !view.ClosedAt.Equal(at.Truncate(time.Millisecond)) {
		t.Errorf("after the runner's report: %d %+v", code, view)
	}
}

// A session no runner holds yet closes on the hub at once, and the runs
// waiting in it are cancelled: none may start a closed session.
func TestCloseAnUnboundSession(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	f.enqueue(t, run("a", "s1"))
	var view hubapi.Session
	if code, e := f.api(t, "POST", "/sessions/s1/close", tok, nil, &view); code != http.StatusOK ||
		view.State != hubapi.SessionClosed || view.CloseReason != "closed" || view.RunnerID != "" {
		t.Fatalf("close: %d %+v %+v", code, e, view)
	}
	if got := f.state(t, "a"); got != "cancelled" {
		t.Errorf("the queued run is %s", got)
	}
	cred := f.register(t, "r1")
	if res := f.mustSync(t, "r1", cred, first("r1", 1)); len(res.Runs) != 0 || len(closes(res)) != 0 {
		t.Errorf("a closed unbound session reached a runner: %+v", res)
	}
}
