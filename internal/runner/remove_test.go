package runner

import (
	"context"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubclient"
)

// quietHub is a second hub with nothing to offer, counting the syncs it hears.
type quietHub struct{ syncs atomic.Int64 }

func (h *quietHub) Sync(context.Context, string, v1.SyncRequest) (v1.SyncResponse, error) {
	h.syncs.Add(1)
	return v1.SyncResponse{NextSyncMS: 5000}, nil
}

func (*quietHub) Result(context.Context, string, v1.Result) error { return nil }

func (*quietHub) Events(context.Context, string, v1.EventBatch) (v1.EventAck, error) {
	return v1.EventAck{}, nil
}

// removable is e.server wired as Serve wires a removal: the connections it
// started with, the profile whose config.toml a refused loop reads, and the
// collector ending what a removed connection leaves. config.toml lists the
// connection, and a loop the hub refuses waits for the owner's removal to
// reach it for as long as a test runs: whichever way the removal comes is
// the one the test chose.
func (e *env) removable(t *testing.T, l *Loop, x *Exec, d *Drain) *server {
	t.Helper()
	cfg := config.Default()
	cfg.Connections = []config.Connection{{Name: l.Connection, URL: e.url}}
	if err := config.Save(e.paths, cfg); err != nil {
		t.Fatal(err)
	}
	sv := e.server(l, x, d, time.Hour)
	sv.paths, sv.monitor, sv.grace = e.paths, NewMonitor(), time.Minute
	sv.configured = map[string]bool{l.Connection: true}
	sv.sessions.Configured = func(conn string) bool { return sv.configured[conn] && !sv.isRemoved(conn) }
	sv.sessions.Holds = x.Holds
	x.Ended = sv.sessions.Wake
	return sv
}

// deregister is what `yad disconnect` asks of the hub before it tells the
// daemon.
func (e *env) deregister(t *testing.T, runnerID string) {
	t.Helper()
	c, err := hubclient.New(e.url, e.cred)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Deregister(context.Background(), runnerID, "test"); err != nil {
		t.Fatal(err)
	}
}

// A running daemon told that a connection was removed lets that one go while
// another carries on: its loop stops, its run in hand is cancelled and — the
// hub having recorded it lost at the deregister — nothing about it is sent
// again; its session closes once the run has ended, is reported to nobody,
// and its workdir is reclaimed (DEV-81). Asking again finds it already gone.
func TestRemovingAConnectionStopsItAloneWhileAnotherCarriesOn(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 2)
	l.Clock = shortClock{}
	x := e.executor(fakeHarness(fake.Script{Hang: true}))
	d := NewDrain()
	sv := e.removable(t, l, x, d)
	quiet := &quietHub{}
	other := &Loop{Connection: "other", RunnerID: l.RunnerID, Hub: quiet, Store: e.store, Pool: l.Pool,
		Capabilities: l.Capabilities, Executor: x, Drain: d, Clock: shortClock{}}
	otherRep := NewReporter("other", quiet, e.store, sv.log)
	other.ClaimAfter = otherRep.Replayed()
	sv.loops = append(sv.loops, other)
	sv.reporters["other"] = otherRep
	sv.configured["other"] = true
	e.enqueue(t, testRun("a", "s1"))

	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- sv.run(ctx) }()
	eventually(t, "the run is running", func() bool {
		r, err := e.store.GetRun(ctx, dbRun("hub", "a"))
		return err == nil && r.State == string(v1.RunRunning)
	})
	workdir := session(t, e, "s1").Workdir
	if workdir == "" {
		t.Fatal("the running run's session has no workdir")
	}

	e.deregister(t, l.RunnerID)
	r, err := sv.monitor.RemoveConnection(ctx, "hub")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Known || r.Already || !slices.Equal(r.Stopped, []string{"a"}) || r.Closing+r.Closed != 1 || r.Remaining != 1 {
		t.Errorf("removal %+v, want known, run a stopped, its session closed or closing once the run has ended, one connection left", r)
	}
	eventually(t, "the stopped run's session is closed and its workdir reclaimed", func() bool {
		s := session(t, e, "s1")
		_, err := os.Stat(workdir)
		return s.State == "closed" && s.ReclaimedAt.Valid && os.IsNotExist(err)
	})
	s := session(t, e, "s1")
	if s.CloseReason.String != string(v1.SessionClosedByOwner) || !s.ReportedAt.Valid {
		t.Errorf("session closed for %q, reported %v; want closed by the owner and owed to nobody", s.CloseReason.String, s.ReportedAt.Valid)
	}
	if got := localRun(t, e, "a").State; got != string(v1.RunCancelled) {
		t.Errorf("the run in hand ended %s here, want cancelled", got)
	}
	eventually(t, "nothing is owed to the removed hub", func() bool {
		o, err1 := e.store.OutboxDepth(ctx)
		sp, err2 := e.store.SpoolDepth(ctx)
		return err1 == nil && err2 == nil && o == 0 && sp == 0
	})
	if got := e.hubState(t, "a"); got != string(v1.RunLost) {
		t.Errorf("the hub has the run %s; the deregister made it lost, and nothing after may change that", got)
	}
	snap, err := sv.monitor.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, listed := snap.Connections["hub"]; listed || !snap.Removed["hub"] {
		t.Errorf("status still lists the removed connection: %+v", snap)
	}

	before := quiet.syncs.Load()
	eventually(t, "the other connection keeps syncing", func() bool { return quiet.syncs.Load() > before+1 })
	select {
	case err := <-done:
		t.Fatalf("the runner went down with a connection left: %v", err)
	default:
	}
	if again, err := sv.monitor.RemoveConnection(ctx, "hub"); err != nil || !again.Already {
		t.Errorf("a second removal: %+v %v, want already removed", again, err)
	}

	d.Begin("test")
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the drain returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the drain never finished")
	}
}

