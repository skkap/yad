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

// tap passes calls through to a real hub, recording them, and can answer in
// the hub's place — to lose a batch, fail a result or play a partition.
type tap struct {
	next ReportHub

	mu      sync.Mutex
	batches [][]int64 // the seqs of each events call
	results int
	// eventsErr and resultErr, when set, are returned instead of calling the
	// hub. ackAt, when set, is returned as acked_through after the call.
	eventsErr, resultErr error
	ackAt                *int64
	// eventsFor, when set, answers an events call in the hub's place.
	eventsFor func(v1.EventBatch) (v1.EventAck, error)
	// beforeEvents runs at the start of each events call.
	beforeEvents func()
}

func (t *tap) Events(ctx context.Context, runID string, b v1.EventBatch) (v1.EventAck, error) {
	t.mu.Lock()
	var seqs []int64
	for _, e := range b.Events {
		seqs = append(seqs, e.Seq)
	}
	t.batches = append(t.batches, seqs)
	err, ack, answer, before := t.eventsErr, t.ackAt, t.eventsFor, t.beforeEvents
	t.mu.Unlock()
	if before != nil {
		before()
	}
	if answer != nil {
		if a, err := answer(b); err != nil {
			return a, err
		}
	}
	if err != nil {
		return v1.EventAck{}, err
	}
	out, err := t.next.Events(ctx, runID, b)
	if ack != nil {
		out.AckedThrough = *ack
	}
	return out, err
}

func (t *tap) Result(ctx context.Context, runID string, res v1.Result) error {
	t.mu.Lock()
	t.results++
	err := t.resultErr
	t.mu.Unlock()
	if err != nil {
		return err
	}
	return t.next.Result(ctx, runID, res)
}

func status(code int) error { return &hubclient.StatusError{Status: code} }

// ranRun takes one run with n events through the executor, leaving its events
// in the spool and its result in the outbox, and returns a reporter whose hub
// calls go through a tap.
func ranRun(t *testing.T, e *env, n int) (*Loop, *Reporter, *tap) {
	t.Helper()
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	claimAndRun(t, l, e.executor(fakeHarness(fake.Script{Events: manyEvents(n), Outcome: adapter.Outcome{State: v1.RunSucceeded}})))
	tp := &tap{next: l.Hub.(*hubclient.Client)}
	r := NewReporter("hub", tp, e.store, nil)
	now := time.Now()
	r.Now = func() time.Time { return now }
	return l, r, tp
}

