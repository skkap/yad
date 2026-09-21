package runner

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/hub"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store/db"
)

// floored serves this env's hub store again with a version floor, as an
// operator gets by restarting `yad hub serve --min-version`: the runners it
// already registered are unchanged, and what they reported is what they are
// judged by.
func (e *env) floored(t *testing.T, min string, cred string) *hubclient.Client {
	t.Helper()
	srv := httptest.NewServer(hub.New(hub.Options{Store: e.hubStore, MinVersion: min}))
	t.Cleanup(srv.Close)
	c, err := hubclient.New(srv.URL+hub.BasePath, cred)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A runner below the hub's floor is turned away at register and at sync, and
// hears what to do about it. The refusal is fatal: retrying cannot change it,
// so the loop stops rather than syncing at a hub that will never take it.
func TestAHubRefusesARunnerBelowItsMinVersion(t *testing.T) {
	e := newEnv(t)
	l := e.loopVersion(t, 1, "0.3.9")
	mustSync(t, l) // the hub as it was, with no floor

	l.Hub = e.floored(t, "0.4.0", e.cred)
	_, err := l.SyncOnce(context.Background())
	if err == nil {
		t.Fatal("the sync was answered by a hub this runner is too old for")
	}
	if code := hubclient.Code(err); code != v1.CodeVersionTooOld {
		t.Fatalf("code %q, want %q: %v", code, v1.CodeVersionTooOld, err)
	}
	if !fatal(err) {
		t.Error("the loop would retry a refusal no retry can change")
	}
	for _, want := range []string{"0.4.0", "0.3.9", "yad upgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q is missing from %q", want, err)
		}
	}

	// And the same at register, where an upgraded runner would arrive.
	anon := e.floored(t, "0.4.0", "")
	doc := drivableDoc("new", 1)
	doc.YadVersion = "0.3.9"
	if _, err := anon.Register(context.Background(), e.token(t), v1.RegisterRequest{Capabilities: doc}); hubclient.Code(err) != v1.CodeVersionTooOld {
		t.Fatalf("register: %v", err)
	}
}

// Decision 0018: v1 has no self-update, and the reserved control must stay
// inert — a runner that acted on one would be replacing its own binary on a
// remote's say-so.
func TestTheUpdateControlIsIgnored(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	ad := fakeHarness(fake.Script{Hang: true})
	x := e.executor(ad)
	running(t, e, l, x, "a")

	x.Control(context.Background(), "hub", v1.Control{Kind: v1.ControlUpdate, RunID: "a"})
	mustSync(t, l)
	if res, ok := outboxResult(t, e, "a"); ok {
		t.Fatalf("the update control ended the run: %+v", res)
	}
	held, err := e.store.GetRun(context.Background(), db.GetRunParams{Connection: "hub", ID: "a"})
	if err != nil || v1.RunState(held.State).IsTerminal() {
		t.Fatalf("run a is %q, %v; want one still going", held.State, err)
	}

	// The run is still there to be stopped the one way a runner is stopped.
	if _, err := e.api(t).Cancel(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	mustSync(t, l)
	ended(t, x)
	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunCancelled {
		t.Fatalf("result %+v, %v", res, ok)
	}
	turns := ad.Turns()
	if len(turns) != 1 {
		t.Fatalf("%d turns", len(turns))
	}
	if n, _ := fake.Rungs(turns[0]); n != 1 {
		t.Errorf("%d interrupts, want the cancel's one and nothing from the update", n)
	}
}
