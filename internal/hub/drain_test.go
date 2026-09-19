package hub

import (
	"context"
	"net/http"
	"testing"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
)

func hasDrain(res v1.SyncResponse) bool {
	for _, c := range res.Controls {
		if c.Kind == v1.ControlDrain {
			return true
		}
	}
	return false
}

// yad hub drain on the hub's side: refused for a runner that would ignore it,
// repeated in every answer until the runner says it is draining, and no work
// offered meanwhile — while the runs it holds keep their leases.
func TestDrainRunner(t *testing.T) {
	f := newFixture(t)
	tok := f.admin(t, "cli")
	old := f.register(t, "old")
	f.mustSync(t, "old", old, stale("old", 1))
	cred := f.register(t, "r1")
	withDrain := first("r1", 2)
	withDrain.Capabilities.ProtocolFeatures = []string{capability.FeatureDrain}
	withDrain.Fingerprint = "fp-r1-drain"
	f.enqueue(t, run("held", "s1"))
	f.mustSync(t, "r1", cred, withDrain)
	sync := func(r v1.SyncRequest) v1.SyncResponse {
		r.Fingerprint = withDrain.Fingerprint
		return f.mustSync(t, "r1", cred, r)
	}
	sync(req("r1", 1, claimed("held")...))

	for _, c := range []struct {
		path string
		code int
	}{
		{"/runners/nope/drain", http.StatusNotFound},
		{"/runners/old/drain", http.StatusConflict},
	} {
		if code, e := f.api(t, "POST", c.path, tok, nil, nil); code != c.code {
			t.Errorf("%s: %d %+v, want %d", c.path, code, e, c.code)
		}
	}

	var view hubapi.Runner
	if code, e := f.api(t, "POST", "/runners/r1/drain", tok, nil, &view); code != http.StatusOK || view.DrainRequestedAt == nil || view.Draining {
		t.Fatalf("drain: %d %+v %+v", code, e, view)
	}
	f.enqueue(t, run("new", "s2"))
	// Repeated: a lost answer must not lose the drain.
	for range 2 {
		res := sync(req("r1", 1, running("held")...))
		if !hasDrain(res) || len(res.Runs) != 0 {
			t.Fatalf("while asked to drain: %+v", res)
		}
	}
	draining := req("r1", 1, running("held")...)
	draining.Health.Draining = true
	res := sync(draining)
	if hasDrain(res) || len(res.Runs) != 0 {
		t.Errorf("once draining: %+v", res)
	}
	r, err := f.store.GetRunner(context.Background(), "r1")
	if err != nil || r.DrainRequestedAt.Valid {
		t.Errorf("request after the runner answered: %+v %v", r.DrainRequestedAt, err)
	}
	// Still draining, and still offered nothing, whatever capacity it
	// claims; its run is held all the while.
	if res := sync(draining); len(res.Runs) != 0 {
		t.Errorf("offered %d to a draining runner", len(res.Runs))
	}
	if got := f.state(t, "held"); got != "running" {
		t.Errorf("held run is %s", got)
	}
	// The runner drains, exits, and its health still says draining. A drain
	// asked for now is recorded all the same: only a sync after it answers.
	var again hubapi.Runner
	if code, _ := f.api(t, "POST", "/runners/r1/drain", tok, nil, &again); code != http.StatusOK || !again.Draining || again.DrainRequestedAt == nil {
		t.Errorf("drain again: %d %+v — want the request recorded beside the stale health", code, again)
	}
	if res := sync(req("r1", 1, running("held")...)); !hasDrain(res) || len(res.Runs) != 0 {
		t.Errorf("the next process, asked to drain after the last one exited: %+v", res)
	}
	sync(draining)
	// Answered; the process after that is offered work again.
	if res := sync(req("r1", 1, running("held")...)); hasDrain(res) || len(res.Runs) != 1 {
		t.Errorf("the process after the answer: %+v", res)
	}
}

func running(ids ...string) []v1.HeldRun {
	var out []v1.HeldRun
	for _, id := range ids {
		out = append(out, v1.HeldRun{RunID: id, State: v1.RunRunning})
	}
	return out
}