// A runner whose every connection the owner removed is not a runner whose
// every connection failed: it stays up, collecting, as one started with none
// does, and goes cleanly when asked (finding 4 of the parked branch: the
// daemon has to be up for the workdirs to go, and a restart is what makes it
// sync with a hub connected after).
func TestTheLastConnectionRemovedLeavesTheRunnerCollecting(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	l.Clock = shortClock{}
	x := e.executor(fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}}))
	d := NewDrain()
	sv := e.removable(t, l, x, d)
	e.enqueue(t, testRun("a", "s1"))
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- sv.run(ctx) }()
	eventually(t, "the hub has the run's result", func() bool { return e.hubState(t, "a") == string(v1.RunSucceeded) })

	e.deregister(t, l.RunnerID)
	r, err := sv.monitor.RemoveConnection(ctx, "hub")
	if err != nil {
		t.Fatal(err)
	}
	if r.Closed != 1 || r.Remaining != 0 {
		t.Errorf("removal %+v, want its idle session closed and nothing left", r)
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the runner went down when its last connection was removed: %v", err)
	default:
	}
	d.Begin("test")
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a runner with every connection removed went down with %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the drain never finished")
	}
}

// A daemon nobody told: `yad disconnect` retired the runner at the hub and
// took the connection out of config.toml, and the control socket did not
// answer. The loop's next sync is refused, and because config.toml no longer
// lists the connection, that refusal is the removal: the session closes now
// rather than at the idle TTL, and the runner is not failing. A connection
// config.toml still lists is a fault instead — a credential replaced at the
// hub, which keeps the runner's sessions — and its sessions are kept.
func TestARefusedLoopOfARemovedConnectionEndsWhatItLeft(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listed bool
	}{{"removed from config.toml", false}, {"still in config.toml", true}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			l.Clock = shortClock{}
			x := e.executor(fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}}))
			d := NewDrain()
			sv := e.removable(t, l, x, d)
			// Past the grace, a refusal of a connection still listed is a
			// fault.
			sv.grace = 300 * time.Millisecond
			if !tc.listed {
				if err := config.Save(e.paths, config.Default()); err != nil {
					t.Fatal(err)
				}
			}
			e.enqueue(t, testRun("a", "s1"))
			ctx := context.Background()
			done := make(chan error, 1)
			go func() { done <- sv.run(ctx) }()
			eventually(t, "the hub has the run's result", func() bool { return e.hubState(t, "a") == string(v1.RunSucceeded) })

			e.deregister(t, l.RunnerID)
			if tc.listed {
				select {
				case err := <-done:
					if err == nil {
						t.Error("the runner's only connection was refused, and it went down reporting nothing")
					}
				case <-time.After(20 * time.Second):
					t.Fatal("the runner did not go down with its only connection refused")
				}
				if s := session(t, e, "s1"); s.State != "open" {
					t.Errorf("a refused connection still configured had its session %s", s.State)
				}
				return
			}
			eventually(t, "the removed connection's session is closed", func() bool {
				return session(t, e, "s1").State == "closed"
			})
			if !sv.isRemoved("hub") {
				t.Error("the refused connection is not recorded as removed")
			}
			d.Begin("test")
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("the runner reported the removal as a failure: %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the drain never finished")
			}
		})
	}
}
