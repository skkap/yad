package runner

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store/db"
)

// heldClock lets a wait of zero through at once and holds every other until
// the test fires it. A loop that syncs only when its interval runs out never
// syncs again on this clock, so every sync after the first wait is one
// something brought forward.
type heldClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
	held  []chan time.Time
	// asked hears every wait that is held, in order.
	asked chan time.Duration
}

func newHeldClock() *heldClock {
	return &heldClock{now: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC), asked: make(chan time.Duration, 16)}
}

func (c *heldClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *heldClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	c.waits = append(c.waits, d)
	ch := make(chan time.Time, 1)
	if d == 0 {
		ch <- c.now
		c.mu.Unlock()
		return ch
	}
	c.held = append(c.held, ch)
	c.mu.Unlock()
	c.asked <- d
	return ch
}

// fire lets the last held wait run out.
func (c *heldClock) fire(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.held[len(c.held)-1] <- c.now
}

// advance moves time on without firing anything: the loop is still waiting.
func (c *heldClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *heldClock) recorded() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.waits)
}

// earlyRig is a runner as Serve wires it — a real executor on the fake
// harness, a reporter that wakes the loop when it has handled a result — against
// yad hub in process, on a heldClock. Each run's turn waits in the harness
// for its gate, so the test says when a run ends.
type earlyRig struct {
	e       *env
	l       *Loop
	x       *Exec
	clock   *heldClock
	gates   map[string]chan struct{}
	started chan string
	// settled hears every flush that handled a result, after the loop has
	// been woken for it.
	settled chan struct{}
	// reportTo is the hub the reporter delivers to; nil is the loop's.
	reportTo ReportHub
	// byHand is a reporter that flushes only when the test calls
	// reporter.Flush: no tick and no executor's wake, so the moment a result
	// reaches the hub is the test's.
	byHand   bool
	reporter *Reporter
	// holds keeps each run named in it from giving its capacity back until
	// the test closes its channel; ending hears the run then, its result
	// already written.
	holds  map[string]chan struct{}
	ending chan string
}

// holdingExec is the rig's executor with the end of a run's release in the
// test's hands: the executor writes the result, then gives the capacity back,
// and the test may act in between.
type holdingExec struct {
	*Exec
	g *earlyRig
}

func (h holdingExec) Start(ctx context.Context, c Claim) {
	if hold, ok := h.g.holds[c.Run.RunID]; ok {
		release, id := c.Release, c.Run.RunID
		c.Release = func() {
			h.g.ending <- id
			<-hold
			release()
		}
	}
	h.Exec.Start(ctx, c)
}

func newEarlyRig(t *testing.T, capacity int, runs ...v1.Run) *earlyRig {
	t.Helper()
	g := &earlyRig{e: newEnv(t), clock: newHeldClock(), gates: map[string]chan struct{}{},
		started: make(chan string, 16), settled: make(chan struct{}, 16), ending: make(chan string, 16)}
	for _, r := range runs {
		g.gates[r.RunID] = make(chan struct{})
	}
	g.l = g.e.loop(t, capacity)
	g.l.Clock = g.clock
	g.x = g.e.executor(&fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
		g.started <- s.RunID
		<-g.gates[s.RunID]
		return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}}
	}})
	g.l.Executor = holdingExec{Exec: g.x, g: g}
	g.e.enqueue(t, runs...)
	return g
}

// run runs the loop and its reporter until drive returns, then lets every
// run still in the harness go and waits for it.
func (g *earlyRig) run(t *testing.T, drive func(ctx context.Context, cancel context.CancelFunc)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	to := g.reportTo
	if to == nil {
		to = g.l.Hub.(*hubclient.Client)
	}
	r := NewReporter(g.l.Connection, to, g.e.store, nil)
	r.Handled = func() {
		g.l.Wake()
		g.settled <- struct{}{}
	}
	g.reporter = r
	var bg sync.WaitGroup
	if !g.byHand {
		g.x.Report = func(string) { r.Wake() }
		bg.Go(func() { r.Run(ctx) })
	}
	done := make(chan error, 1)
	go func() { done <- g.l.Run(ctx) }()
	drive(ctx, cancel)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Before any run still held ends and wakes it again: a wake left over
	// here is a second early sync the burst was meant to fold into one.
	if n := len(g.l.wakes()); n != 0 {
		t.Errorf("%d wake left pending once the loop stopped", n)
	}
	cancel()
	bg.Wait()
	for _, chs := range []map[string]chan struct{}{g.gates, g.holds} {
		for _, ch := range chs {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}
	g.x.Wait()
}

// heard waits until the loop has taken the wake waiting for it.
func (g *earlyRig) heard(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(g.l.wakes()) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the loop never took its wake")
		}
		time.Sleep(time.Millisecond)
	}
}

