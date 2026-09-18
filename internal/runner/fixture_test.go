package runner

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hub"
	hubstore "github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store"
)

// fakeClock records every wait the loop asks for and lets it through at once,
// so the loop's timing is asserted without a sleep.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	waits []time.Duration
	// stopAfter cancels the loop once this many waits were requested.
	stopAfter int
	cancel    context.CancelFunc
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	if c.stopAfter > 0 && len(c.waits) >= c.stopAfter {
		c.cancel()
		return ch
	}
	ch <- c.now
	return ch
}

// executor records what the loop hands it.
type executor struct {
	mu       sync.Mutex
	started  []Claim
	controls []v1.Control
}

func (e *executor) Start(_ context.Context, c Claim) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.started = append(e.started, c)
}

func (e *executor) Control(_ context.Context, _ string, c v1.Control) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.controls = append(e.controls, c)
}

func (e *executor) ids() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for _, c := range e.started {
		out = append(out, c.Run.RunID)
	}
	return out
}

// drivableDoc is a document for a runner that drives claude, as it will be
// once its adapter lands.
func drivableDoc(id string, capacity int) v1.Capabilities {
	return v1.Capabilities{
		RunnerID: id, Name: id, YadVersion: "dev", OS: "linux", Arch: "amd64",
		Harnesses: []v1.HarnessReport{{ID: "claude", Label: "Claude Code", Kind: "first-class", Present: true, Version: "2.1.276"}},
		Capacity:  v1.Capacity{Total: capacity},
	}
}

func testRun(id, session string) v1.Run {
	return v1.Run{RunID: id, Session: v1.SessionRef{ID: session, New: true}, Harness: "claude", Model: "opus", Brief: v1.Brief{Instruction: "do " + id}}
}

type env struct {
	hub      *hub.Hub
	hubStore *hubstore.Store
	url      string
	paths    config.Paths
	store    *store.Store
	exec     *executor
	clock    *fakeClock
}

// newEnv starts yad hub in process on loopback and gives a runner profile in
// temporary directories.
func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	hs, err := hubstore.Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hs.Close() })
	h := hub.New(hub.Options{Store: hs})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	p := config.Paths{Profile: "test", Config: t.TempDir(), Data: t.TempDir()}
	rs, err := store.Open(ctx, p.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	return &env{hub: h, hubStore: hs, url: srv.URL + hub.BasePath, paths: p, store: rs, exec: &executor{}, clock: &fakeClock{now: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)}}
}

func (e *env) token(t *testing.T) string {
	t.Helper()
	tok, _, err := hub.IssueRegistrationToken(context.Background(), e.hubStore, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// loop registers a runner with the hub directly and returns a loop for it.
func (e *env) loop(t *testing.T, capacity int) *Loop {
	t.Helper()
	ctx := context.Background()
	id, err := e.paths.RunnerID()
	if err != nil {
		t.Fatal(err)
	}
	doc := drivableDoc(id, capacity)
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
	return &Loop{
		Connection: "hub", RunnerID: id, Hub: c, Store: e.store, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Executor: e.exec, Clock: e.clock,
		Rand: func() float64 { return 0.5 }, // no jitter: factor exactly 1
	}
}

func (e *env) enqueue(t *testing.T, runs ...v1.Run) {
	t.Helper()
	for _, r := range runs {
		if err := e.hubStore.EnqueueRun(context.Background(), r, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) hubState(t *testing.T, runID string) string {
	t.Helper()
	r, err := e.hubStore.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return r.State
}

func mustSync(t *testing.T, l *Loop) v1.SyncResponse {
	t.Helper()
	res, err := l.SyncOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}
