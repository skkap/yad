package runner

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/hub"
	"github.com/skkap/yad/internal/hubapiclient"
	"github.com/skkap/yad/internal/store/db"
)

// api is a service API client for the env's hub, as `yad hub cancel` uses.
func (e *env) api(t *testing.T) *hubapiclient.Client {
	t.Helper()
	tok, err := hub.IssueAdminToken(context.Background(), e.hubStore, "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, err := hubapiclient.New(strings.TrimSuffix(e.url, hub.BasePath), tok)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// started claims the env's queued run and hands it to x, without waiting for
// it to end — or to begin: the run's goroutine may still be preparing, and a
// control that lands then stops a run with no harness in it.
func started(t *testing.T, l *Loop, x *Exec) {
	t.Helper()
	l.Executor = x
	mustSync(t, l)
	mustSync(t, l)
}

// running is started, and then waits until run id's harness is up. A control
// meant for the turn must not be sent before this: the runner marks a run
// running only once its interrupt can reach the turn, and one that arrives
// earlier ends the run before the harness is spawned (DEV-92).
func running(t *testing.T, e *env, l *Loop, x *Exec, id string) {
	t.Helper()
	started(t, l, x)
	eventually(t, "run "+id+" is running", func() bool {
		return localRun(t, e, id).State == string(v1.RunRunning)
	})
}

// ended waits for every run on x, failing rather than hanging.
func ended(t *testing.T, x *Exec) {
	t.Helper()
	done := make(chan struct{})
	go func() { x.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not end")
	}
}

func runEvents(t *testing.T, e *env, runID string) []v1.Event {
	t.Helper()
	rows, err := e.store.UnackedEvents(context.Background(), db.UnackedEventsParams{Connection: "hub", RunID: runID, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var out []v1.Event
	for _, r := range rows {
		var ev v1.Event
		if err := json.Unmarshal([]byte(r.Body), &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func statuses(evs []v1.Event) []string {
	var out []string
	for _, ev := range evs {
		if ev.Kind == v1.EventStatus {
			out = append(out, ev.Status)
		}
	}
	return out
}

// `yad hub cancel` on a running run, through the hub and the runner's next
// sync, down the ladder as far as the harness makes it go: interrupt, then
// SIGTERM Grace later, then SIGKILL TermGrace after that. Whichever rung ends
// it, the run is cancelled and the latency is measured from the cancel's
// arrival.
func TestCancelClimbsTheLadder(t *testing.T) {
	for _, tc := range []struct {
		name       string
		script     fake.Script
		terminated bool
		atLeast    time.Duration
	}{
		{"the harness stops on the interrupt", fake.Script{Hang: true}, false, 0},
		{"the harness ignores the interrupt", fake.Script{Hang: true, IgnoreInterrupt: true}, true, 20 * time.Millisecond},
		{"the harness ignores SIGTERM too", fake.Script{Hang: true, IgnoreInterrupt: true, IgnoreTerm: true}, true, 40 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			e.enqueue(t, testRun("a", "s1"))
			ad := fakeHarness(tc.script)
			x := e.executor(ad)
			running(t, e, l, x, "a")
			view, err := e.api(t).Cancel(context.Background(), "a")
			if err != nil || view.CancelRequestedAt == nil {
				t.Fatalf("cancel: %+v %v", view, err)
			}
			mustSync(t, l)
			ended(t, x)

			res, ok := outboxResult(t, e, "a")
			if !ok || res.State != v1.RunCancelled || res.Error != nil {
				t.Fatalf("result %+v (error %+v), %v", res, res.Error, ok)
			}
			if lat := res.Metrics.CancelLatencyMS; lat == nil || time.Duration(*lat)*time.Millisecond < tc.atLeast {
				t.Errorf("cancel_latency_ms %v, want at least %s", lat, tc.atLeast)
			}
			turns := ad.Turns()
			if len(turns) != 1 {
				t.Fatalf("%d turns", len(turns))
			}
			if n, term := fake.Rungs(turns[0]); n != 1 || term != tc.terminated {
				t.Errorf("interrupts %d, SIGTERM %v; want 1, %v", n, term, tc.terminated)
			}
			if !slices.Contains(statuses(runEvents(t, e, "a")), "cancelling") {
				t.Errorf("no cancelling status in %v", statuses(runEvents(t, e, "a")))
			}
			// The hub hears it: result, latency and all.
			e.reporter(l).Flush(context.Background())
			if got := e.hubState(t, "a"); got != "cancelled" {
				t.Errorf("hub state %s", got)
			}
			if got := hubResult(t, e, "a"); got.Metrics.CancelLatencyMS == nil {
				t.Errorf("hub result has no cancel latency: %+v", got.Metrics)
			}
		})
	}
}

// A run stopped before its harness is up — waiting on its start time here —
// is cancelled with nothing spawned. An interrupt does the same: there is no
// turn to end, and the session stays as it was.
func TestStopBeforeTheHarnessStarts(t *testing.T) {
	for _, verb := range []string{"cancel", "interrupt"} {
		t.Run(verb, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			run := testRun("a", "s1")
			at := time.Now().Add(time.Hour)
			run.StartAt = &at
			e.enqueue(t, run)
			ad := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}})
			x := e.executor(ad)
			started(t, l, x)
			api := e.api(t)
			var err error
			if verb == "cancel" {
				_, err = api.Cancel(context.Background(), "a")
			} else {
				_, err = api.Interrupt(context.Background(), "a")
			}
			if err != nil {
				t.Fatal(err)
			}
			mustSync(t, l)
			ended(t, x)
			res, ok := outboxResult(t, e, "a")
			if !ok || res.State != v1.RunCancelled || res.Metrics.CancelLatencyMS == nil {
				t.Fatalf("result %+v, %v", res, ok)
			}
			if len(ad.Starts) != 0 {
				t.Errorf("the harness was started: %+v", ad.Starts)
			}
			if l.Pool.Free() != 1 {
				t.Errorf("capacity not released: free %d", l.Pool.Free())
			}
		})
	}
}

// alsoSays is a hub that adds controls to the answer of the sync it is armed
// for. The window it reproduces is one answer wide — yad hub queues an
// interrupt only for a run it holds as claimed, and the answer that claims it
// is the first that can carry one — so a test cannot reach it through the
// service API alone.
type alsoSays struct {
	Hub
	mu    sync.Mutex
	extra []v1.Control
}

func (h *alsoSays) Sync(ctx context.Context, runnerID string, req v1.SyncRequest) (v1.SyncResponse, error) {
	out, err := h.Hub.Sync(ctx, runnerID, req)
	h.mu.Lock()
	defer h.mu.Unlock()
	if err == nil {
		out.Controls = append(out.Controls, h.extra...)
		h.extra = nil
	}
	return out, err
}

func (h *alsoSays) arm(cs ...v1.Control) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.extra = cs
}

// DEV-113: a control in the very answer that acknowledges a claim reaches
// the run. The run is not the executor's until that answer has been read, so
// a control handed to the executor first found nothing and was dropped, and
// the run started as if the hub had said nothing — a fast one finishing
// before the hub could repeat itself.
//
// A cancel withdraws the claim: the run never starts and no result is owed
// (HUB.md §7, decision 0019). An interrupt ends it as a cancel ends a run
// whose harness is not up: cancelled, nothing spawned, with a result, because
// a hub interrupts only a run it holds as claimed.
func TestStopInTheAnswerThatAcknowledgesTheClaim(t *testing.T) {
	for _, kind := range []v1.ControlKind{v1.ControlCancel, v1.ControlInterrupt} {
		t.Run(string(kind), func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			h := &alsoSays{Hub: l.Hub}
			l.Hub = h
			e.enqueue(t, testRun("a", "s1"))
			ad := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}})
			x := e.executor(ad)
			l.Executor = x
			mustSync(t, l) // offered, and claimed here
			h.arm(v1.Control{Kind: kind, RunID: "a"})
			mustSync(t, l) // listed; the answer acknowledges it and stops it
			ended(t, x)

			if len(ad.Starts) != 0 {
				t.Errorf("the harness was started: %+v", ad.Starts)
			}
			if l.Pool.Free() != 1 {
				t.Errorf("capacity not released: free %d", l.Pool.Free())
			}
			res, ok := outboxResult(t, e, "a")
			if kind == v1.ControlCancel {
				if ok {
					t.Errorf("a withdrawn claim owes no result, and one is queued: %+v", res)
				}
				if _, err := e.store.GetRun(context.Background(), db.GetRunParams{Connection: "hub", ID: "a"}); err == nil {
					t.Error("the withdrawn claim is still in the store")
				}
				return
			}
			if !ok || res.State != v1.RunCancelled || res.Error != nil || res.Metrics.CancelLatencyMS == nil {
				t.Fatalf("result %+v, %v", res, ok)
			}
			l.Hub = h.Hub // the reporter talks to the real client
			e.reporter(l).Flush(context.Background())
			if got := e.hubState(t, "a"); got != "cancelled" {
				t.Errorf("hub state %s", got)
			}
		})
	}
}

