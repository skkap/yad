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
	mu       sync.Mutex
	offer    []v1.Run
	controls []v1.Control
	syncs    []v1.SyncRequest
	results  map[string]v1.Result
	fail     error
}

func (h *scriptedHub) Sync(_ context.Context, _ string, req v1.SyncRequest) (v1.SyncResponse, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.syncs = append(h.syncs, req)
	if h.fail != nil {
		return v1.SyncResponse{}, h.fail
	}
	return v1.SyncResponse{NextSyncMS: 15000, Runs: h.offer, Controls: h.controls}, nil
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

// Whatever a hub offers, the runner claims only what it can drive, in sessions
// it can resume, within its capacity — and tells the hub why it refused the
// rest. Not because the hub is an attacker (0038), but because this runner is
// the one that has to run it.
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
	// A grant that would break the run is refused whole, never run with it
	// stripped, and the deny list is not escaped by case (decision 0038).
	loader := testRun("loader", "s-ld")
	loader.Grants = []v1.Grant{{Name: "LD_PRELOAD", Value: "/evil.so", As: v1.GrantEnv}}
	lowerHome := testRun("lower-home", "s-lh")
	lowerHome.Grants = []v1.Grant{{Name: "ZUMINO_TOKEN", Value: "t", As: v1.GrantEnv}, {Name: "home", Value: "/tmp", As: v1.GrantFile}}
	// Names decision 0024 refused and 0038 accepts: this is the run that is
	// claimed and started, grants and all.
	ok := testRun("ok", "s-ok")
	ok.Grants = []v1.Grant{
		{Name: "ANTHROPIC_BASE_URL", Value: "https://hub", As: v1.GrantEnv},
		{Name: "AWS_SECRET_ACCESS_KEY", Value: "k", As: v1.GrantFile},
		{Name: "http_proxy", Value: "http://p", As: v1.GrantEnv},
	}
	h := &scriptedHub{offer: []v1.Run{codex, unknown, reused, live, noModel, loader, lowerHome, ok, testRun("over", "s-over")}}
	doc := drivableDoc("r", 1)
	l := &Loop{Connection: "hub", RunnerID: "r", Hub: h, Store: e.store, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Executor: e.exec, Clock: e.clock}

	mustSync(t, l)
	want := map[string]string{
		"codex": "not one this runner can drive", "unknown-session": "does not hold session",
		"reused-session": "already has a session", "live": "live sessions", "no-model": "model is required",
		"loader": "LD_", "lower-home": "HOME",
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

// Runs a previous process held have no process now. Each that began is
// reported lost — never run again — with a result that names the last event it
// spooled, and stays listed until that result is delivered. Its session keeps
// its native id and its workdir, so a new run can resume it. A claim that
// never began — maybe never even listed back — is withdrawn instead, with the
// empty session it opened.
func TestOrphansAreReportedLost(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, s := range []string{"s1", "s2"} {
		if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: s, Harness: "claude", Workdir: "/work/" + s, CreatedAt: 1, LastUsedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.store.SetSessionNativeID(ctx, db.SetSessionNativeIDParams{NativeID: sql.NullString{String: "native-1", Valid: true}, LastUsedAt: 1, Connection: "hub", ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	for id, sess := range map[string]string{"running": "s1", "claimed": "s2"} {
		if err := e.store.CreateRun(ctx, db.CreateRunParams{Connection: "hub", ID: id, SessionID: sess, Harness: "claude", Spec: "{}", CreatedAt: 1, UpdatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.store.SetRunState(ctx, db.SetRunStateParams{State: "running", UpdatedAt: 1, Connection: "hub", ID: "running"}); err != nil {
		t.Fatal(err)
	}
	for seq := int64(1); seq <= 3; seq++ {
		if err := e.store.AppendEvent(ctx, db.AppendEventParams{Connection: "hub", RunID: "running", Seq: seq, Body: `{"kind":"text"}`}); err != nil {
			t.Fatal(err)
		}
	}
	// Acknowledged events count too: last_seq is the run's, not the spool's.
	if err := e.store.AckEvents(ctx, db.AckEventsParams{AckedThrough: 2, Connection: "hub", RunID: "running"}); err != nil {
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
	r := localRun(t, e, "running")
	if r.State != "lost" || !strings.Contains(r.Reason.String, "never run twice") {
		t.Errorf("orphan = %+v", r)
	}
	res, ok := outboxResult(t, e, "running")
	if !ok || res.State != v1.RunLost || res.Error == nil || res.Error.Class != ClassRunnerRestarted || res.LastSeq != 3 {
		t.Errorf("result owed: %+v (error %+v), %v; want lost, %s, last_seq 3", res, res.Error, ok, ClassRunnerRestarted)
	}
	if _, err := e.store.GetRun(ctx, db.GetRunParams{Connection: "hub", ID: "claimed"}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("an unstarted claim is still recorded: %v", err)
	}
	if _, ok := outboxResult(t, e, "claimed"); ok {
		t.Error("an unstarted claim owes a result")
	}
	if _, err := e.store.GetSession(ctx, db.GetSessionParams{Connection: "hub", ID: "s2"}); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the empty session the claim opened is still recorded: %v", err)
	}
	// Listed until the result lands, so its lease outlasts a hub outage.
	if len(h.syncs) != 1 || len(h.syncs[0].Runs) != 1 || h.syncs[0].Runs[0].Reason != "reporting its result" {
		t.Errorf("listed %+v", h.syncs)
	}
	sess, err := e.store.GetSession(ctx, db.GetSessionParams{Connection: "hub", ID: "s1"})
	if err != nil || sess.NativeID.String != "native-1" || sess.Workdir != "/work/s1" || sess.State != "open" {
		t.Errorf("session after the restart %+v, %v: its native id, workdir and state must survive", sess, err)
	}
	if sess.LastUsedAt <= 1 {
		t.Errorf("session last used at %d after its run was reported lost; the lost result must move it as any result does", sess.LastUsedAt)
	}

	// Recovering again is a no-op: nothing is reported twice.
	if err := l.Recover(ctx); err != nil {
		t.Fatal(err)
	}

	// The hub offers one again: refused, not run, so the offers stop.
	h.offer = []v1.Run{testRun("running", "s1")}
	mustSync(t, l)
	if res, ok := h.results["running"]; !ok || !strings.Contains(res.Error.Message, "not run twice") {
		t.Errorf("re-offered orphan: %+v, %v", res, ok)
	}
	if got := e.exec.ids(); len(got) != 0 {
		t.Errorf("started %v", got)
	}
}

// Through yad hub: a process that claimed a run and stopped before listing it
// leaves the run to the hub's queue, not lost — the next process withdraws it,
// and the hub offers it again.
func TestUnlistedClaimIsWithdrawnAtRestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	mustSync(t, l) // offered and claimed; the process dies before listing it

	l2 := &Loop{Connection: l.Connection, RunnerID: l.RunnerID, Hub: l.Hub, Store: e.store, Pool: NewPool(v1.Capacity{Total: 1}),
		Capabilities: l.Capabilities, Executor: e.exec, Clock: e.clock, Rand: l.Rand}
	if err := l2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if o, err := e.store.OutboxDepth(ctx); err != nil || o != 0 {
		t.Fatalf("outbox %d %v: an unstarted claim owes nothing", o, err)
	}
	res := mustSync(t, l2)
	if len(res.Runs) != 1 || res.Runs[0].RunID != "a" {
		t.Fatalf("offered %+v; the hub should offer the run again", res.Runs)
	}
	mustSync(t, l2)
	if got := e.exec.ids(); len(got) != 1 || got[0] != "a" {
		t.Errorf("started %v", got)
	}
}

// Through yad hub: the lost result reaches the hub at the first flush, which
// comes before the new process claims anything, and the hub marks the run
// lost at once rather than when its lease lapses.
func TestLostIsReportedBeforeClaims(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	l := e.loop(t, 1)
	e.enqueue(t, testRun("old", "s1"))
	mustSync(t, l)
	mustSync(t, l)
	if got := e.exec.ids(); len(got) != 1 {
		t.Fatalf("started %v", got)
	}
	if err := e.store.SetRunState(ctx, db.SetRunStateParams{State: "running", UpdatedAt: 1, Connection: "hub", ID: "old"}); err != nil {
		t.Fatal(err)
	}

	// A new process, with a new run waiting on the hub.
	e.enqueue(t, testRun("new", "s2"))
	l2 := &Loop{Connection: l.Connection, RunnerID: l.RunnerID, Hub: l.Hub, Store: e.store, Pool: NewPool(v1.Capacity{Total: 1}),
		Capabilities: l.Capabilities, Executor: e.exec, Clock: e.clock, Rand: l.Rand}
	rep := e.reporter(l2)
	l2.ClaimAfter = rep.Replayed()
	if err := l2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	res := mustSync(t, l2)
	if len(res.Runs) != 0 {
		t.Fatalf("offered %d runs before the replay", len(res.Runs))
	}
	if got := e.hubState(t, "old"); got != "running" {
		t.Fatalf("hub state %s before the replay", got)
	}

	rctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { rep.Run(rctx); close(done) }()
	<-rep.Replayed()
	stop()
	<-done
	if got := e.hubState(t, "old"); got != "lost" {
		t.Errorf("hub state %s after the replay, want lost", got)
	}
	if got := hubResult(t, e, "old"); got.Error == nil || got.Error.Class != ClassRunnerRestarted {
		t.Errorf("hub result %+v", got)
	}
	if res := mustSync(t, l2); len(res.Runs) != 1 || res.Runs[0].RunID != "new" {
		t.Errorf("after the replay offered %+v", res.Runs)
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
		answer := map[string]struct {
			status int
			code   string
		}{
			"/runs/later/result":    {http.StatusNotImplemented, "x"},
			"/runs/gone/result":     {http.StatusForbidden, v1.CodeNotHolder},
			"/runs/settled/result":  {http.StatusConflict, v1.CodeConflict},
			"/runs/proxied/result":  {http.StatusRequestEntityTooLarge, ""},
			"/runs/nowhere/result":  {http.StatusNotFound, v1.CodeNotFound},
			"/runs/accepted/result": {http.StatusOK, ""},
		}[r.URL.Path]
		if answer.status == http.StatusOK {
			json.NewEncoder(w).Encode(v1.Ack{OK: true})
			return
		}
		if answer.code == "" {
			// A proxy in front of the hub: no protocol envelope.
			http.Error(w, "<html>too large</html>", answer.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(answer.status)
		json.NewEncoder(w).Encode(v1.ErrorEnvelope{Error: v1.Error{Code: answer.code, Message: "x", NextAction: "x"}})
	}))
	defer srv.Close()
	c, _ := hubclient.New(srv.URL, "cred")
	e := newEnv(t)
	doc := drivableDoc("r", 1)
	l := &Loop{Connection: "hub", RunnerID: "r", Hub: c, Store: e.store, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Executor: e.exec, Clock: e.clock}
	l.init()
	for _, id := range []string{"later", "gone", "settled", "proxied", "nowhere", "accepted"} {
		l.refuse(id, "x")
	}
	mustSync(t, l)
	mustSync(t, l)
	want := map[string]int{
		"/runs/later/result": 2, "/runs/proxied/result": 2, "/runs/nowhere/result": 2,
		"/runs/gone/result": 1, "/runs/settled/result": 1, "/runs/accepted/result": 1,
	}
	for path, n := range want {
		if calls[path] != n {
			t.Errorf("%s called %d times, want %d", path, calls[path], n)
		}
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
