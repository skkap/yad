package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store/db"
)

// The whole claim path against yad hub: offered, recorded locally, listed,
// acknowledged, started — and only then.
func TestClaimByListing(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 2)
	withGrant := testRun("a", "s1")
	withGrant.Grants = []v1.Grant{{Name: "ZUMINO_TOKEN", Value: "grant-secret", As: v1.GrantEnv}}
	e.enqueue(t, withGrant)

	res := mustSync(t, l)
	if len(res.Runs) != 1 || e.hubState(t, "a") != "offered" {
		t.Fatalf("offer: %d runs, hub state %s", len(res.Runs), e.hubState(t, "a"))
	}
	if got := e.exec.ids(); len(got) != 0 {
		t.Fatalf("started %v before the claim was acknowledged", got)
	}
	local, err := e.store.GetRun(context.Background(), db.GetRunParams{Connection: "hub", ID: "a"})
	if err != nil || local.State != "claimed" {
		t.Fatalf("local run = %+v, %v", local, err)
	}
	if strings.Contains(local.Spec, "grant-secret") {
		t.Error("a grant was written to the runner's disk")
	}
	if l.Pool.Free() != 1 {
		t.Errorf("free capacity %d while a claim is pending, want 1", l.Pool.Free())
	}

	mustSync(t, l)
	if e.hubState(t, "a") != "claimed" {
		t.Errorf("hub state after listing = %s", e.hubState(t, "a"))
	}
	if got := e.exec.ids(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("started %v, want [a]", got)
	}
	c := e.exec.started[0]
	if c.Connection != "hub" || len(c.Run.Grants) != 1 || c.Run.Grants[0].Value != "grant-secret" {
		t.Errorf("claim handed over = %+v", c)
	}

	// The executor owns the capacity now; a third sync starts nothing twice.
	mustSync(t, l)
	if got := e.exec.ids(); len(got) != 1 {
		t.Errorf("started %v", got)
	}
	c.Release()
	if l.Pool.Free() != 2 {
		t.Errorf("free capacity after release = %d, want 2", l.Pool.Free())
	}
}

// The runner declares only what it has reserved, and the hub offers no more.
func TestNeverClaimsPastFreeCapacity(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"), testRun("b", "s2"))
	mustSync(t, l)
	mustSync(t, l)
	if got := e.exec.ids(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("started %v, want [a]", got)
	}
	if e.hubState(t, "b") != "queued" {
		t.Errorf("b is %s with no capacity free", e.hubState(t, "b"))
	}
	e.exec.started[0].Release()
	mustSync(t, l)
	mustSync(t, l)
	if got := e.exec.ids(); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("started %v after a finished, want [a b]", got)
	}
}

// With nothing to run claimed runs with, a runner stays known to the hub and
// takes nothing.
func TestNoExecutorClaimsNothing(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 4)
	l.Executor = nil
	e.enqueue(t, testRun("a", "s1"))
	if res := mustSync(t, l); len(res.Runs) != 0 {
		t.Errorf("offered %d runs with no executor", len(res.Runs))
	}
	if e.hubState(t, "a") != "queued" {
		t.Errorf("hub state %s", e.hubState(t, "a"))
	}
}

// A claim the hub takes back before it is acknowledged never starts, and
// its capacity comes back.
func TestWithdrawnClaimNeverStarts(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	mustSync(t, l)
	// The hub moves on without this runner and settles the run for good.
	if _, err := e.hubStore.DB.Exec(`UPDATE runs SET state = 'lost' WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	mustSync(t, l)
	if got := e.exec.ids(); len(got) != 0 {
		t.Fatalf("started %v after the hub cancelled it", got)
	}
	if _, err := e.store.GetRun(context.Background(), db.GetRunParams{Connection: "hub", ID: "a"}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("withdrawn run still in the store: %v", err)
	}
	if _, err := e.store.GetSession(context.Background(), db.GetSessionParams{Connection: "hub", ID: "s1"}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the session the withdrawn claim opened is still there: %v", err)
	}
	if l.Pool.Free() != 1 {
		t.Errorf("free capacity %d, want 1", l.Pool.Free())
	}
	if res := mustSync(t, l); len(res.Controls) != 0 {
		t.Errorf("still listed after withdrawal: %+v", res.Controls)
	}
}

// The case the withdrawal exists for: the runner was away past the offer's
// lease and the hub put the run back in its queue. The next sync cancels the
// stale claim, which frees its capacity; the one after is offered the run
// again, and the runner takes it — it starts once, here.
func TestWithdrawnClaimCanBeOfferedAgain(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	mustSync(t, l)
	if _, err := e.hubStore.DB.Exec(`UPDATE runs SET state = 'queued', runner_id = NULL, lease_expires_at = NULL WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	res := mustSync(t, l)
	if !slices.ContainsFunc(res.Controls, func(c v1.Control) bool { return c.Kind == v1.ControlCancel && c.RunID == "a" }) {
		t.Fatalf("stale claim not cancelled: %+v", res.Controls)
	}
	if res := mustSync(t, l); len(res.Runs) != 1 {
		t.Fatalf("not offered again once its capacity was free: %d runs", len(res.Runs))
	}
	mustSync(t, l)
	if got := e.exec.ids(); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("started %v, want [a] once", got)
	}
	if e.hubState(t, "a") != "claimed" {
		t.Errorf("hub state %s", e.hubState(t, "a"))
	}
}

