package hub

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// req is a sync as a runner sends it: the capability document on the first
// one, then only the fingerprint.
func req(id string, free int, held ...v1.HeldRun) v1.SyncRequest {
	return v1.SyncRequest{RunnerID: id, Fingerprint: "fp-" + id, Health: v1.Health{FreeCapacity: v1.Capacity{Total: free}}, Runs: held}
}

func first(id string, free int) v1.SyncRequest {
	r := req(id, free)
	d := doc(id)
	r.Capabilities = &d
	return r
}

func claimed(ids ...string) []v1.HeldRun {
	var out []v1.HeldRun
	for _, id := range ids {
		out = append(out, v1.HeldRun{RunID: id, State: v1.RunClaimed})
	}
	return out
}

func ids(runs []v1.Run) []string {
	var out []string
	for _, r := range runs {
		out = append(out, r.RunID)
	}
	return out
}

func cancels(res v1.SyncResponse) []string {
	var out []string
	for _, c := range res.Controls {
		if c.Kind == v1.ControlCancel {
			out = append(out, c.RunID)
		}
	}
	return out
}

func TestOffersNeverExceedFreeCapacity(t *testing.T) {
	for _, tc := range []struct {
		name string
		free v1.Capacity
		want []string
	}{
		{"none free", v1.Capacity{Total: 0}, nil},
		{"two free", v1.Capacity{Total: 2}, []string{"a", "b"}},
		{"more free than queued", v1.Capacity{Total: 9}, []string{"a", "b", "c"}},
		{"capped harness", v1.Capacity{Total: 3, ByHarness: map[string]int{"claude": 1}}, []string{"a"}},
		{"other harness capped", v1.Capacity{Total: 2, ByHarness: map[string]int{"codex": 0}}, []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			cred := f.register(t, "r1")
			f.enqueue(t, run("a", "s1"), run("b", "s2"), run("c", "s3"))
			r := first("r1", 0)
			r.Health.FreeCapacity = tc.free
			res := f.mustSync(t, "r1", cred, r)
			if got := ids(res.Runs); !slices.Equal(got, tc.want) {
				t.Errorf("offered %v, want %v", got, tc.want)
			}
			if res.NextSyncMS != 15000 || res.LeaseMS != 60000 {
				t.Errorf("timings %d/%d", res.NextSyncMS, res.LeaseMS)
			}
		})
	}
}

// A runner is offered only what it can drive, and a session only ever has
// one run out at a time.
func TestOffersOnlyDrivableRunsOnePerSession(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	codex := run("x", "sx")
	codex.Harness = "codex" // recognised, not first-class, in doc()
	gemini := run("g", "sg")
	gemini.Harness = "gemini" // absent from doc()
	second := run("a2", "s1")
	second.Session.New = false
	f.enqueue(t, codex, gemini, run("a1", "s1"), second)
	res := f.mustSync(t, "r1", cred, first("r1", 4))
	if got := ids(res.Runs); !slices.Equal(got, []string{"a1"}) {
		t.Errorf("offered %v, want only a1", got)
	}
}

// Claim by listing: an offer listed back is claimed and binds its session to
// this runner; one left out was never received and is offered again — here,
// or to another runner — and is never held by two at once.
func TestClaimByListingAndReoffer(t *testing.T) {
	f := newFixture(t)
	c1, c2 := f.register(t, "r1"), f.register(t, "r2")
	f.enqueue(t, run("a", "s1"), run("b", "s2"))

	res := f.mustSync(t, "r1", c1, first("r1", 2))
	if got := ids(res.Runs); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("offered %v", got)
	}
	// r1 received only a (say the response was cut short): it lists a alone.
	res = f.mustSync(t, "r1", c1, req("r1", 0, claimed("a")...))
	if f.state(t, "a") != "claimed" || f.state(t, "b") != "queued" {
		t.Fatalf("a=%s b=%s after listing a alone", f.state(t, "a"), f.state(t, "b"))
	}
	if len(res.Runs) != 0 {
		t.Errorf("offered %v with no free capacity", ids(res.Runs))
	}
	if s, _ := f.store.GetSession(context.Background(), "s1"); s.RunnerID.String != "r1" {
		t.Errorf("session s1 bound to %q", s.RunnerID.String)
	}

	// b goes to r2 now.
	res = f.mustSync(t, "r2", c2, first("r2", 1))
	if got := ids(res.Runs); !slices.Equal(got, []string{"b"}) {
		t.Fatalf("r2 offered %v", got)
	}
	// A late r1 that claims b after all is told to cancel it: b is r2's.
	res = f.mustSync(t, "r1", c1, req("r1", 0, claimed("a", "b")...))
	if got := cancels(res); !slices.Equal(got, []string{"b"}) {
		t.Errorf("cancels %v, want [b]", got)
	}
	res = f.mustSync(t, "r2", c2, req("r2", 0, claimed("b")...))
	if len(cancels(res)) != 0 || f.state(t, "b") != "claimed" {
		t.Errorf("r2 claiming its own offer: cancels %v, state %s", cancels(res), f.state(t, "b"))
	}
}

