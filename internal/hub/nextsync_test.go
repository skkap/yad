package hub

import (
	"net/http"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// A runner is asked back in 3 s only while the hub holds a queued run it
// would be offered once a run it is executing ends — the capacity that run
// holds, or the session that run is in, being all that stands in the way.
// Everything else keeps the interval the hub was configured with: an idle
// fleet, a run no one here could take, and a runner whose capacity nothing
// it holds is about to give back.
func TestNextSyncIsSoonerWhileARunWaitsForThisRunner(t *testing.T) {
	const quick, normal = 3000, 15000
	later := func(id, session string) v1.Run {
		r := run(id, session)
		r.Session.New = false
		return r
	}
	for _, tc := range []struct {
		name string
		// queue runs after r1 holds run a, in session s1, in state held.
		queue func(t *testing.T, f *fixture)
		held  v1.RunState
		free  int
		// sync is r1's sync after queue; nil is a sync listing a as held
		// with free capacity free.
		sync func(r v1.SyncRequest) v1.SyncRequest
		want int
	}{
		{name: "nothing queued", held: v1.RunRunning, want: normal},
		{
			name:  "a run capacity holds back",
			queue: func(t *testing.T, f *fixture) { f.enqueue(t, run("b", "s2")) },
			held:  v1.RunRunning, want: quick,
		},
		{
			name:  "a run capacity holds back, behind a claim",
			queue: func(t *testing.T, f *fixture) { f.enqueue(t, run("b", "s2")) },
			held:  v1.RunClaimed, want: quick,
		},
		{
			name:  "the next run of a session this runner is running",
			queue: func(t *testing.T, f *fixture) { f.enqueue(t, later("a2", "s1")) },
			held:  v1.RunRunning, free: 3, want: quick,
		},
		{
			// Waiting runs give their capacity back at an account's reset,
			// hours off: asked back every 3 s it would sync a thousand times
			// for nothing.
			name:  "a run held back only by runs waiting for a reset",
			queue: func(t *testing.T, f *fixture) { f.enqueue(t, run("b", "s2")) },
			held:  v1.RunWaiting, want: normal,
		},
		{
			name: "a harness this runner cannot drive",
			queue: func(t *testing.T, f *fixture) {
				g := run("g", "sg")
				g.Harness = "gemini"
				f.enqueue(t, g)
			},
			held: v1.RunRunning, want: normal,
		},
		{
			name: "a session bound to another runner",
			queue: func(t *testing.T, f *fixture) {
				c2 := f.register(t, "r2")
				f.enqueue(t, run("x", "s9"))
				f.mustSync(t, "r2", c2, first("r2", 1))
				f.mustSync(t, "r2", c2, req("r2", 0, v1.HeldRun{RunID: "x", State: v1.RunRunning}))
				f.enqueue(t, later("x2", "s9"))
			},
			held: v1.RunRunning, want: normal,
		},
		{
			name: "a session whose close is asked for",
			queue: func(t *testing.T, f *fixture) {
				f.enqueue(t, later("a2", "s1"))
				tok := f.admin(t, "cli")
				if code, e := f.api(t, "POST", "/sessions/s1/close", tok, nil, nil); code != http.StatusOK {
					t.Fatalf("close s1: %d %+v", code, e)
				}
			},
			held: v1.RunRunning, want: normal,
		},
		{
			name: "a start moment still ahead, for a runner that holds none",
			queue: func(t *testing.T, f *fixture) {
				b := run("b", "s2")
				at := f.clock.Now().Add(time.Hour)
				b.StartAt = &at
				f.enqueue(t, b)
			},
			held: v1.RunRunning,
			sync: func(r v1.SyncRequest) v1.SyncRequest {
				d := doc("r1")
				d.ProtocolFeatures = nil
				r.Capabilities, r.Fingerprint = &d, "fp-r1-bare"
				return r
			},
			want: normal,
		},
		{
			name: "a session opening on a path, for a runner whose owner switched those off",
			queue: func(t *testing.T, f *fixture) {
				b := run("b", "s2")
				b.Sources = []v1.Source{{Path: "/srv/repo"}}
				f.enqueue(t, b)
			},
			held: v1.RunRunning,
			sync: func(r v1.SyncRequest) v1.SyncRequest {
				d := doc("r1")
				d.PathSources = new(false)
				r.Capabilities, r.Fingerprint = &d, "fp-r1-no-paths"
				return r
			},
			want: normal,
		},
		{
			name:  "a draining runner",
			queue: func(t *testing.T, f *fixture) { f.enqueue(t, run("b", "s2")) },
			held:  v1.RunRunning,
			sync: func(r v1.SyncRequest) v1.SyncRequest {
				r.Health.Draining = true
				return r
			},
			want: normal,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			cred := f.register(t, "r1")
			f.enqueue(t, run("a", "s1"))
			f.mustSync(t, "r1", cred, first("r1", 1))
			f.mustSync(t, "r1", cred, req("r1", 0, claimed("a")...))
			if tc.queue != nil {
				tc.queue(t, f)
			}
			r := req("r1", tc.free, v1.HeldRun{RunID: "a", State: tc.held})
			if tc.sync != nil {
				r = tc.sync(r)
			}
			res := f.mustSync(t, "r1", cred, r)
			if len(res.Runs) != 0 {
				t.Fatalf("offered %v; the case is about a run not offered", ids(res.Runs))
			}
			if res.NextSyncMS != tc.want {
				t.Errorf("next_sync_ms = %d, want %d", res.NextSyncMS, tc.want)
			}
			if res.LeaseMS < res.NextSyncMS {
				t.Errorf("lease_ms %d is shorter than next_sync_ms %d", res.LeaseMS, res.NextSyncMS)
			}
		})
	}
}