// A steer in the answer that acknowledges the claim waits for the harness,
// as one sent while the run prepares does, rather than being dropped.
func TestSteerInTheAnswerThatAcknowledgesTheClaim(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	h := &alsoSays{Hub: l.Hub}
	l.Hub = h
	e.enqueue(t, testRun("a", "s1"))
	ad := fakeHarness(fake.Script{Hang: true})
	x := e.executor(ad)
	l.Executor = x
	mustSync(t, l)
	h.arm(v1.Control{Kind: v1.ControlSteer, RunID: "a", Text: "use tabs"})
	mustSync(t, l)
	eventually(t, "the steer reaches the turn", func() bool {
		return slices.Contains(statuses(runEvents(t, e, "a")), "steered")
	})
	if _, err := e.api(t).Cancel(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	mustSync(t, l)
	ended(t, x)
	if got := fake.Steered(ad.Turns()[0]); !slices.Equal(got, []string{"use tabs"}) {
		t.Errorf("steered %q", got)
	}
}

// An interrupt ends the turn and nothing more: no SIGTERM follows it. A
// harness that ignores it keeps going and ends as it would have.
func TestInterruptEndsTheTurn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script fake.Script
		state  v1.RunState
	}{
		{"the harness stops", fake.Script{Hang: true}, v1.RunCancelled},
		{"the harness carries on", fake.Script{
			Events: manyEvents(20), Delay: 5 * time.Millisecond, IgnoreInterrupt: true,
			Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "done anyway"},
		}, v1.RunSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			e.enqueue(t, testRun("a", "s1"))
			ad := fakeHarness(tc.script)
			x := e.executor(ad)
			running(t, e, l, x, "a")
			if _, err := e.api(t).Interrupt(context.Background(), "a"); err != nil {
				t.Fatal(err)
			}
			mustSync(t, l)
			mustSync(t, l) // delivered again; acted on once
			ended(t, x)
			res, _ := outboxResult(t, e, "a")
			if res.State != tc.state || res.Metrics.CancelLatencyMS == nil {
				t.Fatalf("result %+v", res)
			}
			if n, term := fake.Rungs(ad.Turns()[0]); n != 1 || term {
				t.Errorf("interrupts %d, SIGTERM %v; want 1, false", n, term)
			}
		})
	}
}