// report_capabilities is answered with the full document on the next sync.
func TestReportCapabilities(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	mustSync(t, l) // first sync of the process carries the document
	if _, err := e.hubStore.DB.Exec(`UPDATE runners SET wants_capabilities = 1, capabilities = '{}'`); err != nil {
		t.Fatal(err)
	}
	res := mustSync(t, l)
	if !slices.ContainsFunc(res.Controls, func(c v1.Control) bool { return c.Kind == v1.ControlReportCapabilities }) {
		t.Fatalf("no report_capabilities in %+v", res.Controls)
	}
	mustSync(t, l)
	r, err := e.hubStore.GetRunner(context.Background(), l.RunnerID)
	if err != nil || r.WantsCapabilities != 0 || !strings.Contains(r.Capabilities, `"claude"`) {
		t.Errorf("hub's runner after the report = %+v, %v", r, err)
	}
}

// scriptedHub answers every sync with the same runs — a hub that checks
// nothing — and records what it is told.
type scriptedHub struct {
	mu      sync.Mutex
	offer   []v1.Run
	syncs   []v1.SyncRequest
	results map[string]v1.Result
	fail    error
}

func (h *scriptedHub) Sync(_ context.Context, _ string, req v1.SyncRequest) (v1.SyncResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.syncs = append(h.syncs, req)
	if h.fail != nil {
		return v1.SyncResponse{}, h.fail
	}
	return v1.SyncResponse{NextSyncMS: 15000, Runs: h.offer}, nil
}

func (h *scriptedHub) Result(_ context.Context, runID string, res v1.Result) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.results == nil {
		h.results = map[string]v1.Result{}
	}
	h.results[runID] = res
	return nil
}

// A hub is untrusted input. Whatever it offers, the runner claims only what
// it can drive, in sessions it can resume, within its capacity — and tells the
// hub why it refused the rest.
func TestRefusesWhatItCannotRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "mine", Harness: "claude", CreatedAt: 1, LastUsedAt: 1}); err != nil {
		t.Fatal(err)
	}
	codex := testRun("codex", "s-codex")
	codex.Harness = "codex"
	unknown := testRun("unknown-session", "elsewhere")
	unknown.Session.New = false
	reused := testRun("reused-session", "mine")
	live := testRun("live", "s-live")
	live.Session.Mode = v1.SessionLive
	noModel := testRun("no-model", "s-nm")
	noModel.Model = ""
	h := &scriptedHub{offer: []v1.Run{codex, unknown, reused, live, noModel, testRun("ok", "s-ok"), testRun("over", "s-over")}}
	doc := drivableDoc("r", 1)
	l := &Loop{Connection: "hub", RunnerID: "r", Hub: h, Store: e.store, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Executor: e.exec, Clock: e.clock}

	mustSync(t, l)
	want := map[string]string{
		"codex": "not one this runner can drive", "unknown-session": "does not hold session",
		"reused-session": "already has a session", "live": "live sessions", "no-model": "model is required",
	}
	for id, msg := range want {
		r, ok := h.results[id]
		if !ok || r.State != v1.RunFailed || r.Error == nil || r.Error.Class != "refused" || !strings.Contains(r.Error.Message, msg) {
			t.Errorf("%s: result %+v, want a refusal mentioning %q", id, r, msg)
		}
	}
	if _, ok := h.results["ok"]; ok {
		t.Error("the runnable run was refused")
	}
	if _, ok := h.results["over"]; ok {
		t.Error("a run past capacity was refused rather than left for a re-offer")
	}

	mustSync(t, l)
	last := h.syncs[len(h.syncs)-1]
	var listed []string
	for _, r := range last.Runs {
		listed = append(listed, r.RunID)
	}
	if !slices.Equal(listed, []string{"ok"}) {
		t.Errorf("listed %v, want only [ok]", listed)
	}
	if got := e.exec.ids(); !slices.Equal(got, []string{"ok"}) {
		t.Errorf("started %v", got)
	}
}

