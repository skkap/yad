package runner

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/store/db"
)

// What `yad status` shows per connection follows the loop: retrying with the
// error while the hub is down, syncing once it answers.
func TestMonitorFollowsTheLoop(t *testing.T) {
	e := newEnv(t)
	m := NewMonitor()
	h := &scriptedHub{fail: errors.New("connection refused")}
	doc := drivableDoc("r", 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.clock.stopAfter, e.clock.cancel = 1, cancel
	l := &Loop{Connection: "hub", RunnerID: "r", Hub: h, Store: e.store, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Executor: e.exec, Clock: e.clock, Monitor: m}
	if err := l.Run(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := s.Connections["hub"]
	if c.State != ConnRetrying || c.LastError != "connection refused" || c.LastErrorAt.IsZero() || !c.LastSync.IsZero() {
		t.Errorf("hub down: %+v", c)
	}

	h.fail = nil
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	e.clock.waits, e.clock.cancel = nil, cancel
	if err := l.Run(ctx); err != nil {
		t.Fatal(err)
	}
	s, _ = m.Snapshot(context.Background())
	if c := s.Connections["hub"]; c.State != ConnSyncing || c.LastSync.IsZero() || c.LastError != "connection refused" {
		t.Errorf("hub back: %+v — the last error stays for the owner to read", c)
	}
}

// Idle is the moment a self-update takes over at (decision 0069): the runner
// set up, every unit of capacity free — none held by a run or by a sync that
// may yet claim one — and no hub login waiting on a person.
func TestMonitorIdle(t *testing.T) {
	var none *Monitor
	if none.Idle() {
		t.Error("a nil monitor is idle")
	}
	m := NewMonitor()
	pool := NewPool(v1.Capacity{Total: 2})
	m.attach(pool, nil, nil)
	if m.Idle() {
		t.Error("idle before the runner is set up")
	}
	m.markReady()
	if !m.Idle() {
		t.Fatal("a ready runner with nothing in progress is not idle")
	}
	res := pool.Reserve("hub")
	if m.Idle() {
		t.Error("idle while a sync holds capacity it may claim with")
	}
	release, ok := res.Take("claude")
	res.Close()
	if !ok || m.Idle() {
		t.Errorf("idle with a run holding a unit (taken %v)", ok)
	}
	release()
	if !m.Idle() {
		t.Error("not idle once the run let its unit go")
	}
	logins := &Logins{}
	logins.init()
	logins.live[account.Ref{Harness: "claude", Label: "main"}] = &hubLogin{}
	m.attachLogins(logins)
	if m.Idle() {
		t.Error("idle while a hub login waits on a person")
	}
	m.attach(nil, nil, nil)
	m.attachLogins(nil)
	if m.Idle() {
		t.Error("idle after Serve let its pool go")
	}
}

// A connection Serve cannot start is stopped, with the reason, and the store
// is let go before Serve returns.
func TestMonitorThroughServe(t *testing.T) {
	e := newEnv(t)
	m := NewMonitor()
	cfg := config.Default()
	cfg.Connections = []config.Connection{{Name: "gone", URL: e.url}}
	doc := drivableDoc("r", 3)
	err := Serve(context.Background(), Options{
		Paths: e.paths, Config: cfg, RunnerID: "r", Capabilities: func() v1.Capabilities { return doc },
		Log: slog.New(slog.DiscardHandler), Monitor: m,
	})
	if err == nil {
		t.Fatal("Serve ran a connection with no credential")
	}
	s, serr := m.Snapshot(context.Background())
	if serr != nil {
		t.Fatalf("a snapshot after Serve returned: %v", serr)
	}
	if c := s.Connections["gone"]; c.State != ConnStopped || !strings.Contains(c.LastError, "connect '<hub url>' --name gone") {
		t.Errorf("connection without a credential: %+v", c)
	}
	if s.Capacity != nil {
		t.Errorf("capacity %+v after Serve returned", s.Capacity)
	}
	if m.Ready() {
		t.Error("a runner none of whose connections started reads as ready")
	}
}

// Ready follows setup: at once with no connection, once a loop runs with one.
func TestMonitorReady(t *testing.T) {
	e := newEnv(t)
	doc := drivableDoc("r", 1)
	for _, tc := range []struct {
		name  string
		conns []config.Connection
		cred  bool
	}{
		{"no connection", nil, false},
		{"a connection with its credential", []config.Connection{{Name: "home", URL: e.url}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.cred {
				if err := e.paths.SaveCredential("home", "yadrun_not_registered"); err != nil {
					t.Fatal(err)
				}
			}
			m := NewMonitor()
			cfg := config.Default()
			cfg.Connections = tc.conns
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- Serve(ctx, Options{Paths: e.paths, Config: cfg, RunnerID: "r",
					Capabilities: func() v1.Capabilities { return doc }, Log: slog.New(slog.DiscardHandler), Monitor: m})
			}()
			deadline := time.Now().Add(5 * time.Second)
			for !m.Ready() && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if !m.Ready() {
				t.Error("never ready")
			}
			cancel()
			<-done
		})
	}
}

func TestMonitorReadsTheStore(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "s1", Harness: "claude", CreatedAt: 1, LastUsedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateRun(ctx, db.CreateRunParams{Connection: "hub", ID: "r1", SessionID: "s1", Harness: "claude", Spec: "{}", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	m := NewMonitor()
	p := NewPool(v1.Capacity{Total: 4})
	res := p.Reserve("hub")
	res.Take("claude")
	res.Close()
	m.attach(p, e.store, nil)
	s, err := m.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Capacity == nil || s.Capacity.Total != 4 || s.Capacity.Free != 3 {
		t.Errorf("capacity %+v, want 3 of 4 free", s.Capacity)
	}
	if len(s.Held) != 1 || s.Held[0].ID != "r1" || s.Sessions != 1 {
		t.Errorf("held %+v, sessions %d", s.Held, s.Sessions)
	}
}
