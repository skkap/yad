package runner

import (
	"context"
	"fmt"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hubclient"
)

// hubs is one runner registered with several `yad hub`s, all of them drawing
// on one capacity pool and one executor — the shape decision 0003 promises,
// and the only place starvation can show.
type hubs struct {
	pool *Pool
	exec *executor
	env  map[string]*env
	loop map[string]*Loop
	// ended is how many of the executor's claims have had their capacity
	// released, so endAll releases each run exactly once.
	ended int
}

// newHubs starts a hub per name, in the order given, and registers one runner
// with each. The connections share the first hub's store and executor, as a
// runner's do.
func newHubs(t *testing.T, capacity int, caps map[string]int, names ...string) *hubs {
	t.Helper()
	ctx := context.Background()
	h := &hubs{env: map[string]*env{}, loop: map[string]*Loop{}}
	var first *env
	for _, name := range names {
		e := newEnv(t)
		if first == nil {
			first, h.exec = e, e.exec
		}
		h.env[name] = e
	}
	id, err := first.paths.RunnerID()
	if err != nil {
		t.Fatal(err)
	}
	doc := drivableDoc(id, capacity)
	h.pool = NewPool(doc.Capacity)
	for _, name := range names {
		e := h.env[name]
		anon, err := hubclient.New(e.url, "")
		if err != nil {
			t.Fatal(err)
		}
		res, err := anon.Register(ctx, e.token(t), v1.RegisterRequest{Capabilities: doc})
		if err != nil {
			t.Fatal(err)
		}
		c, err := hubclient.New(e.url, res.RunnerCredential)
		if err != nil {
			t.Fatal(err)
		}
		h.pool.Join(name, caps[name])
		h.loop[name] = &Loop{
			Connection: name, RunnerID: id, Hub: c, Store: first.store, Pool: h.pool,
			Capabilities: func() v1.Capabilities { return doc }, Executor: h.exec, Clock: first.clock,
		}
	}
	return h
}

// queue puts n runs in that hub's queue, each in a session of its own.
func (h *hubs) queue(t *testing.T, name string, n int) {
	t.Helper()
	for i := range n {
		id := fmt.Sprintf("%s-%d", name, i)
		h.env[name].enqueue(t, testRun(id, "s-"+id))
	}
}

func (h *hubs) sync(t *testing.T, name string) {
	t.Helper()
	mustSync(t, h.loop[name])
}

// endAll ends every run the executor has been handed and not yet ended, as a
// harness finishing would: the capacity goes back to the pool.
func (h *hubs) endAll() {
	h.exec.mu.Lock()
	defer h.exec.mu.Unlock()
	for _, c := range h.exec.started[h.ended:] {
		c.Release()
	}
	h.ended = len(h.exec.started)
}

// started is the runs of one connection the executor was handed, in order.
func (h *hubs) started(conn string) []string {
	h.exec.mu.Lock()
	defer h.exec.mu.Unlock()
	var out []string
	for _, c := range h.exec.started {
		if c.Connection == conn {
			out = append(out, c.Run.RunID)
		}
	}
	return out
}

// Starvation is the thing round-robin exists to prevent: a hub with a deep
// queue, syncing first and twice as often, must not take a pool the quieter
// hub then never gets a unit of. Before the capacity was dealt in turn, the
// first sync took all four units and the quiet hub was offered none of them,
// for as long as the busy hub's runs kept coming.
func TestUnevenDemandDoesNotStarveTheQuieterHub(t *testing.T) {
	h := newHubs(t, 4, nil, "busy", "quiet")
	h.queue(t, "busy", 20)
	h.queue(t, "quiet", 3)

	for range 6 {
		// The adversarial order: the busy hub asks first, and asks more often.
		h.sync(t, "busy")
		h.sync(t, "busy")
		h.sync(t, "quiet")
	}
	busy, quiet := h.started("busy"), h.started("quiet")
	if len(quiet) != 2 {
		t.Errorf("the quiet hub had %d runs started %v, want its half of the 4: it is being starved", len(quiet), quiet)
	}
	if len(busy) != 2 {
		t.Errorf("the busy hub had %d runs started %v, want its half of the 4", len(busy), busy)
	}
	if free := h.pool.Free(); free != 0 {
		t.Errorf("%d units idle with both hubs queueing work", free)
	}
}