func spooled(t *testing.T, e *env) int {
	t.Helper()
	n, err := e.store.SpoolDepth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

func outbox(t *testing.T, e *env) []db.Outbox {
	t.Helper()
	rows, err := e.store.DueOutbox(context.Background(), db.DueOutboxParams{Connection: "hub", NextAttemptAt: 1 << 62})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// Events go up in batches of at most 100, in order, and the result follows
// only once all of them are in.
func TestEventsUploadInBatches(t *testing.T) {
	e := newEnv(t)
	_, r, tp := ranRun(t, e, 250)
	r.Flush(context.Background())
	var sizes []int
	for _, b := range tp.batches {
		sizes = append(sizes, len(b))
	}
	if !slices.Equal(sizes, []int{100, 100, 50}) || tp.batches[2][49] != 250 {
		t.Errorf("batch sizes %v", sizes)
	}
	if spooled(t, e) != 0 || len(outbox(t, e)) != 0 || e.hubState(t, "a") != "succeeded" {
		t.Errorf("after one flush: spool %d, outbox %d, hub %s", spooled(t, e), len(outbox(t, e)), e.hubState(t, "a"))
	}
}

// The hub's acked_through is authoritative: whatever it says it lacks is sent
// again, and the result waits until it has it.
func TestAckedThroughIsAuthoritative(t *testing.T) {
	e := newEnv(t)
	_, r, tp := ranRun(t, e, 5)
	two := int64(2)
	tp.ackAt = &two
	r.Flush(context.Background())
	if spooled(t, e) != 3 {
		t.Fatalf("spool depth %d after an ack through 2 of 5, want 3", spooled(t, e))
	}
	if tp.results != 0 {
		t.Error("the result went out before the hub had the events")
	}
	tp.ackAt = nil
	r.Flush(context.Background())
	if last := tp.batches[len(tp.batches)-1]; !slices.Equal(last, []int64{3, 4, 5}) {
		t.Errorf("resent %v, want [3 4 5]", last)
	}
	if spooled(t, e) != 0 || tp.results != 1 {
		t.Errorf("spool %d, results sent %d", spooled(t, e), tp.results)
	}

}

// A hub that answers an upload with an acked_through below what it had
// acknowledged before has lost events: everything after it goes again.
func TestHubThatLostEventsGetsThemAgain(t *testing.T) {
	e := newEnv(t)
	_, r, tp := ranRun(t, e, 5)
	three, one := int64(3), int64(1)
	tp.ackAt = &three
	r.Flush(context.Background())
	tp.ackAt = &one
	r.Flush(context.Background())
	if spooled(t, e) != 4 {
		t.Fatalf("spool depth %d after the hub fell back to 1, want 4", spooled(t, e))
	}
	tp.ackAt = nil
	r.Flush(context.Background())
	if last := tp.batches[len(tp.batches)-1]; !slices.Equal(last, []int64{2, 3, 4, 5}) {
		t.Errorf("resent %v, want [2 3 4 5]", last)
	}
	if spooled(t, e) != 0 || tp.results != 1 {
		t.Errorf("spool %d, results sent %d", spooled(t, e), tp.results)
	}
}

// A result the hub cannot take yet is retried with a doubling backoff capped
// at five minutes, and survives the process: a new reporter on the same store
// delivers it.
func TestResultRetriesAndReplays(t *testing.T) {
	e := newEnv(t)
	_, r, tp := ranRun(t, e, 0)
	start := r.Now()
	tp.resultErr = status(503)
	var waits []time.Duration
	for range 12 {
		r.Flush(context.Background())
		o := outbox(t, e)
		if len(o) != 1 {
			t.Fatalf("outbox has %d entries", len(o))
		}
		next := time.UnixMilli(o[0].NextAttemptAt)
		waits = append(waits, next.Sub(r.Now()).Round(time.Second))
		r.Now = func() time.Time { return next }
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300, 300}
	for i := range want {
		want[i] *= time.Second
	}
	if !slices.Equal(waits, want) {
		t.Errorf("retry schedule %v, want %v", waits, want)
	}
	if o := outbox(t, e); o[0].Attempts != 12 || !o[0].LastError.Valid {
		t.Errorf("outbox entry %+v", o[0])
	}
	// Not due yet: nothing is sent.
	sent := tp.results
	r.Now = func() time.Time { return start }
	r.Flush(context.Background())
	if tp.results != sent {
		t.Error("a result was sent before it was due")
	}

	// The next process replays it.
	fresh := NewReporter("hub", tp.next, e.store, nil)
	fresh.Now = func() time.Time { return start.Add(time.Hour) }
	fresh.Flush(context.Background())
	if len(outbox(t, e)) != 0 || e.hubState(t, "a") != "succeeded" {
		t.Errorf("after replay: outbox %d, hub %s", len(outbox(t, e)), e.hubState(t, "a"))
	}
}

// Which hub answers settle a result and which leave it owed.
func TestResultAnswers(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		kept bool
	}{
		{"conflict: the hub's state stands", &hubclient.StatusError{Status: 409, Protocol: &v1.Error{Code: v1.CodeConflict}}, false},
		{"not the holder", &hubclient.StatusError{Status: 403, Protocol: &v1.Error{Code: v1.CodeNotHolder}}, false},
		{"invalid", &hubclient.StatusError{Status: 422, Protocol: &v1.Error{Code: v1.CodeInvalid}}, false},
		{"not found, which may be a wrong URL", &hubclient.StatusError{Status: 404, Protocol: &v1.Error{Code: v1.CodeNotFound}}, true},
		{"a proxy's 409", status(409), true},
		{"a proxy's 403", status(403), true},
		{"a proxy's 404", status(404), true},
		{"a proxy's body limit", status(413), true},
		{"credential refused, may come back", status(401), true},
		{"too many requests", status(429), true},
		{"hub error", status(500), true},
		{"a proxy with no protocol body", status(502), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			_, r, tp := ranRun(t, e, 0)
			tp.resultErr = tc.err
			r.Flush(context.Background())
			if kept := len(outbox(t, e)) == 1; kept != tc.kept {
				t.Errorf("kept in the outbox = %v, want %v", kept, tc.kept)
			}
		})
	}
}

// Events the hub will never take are dropped from the upload, so the result
// behind them is not held back forever; a transient failure holds it.
func TestEventRefusalReleasesTheResult(t *testing.T) {
	e := newEnv(t)
	_, r, tp := ranRun(t, e, 3)
	tp.eventsErr = status(500)
	r.Flush(context.Background())
	if tp.results != 0 || spooled(t, e) != 3 {
		t.Fatalf("after a 500: results %d, spool %d", tp.results, spooled(t, e))
	}
	tp.eventsErr = &hubclient.StatusError{Status: 403, Protocol: &v1.Error{Code: v1.CodeNotHolder}}
	r.Flush(context.Background())
	if tp.results != 1 || spooled(t, e) != 0 {
		t.Errorf("after a 403: results %d, spool %d", tp.results, spooled(t, e))
	}
}