// Two runs of one session never run at once, whatever the hub sends.
func TestOneLiveRunPerSession(t *testing.T) {
	e := newEnv(t)
	second := testRun("b", "s1")
	second.Session.New = false
	h := &scriptedHub{offer: []v1.Run{testRun("a", "s1")}}
	doc := drivableDoc("r", 4)
	l := &Loop{Connection: "hub", RunnerID: "r", Hub: h, Store: e.store, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Executor: e.exec, Clock: e.clock}
	mustSync(t, l)
	h.offer = []v1.Run{second}
	mustSync(t, l)
	if r, ok := h.results["b"]; !ok || !strings.Contains(r.Error.Message, "one at a time") {
		t.Errorf("second run in a busy session: %+v", r)
	}
}

// Timing: the hub's interval with jitter, an immediate re-sync to confirm new
// claims, and backoff from 1 s to 30 s while the hub is unreachable.
func TestLoopTiming(t *testing.T) {
	e := newEnv(t)
	h := &scriptedHub{}
	doc := drivableDoc("r", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.clock.stopAfter, e.clock.cancel = 3, cancel
	l := &Loop{Connection: "hub", RunnerID: "r", Hub: h, Store: e.store, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Executor: e.exec, Clock: e.clock, Rand: func() float64 { return 0 }}
	h.offer = []v1.Run{testRun("a", "s1")}
	if err := l.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// Offer → re-sync at once to list it; then the hub's 15 s less 10 %.
	if want := []time.Duration{0, 13500 * time.Millisecond, 13500 * time.Millisecond}; !slices.Equal(e.clock.waits, want) {
		t.Errorf("waits %v, want %v", e.clock.waits, want)
	}

	e2 := newEnv(t)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	e2.clock.stopAfter, e2.clock.cancel = 8, cancel2
	down := &scriptedHub{fail: errors.New("connection refused")}
	l2 := &Loop{Connection: "hub", RunnerID: "r", Hub: down, Store: e2.store, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Executor: e2.exec, Clock: e2.clock, Rand: func() float64 { return 0.5 }}
	if err := l2.Run(ctx2); err != nil {
		t.Fatal(err)
	}
	s := time.Second
	if want := []time.Duration{s, 2 * s, 4 * s, 8 * s, 16 * s, 30 * s, 30 * s, 30 * s}; !slices.Equal(e2.clock.waits, want) {
		t.Errorf("backoff %v, want %v", e2.clock.waits, want)
	}
}

func TestIntervalBoundsAndJitter(t *testing.T) {
	for _, tc := range []struct {
		ms   int
		want time.Duration
	}{
		{0, 15 * time.Second}, {1, 5 * time.Second}, {20000, 20 * time.Second}, {3600000, 60 * time.Second},
	} {
		if got := interval(tc.ms); got != tc.want {
			t.Errorf("interval(%d) = %s, want %s", tc.ms, got, tc.want)
		}
	}
	for r, want := range map[float64]time.Duration{0: 9 * time.Second, 0.5: 10 * time.Second, 0.999999: 11 * time.Second} {
		l := &Loop{Rand: func() float64 { return r }}
		if got := l.jitter(10 * time.Second).Round(time.Millisecond); got != want {
			t.Errorf("jitter at %v = %s, want %s", r, got, want)
		}
	}
}

// A refused credential is the owner's to fix; the loop stops and says how.
func TestLoopStopsOnARefusedCredential(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	bad, err := hubclient.New(e.url, "yadrun_revoked")
	if err != nil {
		t.Fatal(err)
	}
	l.Hub = bad
	err = l.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "yad connect") {
		t.Errorf("err = %v, want a stop naming yad connect", err)
	}
	if len(e.clock.waits) != 0 {
		t.Errorf("retried a refused credential: %v", e.clock.waits)
	}
}