// The other half of the same promise: as runs end, the freed capacity goes
// round rather than back to whoever asks first.
func TestFreedCapacityGoesRoundTheHubs(t *testing.T) {
	h := newHubs(t, 2, nil, "busy", "quiet")
	h.queue(t, "busy", 40)
	h.queue(t, "quiet", 40)

	const rounds = 8
	for range rounds {
		// Three syncs to one: the busy hub both claims and lists first.
		h.sync(t, "busy")
		h.sync(t, "busy")
		h.sync(t, "busy")
		h.sync(t, "quiet")
		h.sync(t, "quiet")
		h.endAll()
	}
	busy, quiet := len(h.started("busy")), len(h.started("quiet"))
	// One run each per round: the pool holds two and each hub's turn comes
	// round once. The bar is deliberately looser than that — what must not
	// happen is the quiet hub falling behind however often the busy one asks.
	if quiet*3 < busy*2 {
		t.Errorf("the busy hub started %d runs and the quiet one %d: the freed capacity is not going round", busy, quiet)
	}
	if quiet < rounds/2 {
		t.Errorf("the quiet hub started %d runs over %d rounds", quiet, rounds)
	}
}

// A connection whose loop has stopped or is draining holds no turn: its share
// would be capacity nothing can use.
func TestAStoppedConnectionYieldsItsShare(t *testing.T) {
	h := newHubs(t, 4, nil, "busy", "gone")
	h.queue(t, "busy", 20)
	h.pool.Pass("gone")

	h.sync(t, "busy")
	h.sync(t, "busy")
	if got := len(h.started("busy")); got != 4 {
		t.Errorf("the only live connection started %d runs of a pool of 4", got)
	}
}

// The acceptance criterion, at the loop: the owner's cap on a connection is
// what the sync asks for, so a run over the cap is never claimed and then
// found to be over it. The hub here offers five runs whatever the sync says,
// as a broken or hostile one would.
func TestAConnectionCapBoundsWhatTheSyncAsksFor(t *testing.T) {
	e := newEnv(t)
	const cap = 1
	var offers []v1.Run
	for i := range 5 {
		offers = append(offers, testRun(fmt.Sprintf("r%d", i), fmt.Sprintf("s%d", i)))
	}
	hub := &scriptedHub{offer: offers}
	doc := drivableDoc("r", 4)
	pool := NewPool(doc.Capacity)
	pool.Join("hub", cap)
	l := &Loop{Connection: "hub", RunnerID: "r", Hub: hub, Store: e.store, Pool: pool,
		Capabilities: func() v1.Capabilities { return doc }, Executor: e.exec, Clock: e.clock}

	for range 4 {
		mustSync(t, l)
	}
	// What a sync asks for, plus what it says it holds, never exceeds the cap:
	// the hub is never in a position to offer a run that would break it.
	for i, req := range hub.syncs {
		if held, free := len(req.Runs), req.Health.FreeCapacity.Total; held+free > cap {
			t.Errorf("sync %d asked for %d free while holding %d, past the cap of %d", i, free, held, cap)
		}
	}
	// Nothing was refused. A cap enforced after a claim would show here, as a
	// failed result for a run this runner had already taken from the hub.
	if len(hub.results) != 0 {
		t.Errorf("results sent %v, want none: the cap refused a run after claiming it", hub.results)
	}
	if got := e.exec.ids(); len(got) != cap {
		t.Fatalf("started %v, want %d under the cap", got, cap)
	}
	if free := pool.Free(); free != doc.Capacity.Total-cap {
		t.Errorf("the capped connection holds %d of the pool, want %d", doc.Capacity.Total-free, cap)
	}

	// The cap bounds what is held at once, not what the hub may ever send:
	// the next run is claimed once the first ends.
	e.exec.mu.Lock()
	first := e.exec.started[0]
	e.exec.mu.Unlock()
	first.Release()
	mustSync(t, l)
	mustSync(t, l)
	if got := e.exec.ids(); len(got) != 2 {
		t.Errorf("started %v after a run ended, want the next one claimed", got)
	}
	if len(hub.results) != 0 {
		t.Errorf("results sent %v, want none", hub.results)
	}
}
