package hub

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
)

// held gets one run onto a runner and holds it there, through the sync that
// claims it.
func (f *fixture) held(t *testing.T, runner, cred, runID string, firstSync v1.SyncRequest) {
	t.Helper()
	f.mustSync(t, runner, cred, firstSync)
	f.enqueue(t, run(runID, "s-"+runID))
	f.mustSync(t, runner, cred, req(runner, 1))
	f.mustSync(t, runner, cred, req(runner, 1, claimed(runID)...))
	if s := f.state(t, runID); s != "claimed" {
		t.Fatalf("run %s is %s, want claimed", runID, s)
	}
}

// A control is not acknowledged, so one sent to a runner that would ignore it
// reads to its caller exactly like one that landed. The service API refuses
// instead, and says what to do with the runner.
func TestSteerAndInterruptNeedTheFeature(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	old := f.register(t, "old")
	f.held(t, "old", old, "o1", stale("old", 1))
	now := f.register(t, "new")
	f.held(t, "new", now, "n1", first("new", 1))

	for _, c := range []struct {
		path    string
		body    any
		feature string
	}{
		{"/runs/o1/steer", hubapi.SteerRequest{Text: "try the other file"}, capability.FeatureSteer},
		{"/runs/o1/interrupt", nil, capability.FeatureInterrupt},
	} {
		code, e := f.api(t, "POST", c.path, tok, c.body, nil)
		if code != http.StatusConflict || e.Code != v1.CodeConflict {
			t.Errorf("%s: %d %+v, want 409 %s", c.path, code, e, v1.CodeConflict)
		}
		if !strings.Contains(e.Message, c.feature) {
			t.Errorf("%s: message %q does not name the feature", c.path, e.Message)
		}
		if !strings.Contains(e.NextAction, "upgrade yad") {
			t.Errorf("%s: next action %q does not say what to do about the runner", c.path, e.NextAction)
		}
	}
	// Nothing was queued for the runner that would have ignored it.
	if res := f.mustSync(t, "old", old, req("old", 1, claimed("o1")...)); len(res.Controls) != 0 {
		t.Errorf("controls for a runner that advertises neither: %+v", res.Controls)
	}
	// And a runner that advertises both still gets them.
	for _, c := range []struct {
		path string
		body any
	}{
		{"/runs/n1/steer", hubapi.SteerRequest{Text: "try the other file"}},
		{"/runs/n1/interrupt", nil},
	} {
		if code, e := f.api(t, "POST", c.path, tok, c.body, nil); code != http.StatusOK {
			t.Fatalf("%s: %d %+v", c.path, code, e)
		}
	}
	kinds := map[v1.ControlKind]bool{}
	for _, c := range f.mustSync(t, "new", now, req("new", 1, claimed("n1")...)).Controls {
		kinds[c.Kind] = true
	}
	if !kinds[v1.ControlSteer] || !kinds[v1.ControlInterrupt] {
		t.Errorf("controls delivered: %v", kinds)
	}
}

// start_at is a promise the runner keeps, not the hub: a runner that never
// made it would start the run the moment it arrived.
func TestStartAtIsOfferedOnlyToARunnerThatHoldsIt(t *testing.T) {
	f := newFixture(t)
	old := f.register(t, "old")
	f.mustSync(t, "old", old, stale("old", 1))
	at := f.clock.t.Add(time.Hour)
	later := run("later", "s1")
	later.StartAt = &at
	f.enqueue(t, later, run("now", "s2"))

	// The run with a start time is passed over; the one without is not, so
	// this is a gate and not a stuck queue.
	if got := ids(f.mustSync(t, "old", old, req("old", 2)).Runs); len(got) != 1 || got[0] != "now" {
		t.Fatalf("offered %v to a runner without %q, want only [now]", got, capability.FeatureStartAt)
	}
	if s := f.state(t, "later"); s != "queued" {
		t.Errorf("run later is %s, want queued", s)
	}

	cred := f.register(t, "r1")
	if got := ids(f.mustSync(t, "r1", cred, first("r1", 2)).Runs); len(got) != 1 || got[0] != "later" {
		t.Fatalf("offered %v to a runner with %q, want [later]", got, capability.FeatureStartAt)
	}
}

// Decision 0018: v1 has no self-update. The control name is reserved so the
// machinery is there when it is built, and nothing acts on it meanwhile.
func TestUpdateControlIsReservedAndUnhandled(t *testing.T) {
	if !containsKind(v1.ControlKinds(), v1.ControlUpdate) {
		t.Fatal("the update control is no longer reserved in the protocol")
	}
	for _, f := range capability.Features() {
		if f == string(v1.ControlUpdate) {
			t.Fatal("a runner advertises update; v1 has no self-update (decision 0018)")
		}
	}
	// Nothing in the hub ever queues one: a control kind reaches a runner
	// only through the service API, which has no operation for update.
	if feature, _ := controlFeature(v1.ControlUpdate); feature != "" {
		t.Errorf("update was given a feature gate, which implies something sends it")
	}
}

func containsKind(kinds []v1.ControlKind, want v1.ControlKind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}