// ended waits until the run id has written its result and is about to give
// its capacity back.
func (g *earlyRig) ended(t *testing.T, id string) {
	t.Helper()
	select {
	case got := <-g.ending:
		if got != id {
			t.Fatalf("%s ended, want %s", got, id)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s never ended", id)
	}
}

// next is the next wait the loop holds, or a failure if it never asks: a loop
// that would sit out its interval asks for nothing more on this clock.
func (g *earlyRig) next(t *testing.T) time.Duration {
	t.Helper()
	select {
	case d := <-g.clock.asked:
		return d
	case <-time.After(10 * time.Second):
		t.Fatalf("the loop asked for no wait after %v; it is sitting out an interval", g.clock.recorded())
		return 0
	}
}

// delivered waits until the hub holds a result for every one of ids.
func (g *earlyRig) delivered(t *testing.T, ids ...string) {
	t.Helper()
	for {
		missing := false
		for _, id := range ids {
			if _, err := g.e.hubStore.GetResult(context.Background(), id); err != nil {
				missing = true
			}
		}
		if !missing {
			return
		}
		select {
		case <-g.settled:
		case <-time.After(10 * time.Second):
			t.Fatalf("results for %v never reached the hub", ids)
		}
	}
}

// Against yad hub: once the run a runner holds ends, the runner syncs as soon
// as the hub has the result and the run's capacity is back, and the next turn
// of the session starts — not after the 3 s the hub asked for, which on this
// clock never passes. The early sync keeps earlySyncGap from the last one, and
// no more once that much has gone by. The run then going on, the runner waits
// the hub's interval again.
//
// The result reaches the hub before the capacity comes back, the order a
// loaded machine can put them in (DEV-156): the executor writes the one, then
// gives the other, and the reporter can deliver in between. A sync then would
// offer no capacity, and the hub would hand a2 to nobody.
func TestARunEndingBringsTheNextSyncForward(t *testing.T) {
	later := testRun("a2", "s1")
	later.Session.New = false
	for _, tc := range []struct {
		name string
		// ran is how long a ran for, from the sync that started it.
		ran  time.Duration
		want []time.Duration
	}{
		{"ending at once", 0, []time.Duration{0, 3 * time.Second, earlySyncGap, 0, 15 * time.Second}},
		{"ending 2 s in", 2 * time.Second, []time.Duration{0, 3 * time.Second, 0, 15 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newEarlyRig(t, 1, testRun("a", "s1"), later)
			g.byHand = true
			g.holds = map[string]chan struct{}{"a": make(chan struct{})}
			g.run(t, func(ctx context.Context, cancel context.CancelFunc) {
				if d := g.next(t); d != 3*time.Second {
					t.Fatalf("waited %s with a2 queued behind a, want 3s", d)
				}
				g.clock.advance(tc.ran)
				close(g.gates["a"])
				g.ended(t, "a")
				g.reporter.Flush(ctx)
				g.delivered(t, "a")
				g.heard(t)
				// Absence has no event to wait for; this bounds how long
				// it is looked for. A loop that syncs on the delivery
				// asks its next wait within microseconds of taking it.
				select {
				case d := <-g.clock.asked:
					t.Fatalf("waited %s before a's capacity came back: the sync it follows offered none", d)
				case <-time.After(200 * time.Millisecond):
				}
				close(g.holds["a"])
				d := g.next(t)
				if d == earlySyncGap {
					g.clock.fire(d)
					d = g.next(t)
				}
				if d != 15*time.Second {
					t.Errorf("waited %s once a2 started, want the hub's 15s", d)
				}
				cancel()
			})
			if got := g.clock.recorded(); !slices.Equal(got, tc.want) {
				t.Errorf("waits %v, want %v", got, tc.want)
			}
			if got := g.e.hubState(t, "a2"); got != "running" && got != "claimed" {
				t.Errorf("a2 is %s at the hub, want it started", got)
			}
		})
	}
}

// Three runs ending together bring one sync forward, not three: the wakes
// they leave fold into it, and the run it takes then waits out the hub's
// interval.
func TestRunsEndingTogetherBringOneSyncForward(t *testing.T) {
	ids := []string{"a", "b", "c"}
	g := newEarlyRig(t, 3, testRun("a", "s1"), testRun("b", "s2"), testRun("c", "s3"), testRun("d", "s4"))
	g.run(t, func(_ context.Context, cancel context.CancelFunc) {
		if d := g.next(t); d != 3*time.Second {
			t.Fatalf("waited %s with d queued behind a, b and c, want 3s", d)
		}
		for range ids {
			<-g.started
		}
		for _, id := range ids {
			close(g.gates[id])
		}
		if d := g.next(t); d != earlySyncGap {
			t.Fatalf("waited %s once the runs ended, want %s", d, earlySyncGap)
		}
		// Every run has ended and every result is in before the early sync
		// goes: what each of them woke is already waiting.
		g.delivered(t, ids...)
		g.clock.fire(earlySyncGap)
		if d := g.next(t); d != 15*time.Second {
			t.Errorf("waited %s once d started, want the hub's 15s", d)
		}
		cancel()
	})
	want := []time.Duration{0, 3 * time.Second, earlySyncGap, 0, 15 * time.Second}
	if got := g.clock.recorded(); !slices.Equal(got, want) {
		t.Errorf("waits %v, want %v", got, want)
	}
	if got := g.e.hubState(t, "d"); got != "running" && got != "claimed" {
		t.Errorf("d is %s at the hub, want it started", got)
	}
}

// syncSpy tells the test of every sync the loop sends.
type syncSpy struct {
	Hub
	synced chan struct{}
}

func (s syncSpy) Sync(ctx context.Context, runnerID string, req v1.SyncRequest) (v1.SyncResponse, error) {
	s.synced <- struct{}{}
	return s.Hub.Sync(ctx, runnerID, req)
}

// A run's capacity comes back before its result reaches the hub, and a sync
// in between would list the run as still running: its session's next turn
// would not be offered, and the hub would answer 3 s again. So a run ending
// brings no sync forward while its result is waiting to go; the result going
// does.
func TestAnEarlySyncWaitsForTheResult(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	clock := newHeldClock()
	spy := syncSpy{Hub: l.Hub, synced: make(chan struct{}, 16)}
	l.Clock, l.Hub = clock, spy
	later := testRun("a2", "s1")
	later.Session.New = false
	e.enqueue(t, testRun("a", "s1"), later)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.Run(ctx) }()
	g := &earlyRig{e: e, l: l, clock: clock}
	if d := g.next(t); d != 3*time.Second {
		t.Fatalf("waited %s with a2 queued behind a, want 3s", d)
	}
	for range 2 {
		<-spy.synced
	}
	clock.advance(2 * time.Second)

	// What the executor does as a run ends: its terminal state and its
	// result in one write, then its capacity back.
	if err := e.store.Tx(ctx, func(q *db.Queries) error {
		if err := q.SetRunState(ctx, db.SetRunStateParams{State: string(v1.RunSucceeded), UpdatedAt: time.Now().UnixMilli(), Connection: "hub", ID: "a"}); err != nil {
			return err
		}
		return q.PutOutbox(ctx, db.PutOutboxParams{Connection: "hub", RunID: "a", Body: `{"state":"succeeded"}`, NextAttemptAt: time.Now().UnixMilli()})
	}); err != nil {
		t.Fatal(err)
	}
	e.exec.started[0].Release()
	// Absence has no event to wait for; this bounds how long it is looked
	// for. A loop that syncs on the release does so within microseconds.
	select {
	case <-spy.synced:
		t.Fatal("synced while a's result had not reached the hub")
	case <-time.After(200 * time.Millisecond):
	}

	r := NewReporter("hub", spy.Hub.(*hubclient.Client), e.store, nil)
	r.Handled = l.Wake
	r.Flush(ctx)
	if d := g.next(t); d != 15*time.Second {
		t.Errorf("waited %s once a2 started, want the hub's 15s", d)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if want := []time.Duration{0, 3 * time.Second, 0, 15 * time.Second}; !slices.Equal(clock.recorded(), want) {
		t.Errorf("waits %v, want %v", clock.recorded(), want)
	}
	if got := e.exec.ids(); !slices.Equal(got, []string{"a", "a2"}) {
		t.Errorf("started %v, want [a a2]", got)
	}
}

// A result the hub fails to take waits for its retry, and a result waiting
// for a retry holds no early sync: the delivery that failed brings it. The
// hub, still holding a as running, keeps the session's next turn and asks
// the runner back in 3 s.
func TestAResultPutOffToARetryStillBringsTheSyncForward(t *testing.T) {
	later := testRun("a2", "s1")
	later.Session.New = false
	g := newEarlyRig(t, 1, testRun("a", "s1"), later)
	failing := &tap{next: g.l.Hub.(*hubclient.Client), resultErr: status(503)}
	g.reportTo = failing
	g.run(t, func(_ context.Context, cancel context.CancelFunc) {
		if d := g.next(t); d != 3*time.Second {
			t.Fatalf("waited %s with a2 queued behind a, want 3s", d)
		}
		g.clock.advance(2 * time.Second)
		close(g.gates["a"])
		if d := g.next(t); d != 3*time.Second {
			t.Errorf("waited %s after the early sync, want the hub's 3s", d)
		}
		cancel()
	})
	if want := []time.Duration{0, 3 * time.Second, 3 * time.Second}; !slices.Equal(g.clock.recorded(), want) {
		t.Errorf("waits %v, want %v: the failed delivery brought no sync forward", g.clock.recorded(), want)
	}
	failing.mu.Lock()
	defer failing.mu.Unlock()
	if failing.results == 0 {
		t.Error("the result was never tried")
	}
}

// syncHook runs during as each sync reaches the hub: after the sync has read
// the free capacity it offers.
type syncHook struct {
	Hub
	during func()
}

func (s syncHook) Sync(ctx context.Context, runnerID string, req v1.SyncRequest) (v1.SyncResponse, error) {
	s.during()
	return s.Hub.Sync(ctx, runnerID, req)
}

// A run's capacity can come back in the middle of an ordinary sync, once
// that sync has emptied the loop's wake: its result already with the hub, the
// release is all that is left of its end. Back before the sync reads the free
// capacity, the sync offers it, and the release's wake has nothing new to
// bring forward (DEV-157). Back after, the sync offered none, and the wake
// brings the next one forward as ever.
func TestAReleaseDuringAnIntervalSync(t *testing.T) {
	for _, tc := range []struct {
		name string
		// atHub gives the capacity back as the sync reaches the hub, not
		// before it reads the free capacity.
		atHub bool
		want  []time.Duration
	}{
		{"before the sync reads the free capacity", false, []time.Duration{0, 15 * time.Second, 15 * time.Second}},
		{"after the sync has read it", true, []time.Duration{0, 15 * time.Second, 15 * time.Second, earlySyncGap}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newEarlyRig(t, 1, testRun("a", "s1"))
			g.byHand = true
			g.holds = map[string]chan struct{}{"a": make(chan struct{})}
			// inside, when it holds one, is run by the next sync, once.
			inside := make(chan func(), 1)
			hook := func() {
				select {
				case f := <-inside:
					f()
				default:
				}
			}
			if tc.atHub {
				g.reportTo = g.l.Hub.(*hubclient.Client)
				g.l.Hub = syncHook{Hub: g.l.Hub, during: hook}
			} else {
				// The capability document is read as the sync begins,
				// before anything it tells the hub.
				caps := g.l.Capabilities
				g.l.Capabilities = func() v1.Capabilities {
					hook()
					return caps()
				}
			}
			g.run(t, func(ctx context.Context, cancel context.CancelFunc) {
				if d := g.next(t); d != 15*time.Second {
					t.Fatalf("waited %s with a running and nothing queued, want the hub's 15s", d)
				}
				close(g.gates["a"])
				g.ended(t, "a")
				g.reporter.Flush(ctx)
				g.delivered(t, "a")
				// The delivery's wake, let go: a's capacity is still out.
				g.heard(t)
				inside <- func() {
					close(g.holds["a"])
					// The release's wake is the last thing it does.
					deadline := time.Now().Add(10 * time.Second)
					for len(g.l.wakes()) == 0 {
						if time.Now().After(deadline) {
							t.Error("a never gave its capacity back")
							return
						}
						time.Sleep(time.Millisecond)
					}
				}
				g.clock.fire(15 * time.Second)
				if d := g.next(t); d != 15*time.Second {
					t.Fatalf("waited %s after the interval sync, want the hub's 15s", d)
				}
				// Once the loop has taken the release's wake, whatever
				// it asks of the clock for it is asked before it looks
				// again, and so before it sees the cancel.
				g.heard(t)
				cancel()
			})
			if got := g.clock.recorded(); !slices.Equal(got, tc.want) {
				t.Errorf("waits %v, want %v", got, tc.want)
			}
		})
	}
}
