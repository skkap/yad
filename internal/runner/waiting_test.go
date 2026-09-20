package runner

import (
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/store/db"
)

// stepClock is a clock the test moves by hand. The resumer decides what is
// due from it, so a run parked until a reset three hours away is resumed by
// moving the clock rather than by waiting three hours — and the wait the
// result reports is the real arithmetic over real timestamps, not a stub.
type stepClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After never fires: every test here calls Sweep itself, so the loop's own
// timing is not what is under test and a firing timer would only race it.
func (c *stepClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

// byHome scripts the fake harness by which account home the turn was given,
// which is the only thing that distinguishes one turn of a failover from the
// next.
func byHome(scripts map[string]fake.Script) *fake.Adapter {
	return &fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
		return scripts[filepath.Base(s.Home)]
	}}
}

func limitScript(window string, reset time.Time, native string) fake.Script {
	return fake.Script{Outcome: adapter.Outcome{
		State:           v1.RunFailed,
		Error:           &v1.RunError{Class: adapter.ClassUsageLimit, Message: "you've hit your usage limit"},
		Limit:           &adapter.Limit{Window: window, ResetAt: reset},
		Windows:         []adapter.Window{{Name: window, UsedPercent: 100, ResetAt: reset}},
		NativeSessionID: native,
	}}
}

func waitingRow(t *testing.T, e *env, id string) db.Run {
	t.Helper()
	r := localRun(t, e, id)
	if r.State != string(v1.RunWaiting) {
		t.Fatalf("run %s is %s, want waiting", id, r.State)
	}
	return r
}

// resumer is a Resumer over the env, as Serve builds one, with a clock the
// test moves.
func (e *env) resumer(x *Exec, pool *Pool, clock Clock) *Resumer {
	r := &Resumer{Store: e.store, Pool: pool, Exec: x, Clock: clock, Log: slog.New(slog.DiscardHandler)}
	x.Ended = r.Wake
	return r
}

// eventStatuses is every status event the run spooled, in order.
func eventStatuses(t *testing.T, e *env, id string) []string {
	t.Helper()
	var out []string
	for _, ev := range spooledEvents(t, e, id) {
		if ev.Kind == v1.EventStatus {
			out = append(out, ev.Status)
		}
	}
	return out
}

// The behaviour epic E6 exists for: a usage limit mid-turn does not fail the
// run. The account is parked until its reset, the run moves to a free one,
// and it continues the same session there — the native id goes to the second
// turn, so the harness resumes the conversation rather than starting a new
// one (decision 0013, measured in DEV-24).
func TestAUsageLimitMovesTheRunToAnotherAccountInTheSameSession(t *testing.T) {
	e := newEnv(t)
	for _, label := range []string{"work", "personal"} {
		plantCredential(t, e.paths.Data, label)
	}
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	ad := byHome(map[string]fake.Script{
		"work": limitScript("five_hour", reset, "native-1"),
		"personal": {Outcome: adapter.Outcome{
			State: v1.RunSucceeded, FinalText: "done", NativeSessionID: "native-1",
			Usage: map[string]v1.Usage{"opus": {Model: "opus", Input: 10, Output: 5}},
		}},
	})
	x, _ := e.accountExecutor(t, accountConfig("work", "personal"), ad)
	claimAndRun(t, l, x)

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunSucceeded {
		t.Fatalf("result = %+v, %v — the run should have continued on the second account", res, ok)
	}
	if res.Metrics.AccountSwitches != 1 {
		t.Errorf("account_switches = %d, want 1", res.Metrics.AccountSwitches)
	}
	if len(ad.Starts) != 2 {
		t.Fatalf("the harness was started %d times, want twice: once per account", len(ad.Starts))
	}
	if got := filepath.Base(ad.Starts[0].Home); got != "work" {
		t.Errorf("the first turn ran in %q, want work", got)
	}
	if got := filepath.Base(ad.Starts[1].Home); got != "personal" {
		t.Errorf("the second turn ran in %q, want personal", got)
	}
	// The whole point of the move: the same session, continued.
	if ad.Starts[0].NativeSessionID != "" {
		t.Errorf("the first turn resumed %q; the session was new", ad.Starts[0].NativeSessionID)
	}
	if ad.Starts[1].NativeSessionID != "native-1" {
		t.Errorf("the second turn resumed %q, want native-1 — a move is a continuation, not a new session", ad.Starts[1].NativeSessionID)
	}
	if ad.Starts[1].SessionID != ad.Starts[0].SessionID {
		t.Errorf("the second turn ran in session %q, not %q", ad.Starts[1].SessionID, ad.Starts[0].SessionID)
	}
	// The account that ran out is parked, and the one that took over is not.
	if state, until := accountRow(t, e, "work"); state != v1.AccountLimited || until == nil || !until.Equal(reset) {
		t.Errorf("work is %q until %v, want limited until %s", state, until, reset)
	}
	if state, _ := accountRow(t, e, "personal"); state != "" && state != v1.AccountFree {
		t.Errorf("personal is %q, want free", state)
	}
	// Whoever is watching the run sees why it changed accounts, by label.
	var moved string
	for _, ev := range spooledEvents(t, e, "a") {
		if ev.Kind == v1.EventStatus && ev.Status == "account_switch" {
			moved = ev.Text
		}
	}
	if !strings.Contains(moved, "work") || !strings.Contains(moved, "personal") {
		t.Errorf("the move is reported as %q, want both labels in it", moved)
	}
	if strings.Contains(moved, sentinel) {
		t.Error("the move event carries something from an account home")
	}
}