// A finished run keeps its lease while its result is owed: a hub outage longer
// than a lease must not turn a finished run into a lost one.
func TestLeaseOutlivesAnOutage(t *testing.T) {
	e := newEnv(t)
	l, r, tp := ranRun(t, e, 0)
	tp.resultErr = status(503)
	r.Flush(context.Background())

	var listed []v1.HeldRun
	l.Hub = hubFunc{Hub: l.Hub, sync: func(req v1.SyncRequest) { listed = req.Runs }}
	mustSync(t, l)
	if len(listed) != 1 || listed[0].RunID != "a" || listed[0].State != v1.RunRunning {
		t.Fatalf("listed %+v, want a as running", listed)
	}
	// The claim's lease ran from the hub's time zero; listings at 45 s renew
	// it, so at 90 s — past the first lease — the run is still not lost.
	e.skew.Store(int64(45 * time.Second))
	mustSync(t, l)
	e.skew.Store(int64(90 * time.Second))
	if err := e.hub.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := e.hubState(t, "a"); st != "running" {
		t.Fatalf("hub state after the outage = %s", st)
	}
	tp.resultErr = nil
	r.Now = func() time.Time { return time.Now().Add(time.Hour) }
	r.Flush(context.Background())
	if e.hubState(t, "a") != "succeeded" || len(outbox(t, e)) != 0 {
		t.Errorf("hub %s, outbox %d", e.hubState(t, "a"), len(outbox(t, e)))
	}
	listed = nil
	mustSync(t, l)
	if len(listed) != 0 {
		t.Errorf("a delivered run is still listed: %+v", listed)
	}
}

// hubFunc sees each sync request before passing it on.
type hubFunc struct {
	Hub
	sync func(v1.SyncRequest)
}

func (h hubFunc) Sync(ctx context.Context, runnerID string, req v1.SyncRequest) (v1.SyncResponse, error) {
	h.sync(req)
	return h.Hub.Sync(ctx, runnerID, req)
}

// A result the hub already settled differently — lost, after the lease lapsed
// during a partition — leaves the outbox; the local record keeps what the
// runner saw.
func TestLateResultAfterLostStopsReporting(t *testing.T) {
	e := newEnv(t)
	_, r, _ := ranRun(t, e, 0)
	if _, err := e.hubStore.DB.Exec(`UPDATE runs SET state = 'lost', lease_expires_at = NULL WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	r.Flush(context.Background())
	if e.hubState(t, "a") != "lost" || len(outbox(t, e)) != 0 {
		t.Errorf("hub %s, outbox %d", e.hubState(t, "a"), len(outbox(t, e)))
	}
	if got := localRun(t, e, "a"); got.State != "succeeded" {
		t.Errorf("local state = %s", got.State)
	}
}

// A batch a proxy refuses as too large is sent again in halves rather than
// dropped: a proxy's limit is not the hub's verdict.
func TestTooLargeBatchesAreHalved(t *testing.T) {
	e := newEnv(t)
	_, r, tp := ranRun(t, e, 100)
	tp.eventsFor = func(b v1.EventBatch) (v1.EventAck, error) {
		if len(b.Events) > 30 {
			return v1.EventAck{}, status(413)
		}
		return v1.EventAck{}, nil
	}
	r.Flush(context.Background())
	var sizes []int
	for _, b := range tp.batches {
		sizes = append(sizes, len(b))
	}
	if !slices.Equal(sizes, []int{100, 50, 25, 25, 25, 25}) {
		t.Errorf("batch sizes %v", sizes)
	}
	if spooled(t, e) != 0 || tp.results != 1 {
		t.Errorf("spool %d, results %d", spooled(t, e), tp.results)
	}
}

// A result written while a flush is under way — its run's last events spooled
// after the flush looked at the spool — waits for those events.
func TestResultNeverOvertakesItsEvents(t *testing.T) {
	e := newEnv(t)
	_, r, tp := ranRun(t, e, 1)
	ctx := context.Background()
	var once sync.Once
	tp.beforeEvents = func() {
		once.Do(func() {
			// What the executor does for a second run mid-flush: its last
			// event, then its result.
			for _, err := range []error{
				e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "s2", Harness: "claude"}),
				e.store.CreateRun(ctx, db.CreateRunParams{Connection: "hub", ID: "b", SessionID: "s2", Harness: "claude", Spec: "{}"}),
				e.store.AppendEvent(ctx, db.AppendEventParams{Connection: "hub", RunID: "b", Seq: 1, Body: `{"seq":1,"kind":"text"}`}),
				e.store.PutOutbox(ctx, db.PutOutboxParams{Connection: "hub", RunID: "b", Body: `{"state":"succeeded"}`}),
			} {
				if err != nil {
					t.Error(err)
				}
			}
		})
	}
	var sent []string
	tp.next = resultSpy{ReportHub: tp.next, sent: &sent}
	r.Flush(ctx)
	if slices.Contains(sent, "b") {
		t.Error("b's result went out while its event was still in the spool")
	}
	if !slices.Contains(sent, "a") {
		t.Errorf("results sent %v, want a's", sent)
	}
}

type resultSpy struct {
	ReportHub
	sent *[]string
}

func (s resultSpy) Result(ctx context.Context, runID string, res v1.Result) error {
	*s.sent = append(*s.sent, runID)
	if runID == "b" {
		return nil
	}
	return s.ReportHub.Result(ctx, runID, res)
}
