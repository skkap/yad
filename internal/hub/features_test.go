package hub

import (
	"net/http"
	"slices"
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
	kinds := kindsOf(f.mustSync(t, "new", now, req("new", 1, claimed("n1")...)))
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
	ahead := f.clock.t.Add(time.Hour)
	later := run("later", "s1")
	later.StartAt = &ahead
	// A moment already past leaves nothing to hold back, so the gate must not
	// strand it: a fleet without the feature would keep it queued for good.
	passed := f.clock.t.Add(-time.Hour)
	elapsed := run("elapsed", "s3")
	elapsed.StartAt = &passed
	f.enqueue(t, later, run("now", "s2"), elapsed)

	got := ids(f.mustSync(t, "old", old, req("old", 3)).Runs)
	slices.Sort(got)
	if !slices.Equal(got, []string{"elapsed", "now"}) {
		t.Fatalf("offered %v to a runner without %q, want [elapsed now]", got, capability.FeatureStartAt)
	}
	if s := f.state(t, "later"); s != "queued" {
		t.Errorf("run later is %s, want queued", s)
	}

	cred := f.register(t, "r1")
	if got := ids(f.mustSync(t, "r1", cred, first("r1", 2)).Runs); len(got) != 1 || got[0] != "later" {
		t.Fatalf("offered %v to a runner with %q, want [later]", got, capability.FeatureStartAt)
	}
}

// A runner without the effort feature drops a field it does not know and runs
// the harness at its default: the run succeeds, and nothing says it was not
// what was asked. So an effort run waits for a runner that advertises it —
// unlike a start moment, for as long as it takes.
func TestAnEffortRunIsOfferedOnlyToARunnerThatAppliesIt(t *testing.T) {
	f := newFixture(t)
	old := f.register(t, "old")
	f.mustSync(t, "old", old, downgraded("old", 3, capability.FeatureEffort))
	hard := run("hard", "s1")
	hard.Effort = "high"
	f.enqueue(t, hard, run("plain", "s2"))

	if got := ids(f.mustSync(t, "old", old, req("old", 3)).Runs); !slices.Equal(got, []string{"plain"}) {
		t.Fatalf("offered %v to a runner without %q, want [plain]", got, capability.FeatureEffort)
	}
	// Asked again, still not: nothing about the run changes with time.
	if got := ids(f.mustSync(t, "old", old, req("old", 3, claimed("plain")...)).Runs); len(got) != 0 {
		t.Fatalf("offered %v to a runner without %q on its next sync", got, capability.FeatureEffort)
	}
	if s := f.state(t, "hard"); s != "queued" {
		t.Errorf("run hard is %s, want queued", s)
	}

	cred := f.register(t, "r1")
	res := f.mustSync(t, "r1", cred, first("r1", 2))
	if got := ids(res.Runs); len(got) != 1 || got[0] != "hard" {
		t.Fatalf("offered %v to a runner with %q, want [hard]", got, capability.FeatureEffort)
	}
	if res.Runs[0].Effort != "high" {
		t.Errorf("the offer carries effort %q, want high", res.Runs[0].Effort)
	}
}

// As for a start moment: an offer is the one thing a later sync cannot take
// back, so a runner whose document the hub has asked to replace is offered no
// effort run until it arrives.
func TestAnEffortRunWaitsForADocumentTheHubKnowsIsStale(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.mustSync(t, "r1", cred, first("r1", 1))
	hard := run("hard", "s1")
	hard.Effort = "high"
	f.enqueue(t, hard)

	moved := req("r1", 1)
	moved.Fingerprint = "fp-r1-moved"
	if got := ids(f.mustSync(t, "r1", cred, moved).Runs); len(got) != 0 {
		t.Errorf("offered %v against a document the hub had just asked to replace", got)
	}
	answer := first("r1", 1)
	answer.Fingerprint = moved.Fingerprint
	if got := ids(f.mustSync(t, "r1", cred, answer).Runs); len(got) != 1 || got[0] != "hard" {
		t.Errorf("offered %v once the document arrived, want [hard]", got)
	}
}

// downgraded is a sync carrying a document with one feature taken out of it —
// a runner restarted under an older binary, which keeps its credential.
func downgraded(id string, free int, without string) v1.SyncRequest {
	r := first(id, free)
	var kept []string
	for _, f := range r.Capabilities.ProtocolFeatures {
		if f != without {
			kept = append(kept, f)
		}
	}
	r.Capabilities.ProtocolFeatures = kept
	return r
}