// Decision 0025: a harness whose answer was already on its way when the cancel
// reached it keeps that answer. A usage limit reported as cancelled would hide
// the one thing the hub needs to act on.
func TestAnswerBeforeTheCancelStands(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	limited := adapter.Outcome{State: v1.RunFailed, Error: &v1.RunError{Class: adapter.ClassUsageLimit, Message: "limit reached"}}
	x := e.executor(fakeHarness(fake.Script{Hang: true, Stopped: &limited}))
	running(t, e, l, x, "a")
	if _, err := e.api(t).Cancel(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	mustSync(t, l)
	ended(t, x)
	res, _ := outboxResult(t, e, "a")
	if res.State != v1.RunFailed || res.Error == nil || res.Error.Class != adapter.ClassUsageLimit || res.Metrics.CancelLatencyMS == nil {
		t.Fatalf("result %+v (error %+v)", res, res.Error)
	}
}

// A steer reaches the running turn once, however many syncs follow; one the
// harness will not take is an error event in the run's own stream, where the
// person who sent it is watching.
func TestSteerReachesTheTurn(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "taken", true: "refused"}[refuse], func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			e.enqueue(t, testRun("a", "s1"))
			script := fake.Script{Hang: true}
			if refuse {
				script.SteerError = "the run has already finished"
			}
			ad := fakeHarness(script)
			x := e.executor(ad)
			running(t, e, l, x, "a")
			api := e.api(t)
			if _, err := api.Steer(context.Background(), "a", "use tabs"); err != nil {
				t.Fatal(err)
			}
			mustSync(t, l)
			mustSync(t, l)
			// Controls are handed over, not acted on in the sync: wait for the
			// run's goroutine to note it.
			deadline := time.Now().Add(5 * time.Second)
			var evs []v1.Event
			for time.Now().Before(deadline) {
				if evs = runEvents(t, e, "a"); len(evs) > 0 {
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if _, err := api.Cancel(context.Background(), "a"); err != nil {
				t.Fatal(err)
			}
			mustSync(t, l)
			ended(t, x)
			got := fake.Steered(ad.Turns()[0])
			if refuse {
				if len(evs) != 1 || evs[0].Kind != v1.EventError || evs[0].Error.Class != ClassSteer || !strings.Contains(evs[0].Error.Message, "already finished") {
					t.Errorf("events %+v", evs)
				}
				return
			}
			if !slices.Equal(got, []string{"use tabs"}) {
				t.Errorf("steered %q", got)
			}
			if len(evs) != 1 || evs[0].Status != "steered" {
				t.Errorf("events %+v", evs)
			}
		})
	}
}

