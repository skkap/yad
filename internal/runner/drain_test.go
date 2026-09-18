package runner

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"syscall"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	hubdb "github.com/skkap/yad/internal/hub/store/db"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store/db"
)

// eventually polls cond until it holds, failing after a deadline rather than
// hanging.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting until %s", what)
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// A draining runner claims nothing: runs it was offered and never listed go
// back to the hub's queue, its health says draining with no free capacity,
// and the hub offers it nothing more. It keeps syncing all the same.
func TestDrainingClaimsNothing(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 2)
	l.Drain = NewDrain()
	e.enqueue(t, testRun("a", "s1"), testRun("b", "s2"))
	if res := mustSync(t, l); len(res.Runs) != 2 {
		t.Fatalf("offered %d", len(res.Runs))
	}
	if closed(l.Quiesced()) {
		t.Fatal("quiesced before draining")
	}

	l.Drain.Begin("test")
	res := mustSync(t, l)
	if len(res.Runs) != 0 {
		t.Errorf("a draining runner was offered %d runs", len(res.Runs))
	}
	for _, id := range []string{"a", "b"} {
		if got := e.hubState(t, id); got != "queued" {
			t.Errorf("hub state of %s is %s; a claim never listed goes back in the queue", id, got)
		}
		if _, err := e.store.GetRun(context.Background(), dbRun("hub", id)); err == nil {
			t.Errorf("run %s is still recorded here", id)
		}
	}
	if got := e.exec.ids(); len(got) != 0 {
		t.Errorf("started %v", got)
	}
	if l.Pool.Free() != 2 {
		t.Errorf("free %d: withdrawn claims keep no capacity", l.Pool.Free())
	}
	if !closed(l.Quiesced()) {
		t.Error("not quiesced after a sync while draining")
	}
	r, err := e.hubStore.GetRunner(context.Background(), l.RunnerID)
	if err != nil {
		t.Fatal(err)
	}
	h, err := runnerHealth(r)
	if err != nil || !h.Draining || h.FreeCapacity.Total != 0 {
		t.Errorf("health on the hub %+v, %v: want draining, no free capacity", h, err)
	}
}

// yad hub drain, through the service API: the runner hears it at its next
// sync and starts draining, is offered nothing from then on, and its next
// sync — saying draining — is the answer that ends the request, so the
// runner's next process is not drained as well.
func TestHubDrainControl(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	l := e.loop(t, 1)
	l.Drain = NewDrain()
	api := e.api(t)
	mustSync(t, l)

	view, err := api.Drain(ctx, l.RunnerID)
	if err != nil || view.DrainRequestedAt == nil || view.Draining {
		t.Fatalf("drain: %+v %v", view, err)
	}
	e.enqueue(t, testRun("a", "s1"))
	res := mustSync(t, l)
	if !hasControl(res, v1.ControlDrain) || !l.Drain.IsDraining() {
		t.Fatalf("controls %+v, draining %v", res.Controls, l.Drain.IsDraining())
	}
	if len(res.Runs) != 0 {
		t.Errorf("offered %d runs to a runner asked to drain", len(res.Runs))
	}

	res = mustSync(t, l)
	if hasControl(res, v1.ControlDrain) || len(res.Runs) != 0 {
		t.Errorf("after the runner said draining: %+v", res)
	}
	r, err := e.hubStore.GetRunner(ctx, l.RunnerID)
	if err != nil || r.DrainRequestedAt.Valid {
		t.Errorf("the request stands after the runner answered it: %+v %v", r.DrainRequestedAt, err)
	}
	if view, err := api.Drain(ctx, l.RunnerID); err != nil || !view.Draining || view.DrainRequestedAt != nil {
		t.Errorf("drain again: %+v %v — a draining runner is answered as it is", view, err)
	}
	if got := e.hubState(t, "a"); got != "queued" {
		t.Errorf("hub state %s", got)
	}

	// The runner's next process syncs as any other, and is offered work.
	l2 := &Loop{Connection: l.Connection, RunnerID: l.RunnerID, Hub: l.Hub, Store: e.store, Pool: NewPool(v1.Capacity{Total: 1}),
		Capabilities: l.Capabilities, Executor: e.exec, Clock: e.clock, Rand: l.Rand, Drain: NewDrain()}
	if res := mustSync(t, l2); hasControl(res, v1.ControlDrain) || len(res.Runs) != 1 {
		t.Errorf("the next process: %+v", res)
	}
}