// Decision 0039: the free account whose window resets soonest goes first, so
// quota about to be refreshed is spent rather than wasted. The owner's list
// names which accounts take part and breaks ties; it is not a priority order,
// which is why the account second in the list is the one that runs here.
func TestTheFreeAccountWhoseWindowResetsSoonestGoesFirst(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, label := range []string{"work", "personal"} {
		plantCredential(t, e.paths.Data, label)
	}
	far := time.Now().Add(4 * time.Hour).UTC()
	soon := time.Now().Add(20 * time.Minute).UTC()
	for _, c := range []struct {
		label string
		at    time.Time
	}{{"work", far}, {"personal", soon}} {
		if err := account.SetWindows(ctx, e.store.Queries, "claude", c.label,
			[]v1.AccountWindow{{Name: "five_hour", UsedPercent: 40, ResetsAt: &c.at}}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	ad := byHome(map[string]fake.Script{
		"work":     {Outcome: adapter.Outcome{State: v1.RunSucceeded}},
		"personal": {Outcome: adapter.Outcome{State: v1.RunSucceeded}},
	})
	x, _ := e.accountExecutor(t, accountConfig("work", "personal"), ad)
	claimAndRun(t, l, x)

	if len(ad.Starts) != 1 {
		t.Fatalf("the harness was started %d times, want once", len(ad.Starts))
	}
	if got := filepath.Base(ad.Starts[0].Home); got != "personal" {
		t.Errorf("the run took %q; personal refills in 20 minutes and work in 4 hours", got)
	}
}

// With nowhere to move to, the run waits: no process, no goroutine, a resume
// time the hub can see, and the capacity handed back so the runner's other
// work is not blocked behind it.
func TestWithNoFreeAccountTheRunWaitsAndHoldsNothing(t *testing.T) {
	e := newEnv(t)
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)

	row := waitingRow(t, e, "a")
	if !row.ResumesAt.Valid || !time.UnixMilli(row.ResumesAt.Int64).Equal(reset) {
		t.Errorf("resumes_at = %v, want %s — the account's own reset", row.ResumesAt, reset)
	}
	if !row.WaitingSince.Valid {
		t.Error("a waiting run records no start for its wait; nothing could report waited_ms or cap it")
	}
	if _, ok := outboxResult(t, e, "a"); ok {
		t.Error("a waiting run owes the hub a result; it has not ended")
	}
	if free := l.Pool.Free(); free != 1 {
		t.Errorf("free capacity = %d, want 1 — a waiting run holds no process and should hold no slot", free)
	}
	// And the hub is told, in the sync, with the moment it comes back.
	var listed []v1.HeldRun
	l.Hub = hubFunc{Hub: l.Hub, sync: func(req v1.SyncRequest) { listed = req.Runs }}
	mustSync(t, l)
	if len(listed) != 1 || listed[0].State != v1.RunWaiting {
		t.Fatalf("the sync lists %+v, want the run as waiting", listed)
	}
	if listed[0].ResumesAt == nil || !listed[0].ResumesAt.Equal(reset) {
		t.Errorf("the sync says the run resumes at %v, want %s", listed[0].ResumesAt, reset)
	}
	if got := eventStatuses(t, e, "a"); len(got) == 0 || got[len(got)-1] != "waiting" {
		t.Errorf("the run's events end %v, want a waiting status", got)
	}
}

// The acceptance criterion this task's design is shaped by: kill the runner
// where it stands and the waiting run is still there, still waiting, and
// continues once the reset has passed. Nothing in the second half of this
// test shares memory with the first — a new executor with an empty mind, as a
// new process has.
func TestAWaitingRunSurvivesTheRunnerBeingKilledAndContinuesAfterTheReset(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	waitingRow(t, e, "a")

	// kill -9: the process is gone, and with it every goroutine and the
	// parked claim it was keeping in memory. Only the store is left. The
	// second half below shares nothing with the first but that.
	x2, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "continued", NativeSessionID: "native-1"},
	}))
	restarted := &Loop{Connection: "hub", Store: e.store, Executor: x2, Log: slog.New(slog.DiscardHandler)}
	if err := restarted.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	// The restart reports every run a previous process held as lost
	// (decision 0030) — except this one, which lost nothing.
	if res, ok := outboxResult(t, e, "a"); ok {
		t.Fatalf("the restart ended the waiting run as %s; it held no process to lose", res.State)
	}
	waitingRow(t, e, "a")

	// The reset passes. The account's limit is over — an elapsed
	// limited_until reads free without a writer (internal/account.stateOf) —
	// and the clock the resumer judges from is past the resume time.
	if err := e.store.SetAccountLimit(ctx, db.SetAccountLimitParams{
		Harness: "claude", Label: "work",
		LimitedUntil: sql.NullInt64{Int64: time.Now().Add(-time.Minute).UnixMilli(), Valid: true},
		UpdatedAt:    time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	clock := &stepClock{now: reset.Add(resumeSkew + time.Second)}
	e.resumer(x2, NewPool(v1.Capacity{Total: 1}), clock).Sweep(ctx)
	x2.Wait()

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunSucceeded {
		t.Fatalf("result = %+v, %v — the run should have continued after the reset", res, ok)
	}
	if res.FinalText != "continued" {
		t.Errorf("final text = %q, want the continuation's", res.FinalText)
	}
	// The wait it served, measured over the real timestamps in the row: the
	// park was stamped by the machine's clock and the resume by the test's,
	// three hours on. duration_ms is deliberately not compared with it here
	// — the executor reads the machine's clock and the resumer the test's,
	// so the two are only commensurable in a real process.
	if want := 3 * time.Hour; res.Metrics.WaitedMS < (want - time.Minute).Milliseconds() {
		t.Errorf("waited_ms = %d, want about %s", res.Metrics.WaitedMS, want)
	}
	// The same session, continued on the other side of a restart.
	resumed, _ := x2.Adapters.Lookup("claude")
	starts := resumed.(*fake.Adapter).Starts
	if len(starts) != 1 {
		t.Fatalf("the second process started the harness %d times, want once", len(starts))
	}
	if starts[0].NativeSessionID != "native-1" {
		t.Errorf("the resumed turn continued %q, want native-1", starts[0].NativeSessionID)
	}
	// And its events continue the run's own stream rather than starting again
	// at one, which would be dropped as already spooled.
	seqs := map[int64]bool{}
	for _, ev := range spooledEvents(t, e, "a") {
		if seqs[ev.Seq] {
			t.Errorf("event sequence %d appears twice", ev.Seq)
		}
		seqs[ev.Seq] = true
	}
}

// A hub may cap how long it is willing to have a run wait. Past the cap the
// run is timed_out, with the wait it served reported.
func TestAWaitingRunPastItsMaxWaitIsTimedOut(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	run := testRun("a", "s1")
	run.MaxWaitMS = (30 * time.Minute).Milliseconds()
	l := e.loop(t, 1)
	e.enqueue(t, run)
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	waitingRow(t, e, "a")

	clock := &stepClock{now: time.Now().Add(45 * time.Minute)}
	r := e.resumer(x, l.Pool, clock)
	r.Sweep(ctx)

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunTimedOut {
		t.Fatalf("result = %+v, %v, want timed_out", res, ok)
	}
	if res.Error == nil || res.Error.Class != ClassMaxWait {
		t.Errorf("error = %+v, want the class for a wait past its cap", res.Error)
	}
	if res.Metrics.WaitedMS < run.MaxWaitMS {
		t.Errorf("waited_ms = %d, want at least the cap it passed (%d)", res.Metrics.WaitedMS, run.MaxWaitMS)
	}
	if free := l.Pool.Free(); free != 1 {
		t.Errorf("free capacity = %d, want 1", free)
	}
}

// A run's grants live only in the process that claimed them: they are never
// written to this machine's disk, so a parked run picked up by a later
// process has lost them. That is reported as a sentence the hub can act on
// rather than left to fail as a missing credential inside the harness.
func TestAWaitingRunWithGrantsIsReportedLostAfterARestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	run := testRun("a", "s1")
	run.Grants = []v1.Grant{{Name: "TOKEN", Value: "s3cret", As: v1.GrantEnv}}
	l := e.loop(t, 1)
	e.enqueue(t, run)
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	waitingRow(t, e, "a")

	// A new process: the parked claim, and the grants in it, are gone.
	x2, logged := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded},
	}))
	clock := &stepClock{now: reset.Add(resumeSkew + time.Second)}
	e.resumer(x2, l.Pool, clock).Sweep(ctx)
	x2.Wait()

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunLost {
		t.Fatalf("result = %+v, %v, want lost", res, ok)
	}
	if res.Error == nil || res.Error.Class != ClassGrantsLost {
		t.Errorf("error = %+v, want the class that says the grants did not survive", res.Error)
	}
	if strings.Contains(logged.String(), "s3cret") || strings.Contains(logged.String(), "TOKEN") {
		t.Error("the log names a grant or its value")
	}
}

// A waiting run has no turn to interrupt and no process to signal, so the
// cancel ladder has nothing to climb. The hub's cancel still ends it.
func TestCancellingAWaitingRunEndsIt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	waitingRow(t, e, "a")

	l.Resumer = e.resumer(x, l.Pool, &stepClock{now: time.Now()})
	if view, err := e.api(t).Cancel(ctx, "a"); err != nil || view.CancelRequestedAt == nil {
		t.Fatalf("cancel: %+v %v", view, err)
	}
	mustSync(t, l)

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunCancelled {
		t.Fatalf("result = %+v, %v, want cancelled", res, ok)
	}
	if got := localRun(t, e, "a").State; got != string(v1.RunCancelled) {
		t.Errorf("local state = %s, want cancelled", got)
	}
}