// A run the hub says this runner does not hold — lost while the runner was
// away — is stopped, not left running for nobody.
func TestTheHubsCancelStopsAHeldRun(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x := e.executor(fakeHarness(fake.Script{Hang: true}))
	running(t, e, l, x, "a")
	// The lease lapses on the hub's clock: the run is lost there.
	e.skew.Store(int64(24 * time.Hour))
	if err := e.hub.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.hubState(t, "a"); got != "lost" {
		t.Fatalf("hub state %s", got)
	}
	if res := mustSync(t, l); !slices.Equal(cancels(res), []string{"a"}) {
		t.Fatalf("cancels %v", cancels(res))
	}
	ended(t, x)
	if res, _ := outboxResult(t, e, "a"); res.State != v1.RunCancelled {
		t.Errorf("result %+v", res)
	}
}

// A watchdog's timed_out stands even when the hub interrupted the run too: a
// harness killed on the way down reports no result of its own, and that is
// the watchdog's doing, not the hub's.
func TestWatchdogWinsOverAnInterrupt(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	run := testRun("a", "s1")
	run.InactivityMS = 200
	e.enqueue(t, run)
	exited := adapter.Outcome{State: v1.RunFailed, Error: &v1.RunError{Class: adapter.ClassHarnessExited, Message: "killed"}}
	x := e.executor(fakeHarness(fake.Script{Hang: true, IgnoreInterrupt: true, Stopped: &exited}))
	running(t, e, l, x, "a")
	if _, err := e.api(t).Interrupt(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	mustSync(t, l)
	ended(t, x)
	res, _ := outboxResult(t, e, "a")
	if res.State != v1.RunTimedOut || res.Error == nil || res.Error.Class != ClassInactivity {
		t.Fatalf("result %+v (error %+v)", res, res.Error)
	}
}

// An interrupt that did not reach the harness is not counted: the hub's
// repeat at the next sync is tried again, and it is the one that lands.
func TestAFailedInterruptIsRetried(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	ad := fakeHarness(fake.Script{Hang: true, InterruptFails: 1})
	x := e.executor(ad)
	running(t, e, l, x, "a")
	if _, err := e.api(t).Interrupt(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	mustSync(t, l)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if n, _ := fake.Rungs(ad.Turns()[0]); n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first interrupt never reached the turn")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mustSync(t, l) // the hub repeats it
	ended(t, x)
	res, _ := outboxResult(t, e, "a")
	if res.State != v1.RunCancelled {
		t.Fatalf("result %+v", res)
	}
	if n, _ := fake.Rungs(ad.Turns()[0]); n != 2 {
		t.Errorf("%d interrupts, want 2", n)
	}
	var classes []string
	for _, ev := range runEvents(t, e, "a") {
		if ev.Kind == v1.EventError {
			classes = append(classes, ev.Error.Class)
		}
	}
	if !slices.Equal(classes, []string{ClassInterrupt}) {
		t.Errorf("error events %v", classes)
	}
}