func hasControl(res v1.SyncResponse, kind v1.ControlKind) bool {
	for _, c := range res.Controls {
		if c.Kind == kind {
			return true
		}
	}
	return false
}

// The way down, as Serve runs it, against yad hub in process: a drain lets
// the run held finish and exits once its result is delivered; a drain whose
// wait runs out, or a second ask, cancels it down the cancel ladder and says
// the runner did; the end of the context exits at once and leaves the run
// held for the next start to report lost.
func TestWayDown(t *testing.T) {
	finishes := fake.Script{Events: manyEvents(5), Delay: 40 * time.Millisecond, Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "done"}}
	hangs := fake.Script{Hang: true}
	for _, tc := range []struct {
		name   string
		script fake.Script
		wait   time.Duration
		// down takes the runner down once its run is running.
		down  func(d *Drain, exit context.CancelFunc)
		state v1.RunState // the run's end; "" is still held
		class string
	}{
		{"the run finishes", finishes, time.Minute, func(d *Drain, _ context.CancelFunc) { d.Begin("test") }, v1.RunSucceeded, ""},
		{"the drain wait runs out", hangs, 50 * time.Millisecond, func(d *Drain, _ context.CancelFunc) { d.Begin("test") }, v1.RunCancelled, ClassRunnerStopping},
		{"asked twice", hangs, time.Hour, func(d *Drain, _ context.CancelFunc) { d.Begin("test"); d.Cancel("test again") }, v1.RunCancelled, ClassRunnerStopping},
		{"exit now", hangs, time.Hour, func(d *Drain, exit context.CancelFunc) { d.Begin("test"); exit() }, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 2)
			l.Clock = realClock{}
			x := e.executor(fakeHarness(tc.script))
			d := NewDrain()
			sv := e.server(l, x, d, tc.wait)
			e.enqueue(t, testRun("a", "s1"))

			ctx, exit := context.WithCancel(context.Background())
			defer exit()
			done := make(chan error, 1)
			go func() { done <- sv.run(ctx) }()
			eventually(t, "the run is running", func() bool {
				r, err := e.store.GetRun(context.Background(), dbRun("hub", "a"))
				return err == nil && r.State == string(v1.RunRunning)
			})
			tc.down(d, exit)
			// Queued after the drain began: never offered here.
			e.enqueue(t, testRun("b", "s2"))

			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the runner did not go down")
			}
			if got := e.hubState(t, "b"); got != "queued" {
				t.Errorf("a run queued during the drain is %s on the hub", got)
			}
			local := localRun(t, e, "a")
			if tc.state == "" {
				if v1.RunState(local.State).IsTerminal() {
					t.Errorf("exit now left the run %s; it must stay held for the next start", local.State)
				}
				if _, ok := outboxResult(t, e, "a"); ok {
					t.Error("exit now owes a result")
				}
				return
			}
			// Delivered before the runner went: the hub has the result.
			if local.State != string(tc.state) || e.hubState(t, "a") != string(tc.state) {
				t.Fatalf("run ended %s here and %s on the hub, want %s", local.State, e.hubState(t, "a"), tc.state)
			}
			res := hubResult(t, e, "a")
			switch {
			case tc.class == "" && res.Error != nil:
				t.Errorf("result error %+v", res.Error)
			case tc.class != "" && (res.Error == nil || res.Error.Class != tc.class || res.Metrics.CancelLatencyMS == nil):
				t.Errorf("result %+v (error %+v), want class %s and a cancel latency", res, res.Error, tc.class)
			}
			if n, err := e.store.OutboxDepth(context.Background()); err != nil || n != 0 {
				t.Errorf("outbox %d %v after the drain", n, err)
			}
		})
	}
}