// A session is resumable only where it lives: once its first run is claimed,
// its later runs are offered to that runner and no other.
func TestSessionAffinity(t *testing.T) {
	f := newFixture(t)
	c1, c2 := f.register(t, "r1"), f.register(t, "r2")
	f.enqueue(t, run("a1", "s1"))
	f.mustSync(t, "r1", c1, first("r1", 1))
	f.mustSync(t, "r1", c1, req("r1", 0, claimed("a1")...))
	if _, err := f.store.DB.Exec(`UPDATE runs SET state = 'succeeded' WHERE id = 'a1'`); err != nil {
		t.Fatal(err)
	}
	next := run("a2", "s1")
	next.Session.New = false
	f.enqueue(t, next)
	if res := f.mustSync(t, "r2", c2, first("r2", 4)); len(res.Runs) != 0 {
		t.Errorf("r2 was offered %v from r1's session", ids(res.Runs))
	}
	if res := f.mustSync(t, "r1", c1, req("r1", 1)); !slices.Equal(ids(res.Runs), []string{"a2"}) {
		t.Errorf("r1 offered %v, want a2", ids(res.Runs))
	}
}

// Four missed intervals and the lease lapses: the run is lost, decided by the
// hub, and a runner that comes back listing it is told to stop.
func TestLapsedLeaseLosesTheRun(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.enqueue(t, run("a", "s1"))
	f.mustSync(t, "r1", cred, first("r1", 1))
	f.mustSync(t, "r1", cred, req("r1", 0, v1.HeldRun{RunID: "a", State: v1.RunRunning}))

	f.clock.Advance(4*DefaultSyncInterval - time.Millisecond)
	if err := f.hub.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, "a"); got != "running" {
		t.Fatalf("lost early: %s", got)
	}
	f.clock.Advance(time.Millisecond)
	if err := f.hub.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.state(t, "a"); got != "lost" {
		t.Fatalf("state after four missed intervals = %s, want lost", got)
	}
	res := f.mustSync(t, "r1", cred, req("r1", 0, v1.HeldRun{RunID: "a", State: v1.RunRunning}))
	if got := cancels(res); !slices.Equal(got, []string{"a"}) {
		t.Errorf("cancels %v, want [a]", got)
	}
	if got := f.state(t, "a"); got != "lost" {
		t.Errorf("a lost run came back as %s", got)
	}
}

// Each sync renews the lease, so a runner that keeps syncing keeps its runs
// however long they take.
func TestSyncRenewsTheLease(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.enqueue(t, run("a", "s1"))
	f.mustSync(t, "r1", cred, first("r1", 1))
	for range 10 {
		f.mustSync(t, "r1", cred, req("r1", 0, v1.HeldRun{RunID: "a", State: v1.RunRunning}))
		f.clock.Advance(3 * DefaultSyncInterval)
	}
	if got := f.state(t, "a"); got != "running" {
		t.Errorf("state = %s", got)
	}
}

// An offer to a runner that never syncs again is withdrawn when its lease
// lapses, and the run goes to another runner.
func TestWithdrawnOfferGoesElsewhere(t *testing.T) {
	f := newFixture(t)
	c1, c2 := f.register(t, "r1"), f.register(t, "r2")
	f.enqueue(t, run("a", "s1"))
	f.mustSync(t, "r1", c1, first("r1", 1))
	if res := f.mustSync(t, "r2", c2, first("r2", 1)); len(res.Runs) != 0 {
		t.Fatalf("offered to two runners: %v", ids(res.Runs))
	}
	f.clock.Advance(4 * DefaultSyncInterval)
	if res := f.mustSync(t, "r2", c2, req("r2", 1)); !slices.Equal(ids(res.Runs), []string{"a"}) {
		t.Errorf("r2 offered %v after r1's offer lapsed", ids(res.Runs))
	}
}

func TestFingerprintMoveAsksForTheDocument(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	wants := func(res v1.SyncResponse) bool {
		return slices.ContainsFunc(res.Controls, func(c v1.Control) bool { return c.Kind == v1.ControlReportCapabilities })
	}
	if res := f.mustSync(t, "r1", cred, first("r1", 0)); wants(res) {
		t.Error("asked for the document it was just sent")
	}
	if res := f.mustSync(t, "r1", cred, req("r1", 0)); wants(res) {
		t.Error("asked with an unchanged fingerprint")
	}
	moved := req("r1", 0)
	moved.Fingerprint = "fp-new"
	if res := f.mustSync(t, "r1", cred, moved); !wants(res) {
		t.Error("fingerprint moved and the hub did not ask")
	}
	// Asked again until the document arrives, even if the fingerprint settles.
	if res := f.mustSync(t, "r1", cred, moved); !wants(res) {
		t.Error("stopped asking before the document arrived")
	}
	d := doc("r1")
	d.Labels = []string{"gpu"}
	moved.Capabilities = &d
	if res := f.mustSync(t, "r1", cred, moved); wants(res) {
		t.Error("asked in the answer to the document itself")
	}
	r, _ := f.store.GetRunner(context.Background(), "r1")
	if r.Fingerprint != "fp-new" || !strings.Contains(r.Capabilities, `"gpu"`) {
		t.Errorf("stored %s %s", r.Fingerprint, r.Capabilities)
	}
}

func TestSyncAuthentication(t *testing.T) {
	f := newFixture(t)
	c1 := f.register(t, "r1")
	f.register(t, "r2")
	for _, tc := range []struct {
		name, path, cred string
		body             v1.SyncRequest
		status           int
	}{
		{"no credential", "r1", "", req("r1", 0), http.StatusUnauthorized},
		{"unknown credential", "r1", "yadrun_nope", req("r1", 0), http.StatusUnauthorized},
		{"another runner's path", "r2", c1, req("r2", 0), http.StatusForbidden},
		{"body names another runner", "r1", c1, req("r2", 0), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, env := f.sync(t, tc.path, tc.cred, tc.body)
			if res.StatusCode != tc.status || env.Error.NextAction == "" {
				t.Errorf("%d %+v", res.StatusCode, env.Error)
			}
		})
	}
}