// A turn queued behind its session's waiting run waits for an account's
// reset, not for anything this runner is executing: a run going on in another
// session does not make it any sooner, and must not ask the runner back in
// 3 s for as long as that run lasts.
func TestNextSyncIsNormalForATurnBehindAWaitingRun(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.enqueue(t, run("a", "s1"), run("c", "s2"))
	f.mustSync(t, "r1", cred, first("r1", 2))
	f.mustSync(t, "r1", cred, req("r1", 0, claimed("a", "c")...))
	next := run("a2", "s1")
	next.Session.New = false
	f.enqueue(t, next)
	res := f.mustSync(t, "r1", cred, req("r1", 1,
		v1.HeldRun{RunID: "a", State: v1.RunWaiting}, v1.HeldRun{RunID: "c", State: v1.RunRunning}))
	if len(res.Runs) != 0 || res.NextSyncMS != 15000 {
		t.Errorf("offered %v, next_sync_ms %d; want nothing and 15000", ids(res.Runs), res.NextSyncMS)
	}
}

// With a harness's own cap full, only a run of that harness ending lets the
// queued run go: a claude run ending frees nothing for a codex run held back
// by codex's cap. With only the total full, any run ending frees it.
func TestNextSyncCountsOnlyARunWhoseEndFreesTheQueuedOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		free v1.Capacity
		want int
	}{
		{"its harness's cap full, held by a waiting run", v1.Capacity{Total: 1, ByHarness: map[string]int{"codex": 0}}, 15000},
		{"only the total full", v1.Capacity{Total: 0}, 3000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			cred := f.register(t, "r1")
			d := doc("r1")
			d.Harnesses[1].Kind = "first-class"
			d.Capacity = v1.Capacity{Total: 3, ByHarness: map[string]int{"codex": 1}}
			r := first("r1", 2)
			r.Capabilities = &d
			x := run("x", "sx")
			x.Harness = "codex"
			f.enqueue(t, run("a", "s1"), x)
			f.mustSync(t, "r1", cred, r)
			f.mustSync(t, "r1", cred, req("r1", 0, claimed("a", "x")...))
			y := run("y", "sy")
			y.Harness = "codex"
			f.enqueue(t, y)
			s := req("r1", 0, v1.HeldRun{RunID: "a", State: v1.RunRunning}, v1.HeldRun{RunID: "x", State: v1.RunWaiting})
			s.Health.FreeCapacity = tc.free
			res := f.mustSync(t, "r1", cred, s)
			if len(res.Runs) != 0 || res.NextSyncMS != tc.want {
				t.Errorf("offered %v, next_sync_ms %d; want nothing and %d", ids(res.Runs), res.NextSyncMS, tc.want)
			}
		})
	}
}

// A runner holding nothing gets the normal interval whatever is queued: its
// capacity is taken by nothing this hub can see end — another hub's runs, or
// a pool of none — so a sooner sync would find it no freer.
func TestNextSyncIsNormalForARunnerHoldingNothing(t *testing.T) {
	f := newFixture(t)
	cred := f.register(t, "r1")
	f.enqueue(t, run("a", "s1"))
	res := f.mustSync(t, "r1", cred, first("r1", 0))
	if len(res.Runs) != 0 || res.NextSyncMS != 15000 {
		t.Errorf("offered %v, next_sync_ms %d; want nothing and 15000", ids(res.Runs), res.NextSyncMS)
	}
}

// Sooner is never slower: a hub configured below 3 s — only a test can make
// one — keeps its own interval rather than being raised to the quick one.
func TestQuickSyncNeverExceedsTheInterval(t *testing.T) {
	for _, tc := range []struct {
		interval, want time.Duration
	}{
		{15 * time.Second, 3 * time.Second},
		{5 * time.Second, 3 * time.Second},
		{time.Second, time.Second},
	} {
		h := &Hub{interval: tc.interval}
		if got := h.quickInterval(); got != tc.want {
			t.Errorf("interval %s: quick %s, want %s", tc.interval, got, tc.want)
		}
	}
}