// Runs a previous process held have no process now; they are lost locally
// and no longer listed, so their leases lapse and the hub reports them.
func TestOrphansAreAbandoned(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "s1", Harness: "claude", CreatedAt: 1, LastUsedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateRun(ctx, db.CreateRunParams{Connection: "hub", ID: "old", SessionID: "s1", Harness: "claude", Spec: "{}", CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	e.clock.stopAfter, e.clock.cancel = 1, cancel
	h := &scriptedHub{}
	doc := drivableDoc("r", 1)
	l := &Loop{Connection: "hub", RunnerID: "r", Hub: h, Store: e.store, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Executor: e.exec, Clock: e.clock}
	if err := l.Run(cctx); err != nil {
		t.Fatal(err)
	}
	if len(h.syncs) != 1 || len(h.syncs[0].Runs) != 0 {
		t.Errorf("listed %+v", h.syncs)
	}
	r, _ := e.store.GetRun(ctx, db.GetRunParams{Connection: "hub", ID: "old"})
	if r.State != "lost" || r.Reason != (sql.NullString{String: "the runner restarted while holding it", Valid: true}) {
		t.Errorf("orphan = %+v", r)
	}

	// The hub, which never saw it claimed, offers it again: refused, not
	// dropped, so the offers stop.
	h.offer = []v1.Run{testRun("old", "s1")}
	mustSync(t, l)
	if res, ok := h.results["old"]; !ok || !strings.Contains(res.Error.Message, "not run twice") {
		t.Errorf("re-offered orphan: %+v, %v", res, ok)
	}
	if got := e.exec.ids(); len(got) != 0 {
		t.Errorf("started %v", got)
	}
}

// Refusals the hub cannot take yet are kept and sent again; ones it answered
// with a 4xx are done.
func TestRefusalsRetryOnlyTransientFailures(t *testing.T) {
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/sync") {
			json.NewEncoder(w).Encode(v1.SyncResponse{NextSyncMS: 15000})
			return
		}
		calls[r.URL.Path]++
		w.Header().Set("Content-Type", "application/json")
		status := http.StatusNotImplemented
		if strings.Contains(r.URL.Path, "gone") {
			status = http.StatusNotFound
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v1.ErrorEnvelope{Error: v1.Error{Code: "x", Message: "x", NextAction: "x"}})
	}))
	defer srv.Close()
	c, _ := hubclient.New(srv.URL, "cred")
	e := newEnv(t)
	doc := drivableDoc("r", 1)
	l := &Loop{Connection: "hub", RunnerID: "r", Hub: c, Store: e.store, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Executor: e.exec, Clock: e.clock}
	l.init()
	l.refuse("later", "x")
	l.refuse("gone", "x")
	mustSync(t, l)
	mustSync(t, l)
	if calls["/runs/later/result"] != 2 || calls["/runs/gone/result"] != 1 {
		t.Errorf("result calls %v", calls)
	}
}

// A hub that stalls on results cannot hold the next sync back for long:
// refusals go out a few per sync, after the reservation is returned.
func TestRefusalsAreBoundedPerSync(t *testing.T) {
	e := newEnv(t)
	h := &scriptedHub{}
	doc := drivableDoc("r", 2)
	l := &Loop{Connection: "hub", RunnerID: "r", Hub: h, Store: e.store, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Executor: e.exec, Clock: e.clock}
	l.init()
	for i := range 3 * refusalsPerSync {
		l.refuse(string(rune('a'+i)), "x")
	}
	mustSync(t, l)
	if len(h.results) != refusalsPerSync {
		t.Errorf("sent %d refusals in one sync, want %d", len(h.results), refusalsPerSync)
	}
	if l.Pool.Free() != 2 {
		t.Errorf("capacity held after the sync: free %d", l.Pool.Free())
	}
	mustSync(t, l)
	mustSync(t, l)
	if len(h.results) != 3*refusalsPerSync || len(l.refused) != 0 {
		t.Errorf("after three syncs: sent %d, still owed %d", len(h.results), len(l.refused))
	}
}