// The enqueue-time check is against the document the runner had then. What
// reaches it is decided by the document it has now, so a control queued for a
// runner that has since dropped the feature is held rather than spent on it.
func TestAControlIsHeldBackFromARunnerThatDowngraded(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	f.held(t, "r1", cred, "a", first("r1", 1))
	if code, e := f.api(t, "POST", "/runs/a/steer", tok, hubapi.SteerRequest{Text: "try the other file"}, nil); code != http.StatusOK {
		t.Fatalf("steer: %d %+v", code, e)
	}

	// The same runner, now reporting a build without steer.
	down := downgraded("r1", 1, capability.FeatureSteer)
	down.Runs = claimed("a")
	if got := kindsOf(f.mustSync(t, "r1", cred, down)); got[v1.ControlSteer] {
		t.Error("a steer reached a runner that no longer advertises it")
	}
	// Held, not dropped: the steer is still there for the runner it was meant
	// for. A steer delivered twice would be read twice, so this is the only
	// safe way to answer a downgrade.
	back := first("r1", 1)
	back.Runs = claimed("a")
	if got := kindsOf(f.mustSync(t, "r1", cred, back)); !got[v1.ControlSteer] {
		t.Error("the steer was lost by the sync that could not deliver it")
	}
}

// The same for the two controls a sync emits on its own rather than from the
// run's queue.
func TestDrainAndCloseAreHeldBackFromARunnerThatDowngraded(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	f.held(t, "r1", cred, "a", first("r1", 1))
	for _, path := range []string{"/runners/r1/drain", "/sessions/s-a/close"} {
		if code, e := f.api(t, "POST", path, tok, nil, nil); code != http.StatusOK {
			t.Fatalf("%s: %d %+v", path, code, e)
		}
	}

	for _, c := range []struct {
		feature string
		kind    v1.ControlKind
	}{
		{capability.FeatureDrain, v1.ControlDrain},
		{capability.FeatureCloseSession, v1.ControlCloseSession},
	} {
		down := downgraded("r1", 1, c.feature)
		down.Runs = claimed("a")
		if got := kindsOf(f.mustSync(t, "r1", cred, down)); got[c.kind] {
			t.Errorf("a %s reached a runner that no longer advertises %q", c.kind, c.feature)
		}
	}
	// Both requests still stand, and the runner that advertises them again
	// hears both: neither was answered by a sync that could not deliver it.
	back := first("r1", 1)
	back.Runs = claimed("a")
	got := kindsOf(f.mustSync(t, "r1", cred, back))
	if !got[v1.ControlDrain] || !got[v1.ControlCloseSession] {
		t.Errorf("controls after the runner came back: %v", got)
	}
}

// v1 says the document goes with the first sync after a fingerprint move. A
// runner that moves its fingerprint and sends nothing leaves the hub holding a
// document it knows is stale, and a control gated on a feature must not be
// spent against it — least of all a steer, which delivery consumes.
func TestAGatedControlWaitsForADocumentTheHubKnowsIsStale(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	cred := f.register(t, "r1")
	f.held(t, "r1", cred, "a", first("r1", 1))
	if code, e := f.api(t, "POST", "/runs/a/steer", tok, hubapi.SteerRequest{Text: "try the other file"}, nil); code != http.StatusOK {
		t.Fatalf("steer: %d %+v", code, e)
	}

	moved := req("r1", 1, claimed("a")...)
	moved.Fingerprint = "fp-r1-moved"
	got := kindsOf(f.mustSync(t, "r1", cred, moved))
	if !got[v1.ControlReportCapabilities] {
		t.Error("a moved fingerprint did not ask for the document")
	}
	if got[v1.ControlSteer] {
		t.Error("a steer was spent against a document the hub had just asked to replace")
	}

	// The document arrives, and with it the steer — held, not consumed.
	answer := first("r1", 1)
	answer.Fingerprint = moved.Fingerprint
	answer.Runs = claimed("a")
	if got := kindsOf(f.mustSync(t, "r1", cred, answer)); !got[v1.ControlSteer] {
		t.Error("the steer was lost by the sync that waited for the document")
	}
}

// A withheld control comes round again next sync; a run started early has
// started. So a run whose moment is still ahead waits for the document too.
func TestAStartAtRunWaitsForADocumentTheHubKnowsIsStale(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.mustSync(t, "r1", cred, first("r1", 1))
	ahead := f.clock.t.Add(time.Hour)
	later := run("later", "s1")
	later.StartAt = &ahead
	f.enqueue(t, later)

	moved := req("r1", 1)
	moved.Fingerprint = "fp-r1-moved"
	res := f.mustSync(t, "r1", cred, moved)
	if got := ids(res.Runs); len(got) != 0 {
		t.Errorf("offered %v against a document the hub had just asked to replace", got)
	}
	if !kindsOf(res)[v1.ControlReportCapabilities] {
		t.Error("a moved fingerprint did not ask for the document")
	}

	// The document lands, and with it the run: withheld for one sync, not lost.
	answer := first("r1", 1)
	answer.Fingerprint = moved.Fingerprint
	if got := ids(f.mustSync(t, "r1", cred, answer).Runs); len(got) != 1 || got[0] != "later" {
		t.Errorf("offered %v once the document arrived, want [later]", got)
	}
}

func kindsOf(res v1.SyncResponse) map[v1.ControlKind]bool {
	out := map[v1.ControlKind]bool{}
	for _, c := range res.Controls {
		out[c.Kind] = true
	}
	return out
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
	if feature, _ := controlFeature(v1.ControlUpdate, "r1"); feature != "" {
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