// A run cancelled on the way down before its harness is up — waiting on its
// start time — ends cancelled with nothing spawned, and says the runner did it.
func TestWayDownBeforeTheHarnessStarts(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	l.Clock = realClock{}
	ad := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}})
	x := e.executor(ad)
	d := NewDrain()
	sv := e.server(l, x, d, time.Hour)
	run := testRun("a", "s1")
	at := time.Now().Add(time.Hour)
	run.StartAt = &at
	e.enqueue(t, run)

	done := make(chan error, 1)
	go func() { done <- sv.run(context.Background()) }()
	eventually(t, "the run is claimed", func() bool {
		_, err := e.store.GetRun(context.Background(), dbRun("hub", "a"))
		return err == nil && len(x.activeIDs()) == 1
	})
	d.Cancel("test")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	res := hubResult(t, e, "a")
	if res.State != v1.RunCancelled || res.Error == nil || res.Error.Class != ClassRunnerStopping || len(ad.Starts) != 0 {
		t.Errorf("result %+v (error %+v), %d starts", res, res.Error, len(ad.Starts))
	}
}

// Every connection stopped on its own — its credential refused — and a drain
// begun after that has nothing to wait for.
func TestWayDownWithNoConnectionLeft(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	l.Clock = realClock{}
	sv := e.server(l, e.executor(), NewDrain(), time.Hour)
	l.Hub = refusingHub{}
	done := make(chan error, 1)
	go func() { done <- sv.run(context.Background()) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a refused credential was not reported")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve outlived its only connection")
	}
}

type refusingHub struct{}

func (refusingHub) Sync(context.Context, string, v1.SyncRequest) (v1.SyncResponse, error) {
	return v1.SyncResponse{}, errUnauthorized
}

func (refusingHub) Result(context.Context, string, v1.Result) error { return errUnauthorized }

// Signals are counted: the first drains, the second cancels, the third exits.
func TestOnSignals(t *testing.T) {
	d := NewDrain()
	sigs := make(chan os.Signal)
	exited := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		OnSignals(context.Background(), sigs, d, func() { close(exited) }, slog.New(slog.DiscardHandler))
		close(returned)
	}()
	sigs <- syscall.SIGTERM
	eventually(t, "the first signal drains", func() bool { return d.IsDraining() })
	if closed(d.Cancelling()) {
		t.Fatal("one signal cancelled")
	}
	sigs <- os.Interrupt
	eventually(t, "the second signal cancels", func() bool { return closed(d.Cancelling()) })
	if closed(exited) {
		t.Fatal("two signals exited")
	}
	sigs <- syscall.SIGTERM
	<-exited
	<-returned

	// A runner the hub already drains still only drains on the first signal.
	d = NewDrain()
	d.Begin("the hub")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go OnSignals(ctx, sigs, d, func() { t.Error("exited") }, slog.New(slog.DiscardHandler))
	sigs <- syscall.SIGTERM
	time.Sleep(20 * time.Millisecond)
	if closed(d.Cancelling()) {
		t.Error("a first signal to a runner the hub drains cancelled its runs")
	}
}

// server is Serve's supervisor over the env's hub, with one connection.
func (e *env) server(l *Loop, x *Exec, d *Drain, wait time.Duration) *server {
	rep := e.reporter(l)
	l.Drain, l.Executor, l.ClaimAfter = d, x, rep.Replayed()
	x.Report = func(string) { rep.Wake() }
	return &server{drain: d, wait: wait, store: e.store, exec: x, loops: []*Loop{l},
		reporters: map[string]*Reporter{l.Connection: rep}, log: slog.New(slog.DiscardHandler)}
}

// errUnauthorized is the hub refusing the runner's credential: no retry
// helps, and the connection stops.
var errUnauthorized = &hubclient.StatusError{Status: 401, Protocol: &v1.Error{Code: v1.CodeUnauthorized, Message: "no", NextAction: "yad connect"}}

func runnerHealth(r hubdb.Runner) (v1.Health, error) {
	var h v1.Health
	err := json.Unmarshal([]byte(r.Health.String), &h)
	return h, err
}

func dbRun(conn, id string) db.GetRunParams { return db.GetRunParams{Connection: conn, ID: id} }

// activeIDs is the runs x has in hand.
func (x *Exec) activeIDs() []string {
	x.init()
	x.mu.Lock()
	defer x.mu.Unlock()
	var out []string
	for k := range x.active {
		out = append(out, k.run)
	}
	return out
}
