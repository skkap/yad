package runner

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store"
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

// restartedLoop is a second Loop for the same runner and the same hub
// credential: the next process, which has not synced yet.
func (e *env) restartedLoop(t *testing.T, capacity int) *Loop {
	t.Helper()
	id, err := e.paths.RunnerID()
	if err != nil {
		t.Fatal(err)
	}
	c, err := hubclient.New(e.url, e.cred)
	if err != nil {
		t.Fatal(err)
	}
	doc := drivableDoc(id, capacity)
	return &Loop{
		Connection: "hub", RunnerID: id, Hub: c, Store: e.store, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Clock: e.clock,
		Rand: func() float64 { return 0.5 }, Log: slog.New(slog.DiscardHandler),
	}
}

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

// syncAt is how a parked run comes back: its connection's own sync, with
// the loop's clock at the given moment. It returns once the runs that sync
// started have finished.
func syncAt(t *testing.T, l *Loop, x *Exec, now time.Time) {
	t.Helper()
	l.Executor = x
	l.Clock = &stepClock{now: now}
	l.Accounts = x.Accounts
	mustSync(t, l)
	x.Wait()
}

// collectorFor is a Collector over the env, as Serve builds one: the only
// thing in the process that ends a parked run without a sync, and it never
// starts one.
func collectorFor(e *env, x *Exec) *Collector {
	return &Collector{Store: e.store, Workdirs: filepath.Join(e.paths.Data, "workdirs"),
		Runs: x, Clock: realClock{}, Log: slog.New(slog.DiscardHandler)}
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
	restarted := e.restartedLoop(t, 1)
	restarted.Executor = x2
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
	syncAt(t, restarted, x2, reset.Add(resumeSkew+time.Second))

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
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	run := testRun("a", "s1")
	run.MaxWaitMS = (30 * time.Minute).Milliseconds()
	l := e.loop(t, 1)
	e.enqueue(t, run)
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	waitingRow(t, e, "a")

	syncAt(t, l, x, time.Now().Add(45*time.Minute))

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
	syncAt(t, e.restartedLoop(t, 1), x2, reset.Add(resumeSkew+time.Second))

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

// ARCHITECTURE's run-state diagram says the way out of waiting is "limit
// resets / account frees". The second half is the one a fixed resume time
// cannot express: the run parks on the earliest reset known at the time, and
// the owner then finishes a login in an already-configured account's home,
// which LoginProbe notices. Otherwise the run sits out four more hours beside
// a working account, and one with a max_wait times out while that account
// runs newer work.
//
// A login of a configured account, not `yad account add` of a new one: a
// running daemon enumerates the labels in the configuration it started with,
// so a new label reaches it only at a restart. That is why spare is in cfg
// from the start here and only its login arrives late.
func TestAFreedAccountBringsAParkedRunBackBeforeItsResumeTime(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	plantCredential(t, e.paths.Data, "spare")
	// spare needs a login, so it is no use when the run parks.
	if err := account.SetState(ctx, e.store.Queries, "claude", "spare", v1.AccountNeedsLogin, time.Now()); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Second)

	cfg := accountConfig("work", "spare")
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	ad := byHome(map[string]fake.Script{
		"work":  limitScript("five_hour", reset, "native-1"),
		"spare": {Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "on the spare", NativeSessionID: "native-1"}},
	})
	x, _ := e.accountExecutor(t, cfg, ad)
	claimAndRun(t, l, x)
	row := waitingRow(t, e, "a")
	if !time.UnixMilli(row.ResumesAt.Int64).Equal(reset) {
		t.Fatalf("resumes_at = %s, want the five-hour reset %s", time.UnixMilli(row.ResumesAt.Int64), reset)
	}

	// The clock stays well before the reset. Only the account becoming
	// usable can move the run.
	early := time.Now().Add(10 * time.Minute)
	syncAt(t, l, x, early)
	waitingRow(t, e, "a")

	// The owner logs spare in by hand and the probe frees it. Nothing tells
	// the sync loop; its next sync reads account state anyway.
	if freed := probeFor(t, e, cfg).Sweep(ctx); freed != 1 {
		t.Fatalf("the probe freed %d accounts, want 1", freed)
	}

	syncAt(t, l, x, early)
	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunSucceeded {
		t.Fatalf("result = %+v, %v — a free account should have brought the run back", res, ok)
	}
	if res.FinalText != "on the spare" {
		t.Errorf("final text = %q, want the turn on the account that came back", res.FinalText)
	}
	if res.Metrics.AccountSwitches != 1 {
		t.Errorf("account_switches = %d, want 1 — the run came back on another account", res.Metrics.AccountSwitches)
	}
}

// A run is one run however many accounts and however many processes it took.
// protocol/v1.Result says its usage covers the whole of it, so a result built
// from the last turn alone would tell a hub that a run which spent an
// account's entire window used only what the turn after it did.
func TestAMovedRunReportsWhatEveryTurnCost(t *testing.T) {
	e := newEnv(t)
	for _, label := range []string{"work", "personal"} {
		plantCredential(t, e.paths.Data, label)
	}
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	first := limitScript("five_hour", reset, "native-1")
	first.Events = []v1.Event{{Kind: v1.EventToolCall, Tool: &v1.ToolEvent{ID: "t1", Name: "Bash", Input: "ls"}}}
	first.Outcome.Usage = map[string]v1.Usage{"opus": {Model: "opus", Input: 100, Output: 50, CacheRead: 9}}
	first.Outcome.APIRetries = 2

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work", "personal"), byHome(map[string]fake.Script{
		"work": first,
		"personal": {
			Events: []v1.Event{{Kind: v1.EventToolCall, Tool: &v1.ToolEvent{ID: "t2", Name: "Bash", Input: "pwd"}}},
			Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "done", NativeSessionID: "native-1",
				APIRetries: 1, Usage: map[string]v1.Usage{"opus": {Model: "opus", Input: 7, Output: 3}}},
		},
	}))
	claimAndRun(t, l, x)

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunSucceeded {
		t.Fatalf("result = %+v, %v", res, ok)
	}
	got := res.Usage.ByModel["opus"]
	if want := (v1.Usage{Model: "opus", Input: 107, Output: 53, CacheRead: 9}); got != want {
		t.Errorf("usage = %+v, want %+v — both turns", got, want)
	}
	if res.Metrics.ToolCalls != 2 {
		t.Errorf("tool_calls = %d, want 2 — one per turn", res.Metrics.ToolCalls)
	}
	if res.Metrics.APIRetries != 3 {
		t.Errorf("api_retries = %d, want 3 — 2 on the first account and 1 on the second", res.Metrics.APIRetries)
	}
}

// The same, across a park and a restart: the pre-park turn's cost is in the
// run's row, so a process that never saw it still reports it.
func TestAResumedRunReportsWhatTheTurnBeforeTheParkCost(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	first := limitScript("five_hour", reset, "native-1")
	first.Events = []v1.Event{{Kind: v1.EventToolCall, Tool: &v1.ToolEvent{ID: "t1", Name: "Bash", Input: "ls"}}}
	first.Outcome.Usage = map[string]v1.Usage{"opus": {Model: "opus", Input: 100, Output: 50}}

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(first))
	claimAndRun(t, l, x)
	waitingRow(t, e, "a")

	// A new process, and the reset has passed.
	if err := e.store.SetAccountLimit(ctx, db.SetAccountLimitParams{
		Harness: "claude", Label: "work",
		LimitedUntil: sql.NullInt64{Int64: time.Now().Add(-time.Minute).UnixMilli(), Valid: true},
		UpdatedAt:    time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	x2, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1",
			Usage: map[string]v1.Usage{"opus": {Model: "opus", Input: 7, Output: 3}}},
	}))
	syncAt(t, e.restartedLoop(t, 1), x2, reset.Add(resumeSkew+time.Second))

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunSucceeded {
		t.Fatalf("result = %+v, %v", res, ok)
	}
	got := res.Usage.ByModel["opus"]
	if want := (v1.Usage{Model: "opus", Input: 107, Output: 53}); got != want {
		t.Errorf("usage = %+v, want %+v — the turn before the park counts too", got, want)
	}
	if res.Metrics.ToolCalls != 1 {
		t.Errorf("tool_calls = %d, want 1 — from the turn before the park", res.Metrics.ToolCalls)
	}
}

// The hub's wall-clock cap is a cap on the run, not a budget handed out
// afresh to each account it tries (DOMAIN.md, "Watchdog"). Two turns that
// each fit inside the cap must not add up to twice it.
//
// The second turn here would finish comfortably inside a fresh cap and does
// not fit in what the first left, so the run's state is what tells the two
// readings apart.
func TestTheWallClockCapIsSharedBetweenAccountsRatherThanRenewed(t *testing.T) {
	e := newEnv(t)
	for _, label := range []string{"work", "personal"} {
		plantCredential(t, e.paths.Data, label)
	}
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	first := limitScript("five_hour", reset, "native-1")
	// One event, 500ms in: the first turn spends most of the cap and then
	// ends on the limit rather than on the watchdog.
	first.Events = []v1.Event{{Kind: v1.EventText, Text: "working"}}
	first.Delay = 500 * time.Millisecond

	run := testRun("a", "s1")
	run.WallClockMS = 600
	l := e.loop(t, 1)
	e.enqueue(t, run)
	x, _ := e.accountExecutor(t, accountConfig("work", "personal"), byHome(map[string]fake.Script{
		"work": first,
		"personal": {
			Events:  []v1.Event{{Kind: v1.EventText, Text: "still working"}},
			Delay:   300 * time.Millisecond,
			Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "done", NativeSessionID: "native-1"},
		},
	}))
	claimAndRun(t, l, x)

	res, ok := outboxResult(t, e, "a")
	if !ok {
		t.Fatal("no result")
	}
	if res.State != v1.RunTimedOut {
		t.Fatalf("state = %s, want timed_out — the second turn had ~100ms of the 600ms cap left, not another 600ms", res.State)
	}
	if res.Error == nil || res.Error.Class != ClassWallClock {
		t.Errorf("error = %+v, want the wall-clock class", res.Error)
	}
}

// Nothing in the process starts a parked run except its own connection's
// sync. That is not a check anywhere — it is that the only code which starts
// one runs inside SyncOnce — so this pins the property rather than a guard:
// a connection with no loop has no sync, and the collector, which does sweep
// every connection, only ever ends a run.
func TestNothingButASyncStartsAParkedRun(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	waitingRow(t, e, "a")

	// Everything a resume needs is true: the reset has passed, the account
	// is free, capacity is back. Only the sync is missing.
	if err := e.store.SetAccountLimit(ctx, db.SetAccountLimitParams{
		Harness: "claude", Label: "work",
		LimitedUntil: sql.NullInt64{Int64: time.Now().Add(-time.Minute).UnixMilli(), Valid: true},
		UpdatedAt:    time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	x2, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded},
	}))
	// The one thing in the process that reaches every connection's parked
	// runs without a sync. It has no cap to expire here, so it does nothing
	// — and it could do nothing else, because it cannot start a run.
	if err := collectorFor(e, x2).Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	x2.Wait()

	waitingRow(t, e, "a")
	if _, ok := outboxResult(t, e, "a"); ok {
		t.Error("a parked run ended without its connection's sync")
	}
	if got, _ := x2.Adapters.Lookup("claude"); len(got.(*fake.Adapter).Starts) != 0 {
		t.Error("a turn was started without a sync")
	}
}

// A drain leaves a parked run exactly as it is, for the next process: a
// runner on its way down takes on nothing new, and a run that holds no
// process is the easiest thing in the world to leave. It keeps syncing
// meanwhile, so the gate has to be in the sync rather than around it.
func TestADrainLeavesAParkedRunForTheNextProcess(t *testing.T) {
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
	if err := e.store.SetAccountLimit(ctx, db.SetAccountLimitParams{
		Harness: "claude", Label: "work",
		LimitedUntil: sql.NullInt64{Int64: time.Now().Add(-time.Minute).UnixMilli(), Valid: true},
		UpdatedAt:    time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}

	l.Drain = NewDrain()
	l.Drain.Begin("test")
	syncAt(t, l, x, reset.Add(resumeSkew+time.Second))

	waitingRow(t, e, "a")
	if res, ok := outboxResult(t, e, "a"); ok {
		t.Fatalf("a draining runner ended the parked run as %s; it should stay parked for the next process", res.State)
	}
	// And the claim it was keeping is still here, so the next sync after a
	// drain that was called off can still start it with its grants.
	if _, ok := x.Parked("hub", "a"); !ok {
		t.Error("the drain gave up the parked claim; a resume would report grants_lost")
	}
}

// EndRunWait clears resumes_at, and a waiting row with no resumes_at reads as
// due right now. So ending a run's wait and writing what it became have to be
// one transaction: apart, a failure of the second leaves a row still
// `waiting` and permanently due, and the next sweep runs a turn for a run
// that was cancelled, or timed out, or is already running.
//
// This is the mechanism the two call sites rest on — that a failure after
// EndRunWait inside a transaction takes EndRunWait with it.
func TestEndingAWaitRollsBackWithTheWriteBesideIt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	before := waitingRow(t, e, "a")

	failed := errors.New("the write beside it failed")
	err := e.store.Tx(ctx, func(q *db.Queries) error {
		if err := q.EndRunWait(ctx, db.EndRunWaitParams{Now: time.Now().UnixMilli(), Connection: "hub", ID: "a"}); err != nil {
			return err
		}
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("Tx = %v, want the injected failure", err)
	}

	after := waitingRow(t, e, "a")
	if after.ResumesAt != before.ResumesAt {
		t.Errorf("resumes_at = %v, want %v — a rolled-back wait must not leave the run due", after.ResumesAt, before.ResumesAt)
	}
	if after.WaitingSince != before.WaitingSince || after.WaitedMs != before.WaitedMs {
		t.Errorf("the wait moved: since %v→%v, waited %d→%d",
			before.WaitingSince, after.WaitingSince, before.WaitedMs, after.WaitedMs)
	}
}

// And the backstop that the call sites use it that way. Matching the source
// is not a proof — a call reached some other way is beyond what reading it
// can see — but it is the write someone would actually add: a bare
// Store.EndRunWait beside a best-effort state write reads perfectly
// reasonable and reintroduces exactly the defect above.
func TestNoShippedFileEndsARunsWaitOutsideATransaction(t *testing.T) {
	// q is the transaction's own Queries, which is the only receiver allowed;
	// anything else is a handle that commits on its own.
	loose := regexp.MustCompile(`(?:[A-Za-z_][A-Za-z0-9_.]*)\.EndRunWait\(`)
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && (d.Name() == ".git" || d.Name() == "testdata"):
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range loose.FindAllIndex(raw, -1) {
			if strings.HasPrefix(string(raw[m[0]:m[1]]), "q.") {
				continue
			}
			line := 1 + strings.Count(string(raw[:m[0]]), "\n")
			t.Errorf("%s:%d calls %s outside a transaction; ending a wait clears resumes_at, so it must commit with whatever the run became",
				path, line, string(raw[m[0]:m[1]]))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// What a run spent is true whatever the run then became. A cancel arriving
// during the turn after a move does not undo the tokens the first account
// burned, and the result the hub is finally told has to carry them — it is
// the only report of what the run cost.
//
// The asymmetry this pins: a run cancelled a moment *before* its turn starts
// already reported the earlier turns' usage, through the cancelled-early
// path. One cancelled a moment after must not report less.
func TestACancelledRunStillReportsWhatTheAccountBeforeItSpent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, label := range []string{"work", "personal"} {
		plantCredential(t, e.paths.Data, label)
	}
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	first := limitScript("five_hour", reset, "native-1")
	first.Outcome.Usage = map[string]v1.Usage{"opus": {Model: "opus", Input: 100, Output: 50}}

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	ad := byHome(map[string]fake.Script{
		"work": first,
		// The turn the cancel lands in: it never answers on its own.
		"personal": {Hang: true, Outcome: adapter.Outcome{NativeSessionID: "native-1"}},
	})
	x, _ := e.accountExecutor(t, accountConfig("work", "personal"), ad)
	started(t, l, x)
	// The second turn itself, not the account the run moved to: the account is
	// recorded before that turn is started, and a cancel landing in between
	// ends the run before the turn this test is about (DEV-92).
	eventually(t, "the second account's turn has started", func() bool {
		return len(ad.Turns()) == 2
	})
	if view, err := e.api(t).Cancel(ctx, "a"); err != nil || view.CancelRequestedAt == nil {
		t.Fatalf("cancel: %+v %v", view, err)
	}
	mustSync(t, l)
	ended(t, x)

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunCancelled {
		t.Fatalf("result = %+v, %v, want cancelled", res, ok)
	}
	got := res.Usage.ByModel["opus"]
	if want := (v1.Usage{Model: "opus", Input: 100, Output: 50}); got != want {
		t.Errorf("usage = %+v, want %+v — the cancel does not undo what the first account spent", got, want)
	}
}

// first_event_ms answers "why is the harness slow" (ARCHITECTURE.md §2). A
// run parked for hours and then answering in milliseconds is not a slow
// harness, and reporting the park here would count the same wait twice —
// once as waited_ms and once as time to first token.
func TestFirstEventMSMeasuresTheTurnAndNotTheWaitBeforeIt(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	waitingRow(t, e, "a")

	if err := e.store.SetAccountLimit(ctx, db.SetAccountLimitParams{
		Harness: "claude", Label: "work",
		LimitedUntil: sql.NullInt64{Int64: time.Now().Add(-time.Minute).UnixMilli(), Valid: true},
		UpdatedAt:    time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	// The park is milliseconds old in wall-clock terms, and the three hours
	// live only in the resumer's clock — so the row is backdated to make the
	// wait real. Without this the two readings of first_event_ms, from the
	// run's start and from the turn's, are the same number.
	long := time.Now().Add(-3 * time.Hour).UnixMilli()
	if _, err := e.store.DB.ExecContext(ctx,
		`UPDATE runs SET started_at = ?, waiting_since = ? WHERE connection = 'hub' AND id = 'a'`, long, long); err != nil {
		t.Fatal(err)
	}
	x2, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Events:  []v1.Event{{Kind: v1.EventText, Text: "here at once"}},
		Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"},
	}))
	syncAt(t, e.restartedLoop(t, 1), x2, time.Now())

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunSucceeded {
		t.Fatalf("result = %+v, %v", res, ok)
	}
	if res.Metrics.WaitedMS < (3*time.Hour).Milliseconds()-time.Minute.Milliseconds() {
		t.Fatalf("waited_ms = %d, want about three hours — the rest of this test rests on it", res.Metrics.WaitedMS)
	}
	if res.Metrics.FirstEventMS > time.Minute.Milliseconds() {
		t.Errorf("first_event_ms = %d; the harness answered at once and only the park was long",
			res.Metrics.FirstEventMS)
	}
}

// resumeSkew exists because the reset came from the harness, whose clock is
// its own: a turn started a moment early gets the same limit back and costs a
// whole cache-cold turn. The early-resume path must not walk around it.
//
// The window is not hypothetical. resumes_at is the earliest limited_until
// among the harness's accounts, and account.stateOf reads that same account
// free the instant the moment passes — with no skew — so for resumeSkew after
// a reset, "an account is free" and "the run is not due" are the same reset
// seen through two clocks.
func TestTheEarlyResumeDoesNotWalkAroundTheSkewOnTheRunsOwnReset(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	waitingRow(t, e, "a")

	// The reset is one second ago: the account reads free, and the run is
	// due in four more seconds. Both are the same moment.
	just := time.Now().Add(-time.Second)
	if err := e.store.SetAccountLimit(ctx, db.SetAccountLimitParams{
		Harness: "claude", Label: "work",
		LimitedUntil: sql.NullInt64{Int64: just.UnixMilli(), Valid: true}, UpdatedAt: time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetRunWaiting(ctx, db.SetRunWaitingParams{
		ResumesAt:    sql.NullInt64{Int64: just.UnixMilli(), Valid: true},
		WaitingSince: sql.NullInt64{Int64: time.Now().Add(-time.Hour).UnixMilli(), Valid: true},
		UpdatedAt:    time.Now().UnixMilli(), Connection: "hub", ID: "a",
	}); err != nil {
		t.Fatal(err)
	}

	x2, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"},
	}))
	syncAt(t, e.restartedLoop(t, 1), x2, time.Now())

	waitingRow(t, e, "a")
	if _, ok := outboxResult(t, e, "a"); ok {
		t.Error("the run was resumed inside the skew; the harness's own clock has not reached the reset")
	}
	if got, _ := x2.Adapters.Lookup("claude"); len(got.(*fake.Adapter).Starts) != 0 {
		t.Error("a turn was started inside the skew")
	}
}

// A run this process cannot sync for is still bounded by the cap its hub
// gave it. Nothing will ever resume it, so if nothing ends it either, it
// holds its session and workdir out of collection for ever and the hub
// never hears the timed_out it asked for. That is the collector's job, and
// the only thing it does to a parked run.
func TestTheCollectorEndsAParkedRunPastItsCap(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Second)

	run := testRun("a", "s1")
	run.MaxWaitMS = (30 * time.Minute).Milliseconds()
	l := e.loop(t, 1)
	e.enqueue(t, run)
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	before := waitingRow(t, e, "a")

	c := collectorFor(e, x)
	// Not yet: the cap has not run out, and the collector leaves it alone.
	c.Clock = &stepClock{now: time.Now().Add(10 * time.Minute)}
	if err := c.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if after := waitingRow(t, e, "a"); after.ResumesAt != before.ResumesAt {
		t.Fatalf("the collector moved a parked run whose cap is still running")
	}

	c.Clock = &stepClock{now: time.Now().Add(45 * time.Minute)}
	if err := c.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunTimedOut {
		t.Fatalf("result = %+v, %v, want timed_out", res, ok)
	}
	if res.Error == nil || res.Error.Class != ClassMaxWait {
		t.Errorf("error = %+v, want the class for a wait past its cap", res.Error)
	}
	if got := localRun(t, e, "a").State; got == string(v1.RunWaiting) {
		t.Error("the run is still waiting; its session and workdir can never be collected")
	}
}

// The claim, and the grants in it, are given up only once the terminal
// result has actually landed. The transaction leaves the row exactly as it
// was when it fails, so a claim forgotten first leaves a run still `waiting`
// whose grants this process no longer holds — and the next sync reports it
// `lost` with class grants_lost, which decision 0023 makes final, so the
// state the hub asked for could never be sent.
//
// The failure is injected by giving the executor a store handle that is
// closed: the loop's own reads still work, and only the write fails.
func TestAFailedResultKeepsTheParkedClaimSoTheCancelCanBeTriedAgain(t *testing.T) {
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
	before := waitingRow(t, e, "a")

	broken, err := store.Open(ctx, e.paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	broken.Close()
	x.Store = broken
	if l.cancelWaiting(ctx, "a") {
		t.Error("the cancel reported itself recorded; its result was never written")
	}
	x.Store = e.store

	after := waitingRow(t, e, "a")
	if after.ResumesAt != before.ResumesAt || after.WaitingSince != before.WaitingSince {
		t.Errorf("the row moved: resumes_at %v→%v, waiting_since %v→%v",
			before.ResumesAt, after.ResumesAt, before.WaitingSince, after.WaitingSince)
	}
	// And the claim is still here, so the retry can end the run properly
	// instead of finding it grantless.
	if _, ok := x.Parked("hub", "a"); !ok {
		t.Error("the parked claim was given up for a result that did not land; the retry would report grants_lost")
	}
}

// A cancel this process could not write down is still a cancel. The
// transaction leaves the row exactly as it was, which is what keeps the
// run's grants and its wait — and makes the row indistinguishable from one
// nobody cancelled. Without somewhere to remember the intent, the next sync
// starts a turn for a run the hub asked to stop.
func TestACancelThatCouldNotBeWrittenStillStopsTheRunFromResuming(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	waitingRow(t, e, "a")

	broken, err := store.Open(ctx, e.paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	broken.Close()
	x.Store = broken
	if l.cancelWaiting(ctx, "a") {
		t.Fatal("the cancel reported itself recorded on a store that cannot be written")
	}
	x.Store = e.store

	// The reset has passed and the account is free, so nothing but the
	// remembered cancel stands between this run and a turn.
	if err := e.store.SetAccountLimit(ctx, db.SetAccountLimitParams{
		Harness: "claude", Label: "work",
		LimitedUntil: sql.NullInt64{Int64: time.Now().Add(-time.Minute).UnixMilli(), Valid: true},
		UpdatedAt:    time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	syncAt(t, l, x, reset.Add(resumeSkew+time.Second))

	if got, _ := x.Adapters.Lookup("claude"); len(got.(*fake.Adapter).Starts) != 1 {
		t.Errorf("the harness was started %d times, want the one turn before the cancel", len(got.(*fake.Adapter).Starts))
	}
	// And the sync that could write finished the cancel rather than leaving
	// it to the hub to ask again.
	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunCancelled {
		t.Fatalf("result = %+v, %v, want the cancel recorded once the store worked again", res, ok)
	}
}

// The property the redesign exists for, pinned as a property rather than as
// a behaviour: only the sync loop starts a run. Four rounds found four
// clauses of "may I start work now?" missing from a second scheduler, so the
// answer was to have one place that starts work — and a backstop against
// somebody adding a second is worth more than another clause.
//
// Matching the source is not a proof; a call reached some other way is
// beyond what reading it can see. It is the write someone would actually
// add: a sweep that resumes parked runs on a ticker reads perfectly
// reasonable and is exactly what this branch removed.
func TestOnlyTheSyncLoopStartsARun(t *testing.T) {
	// A run begins in exactly one way: Executor.Start, handed a Claim.
	// Both spellings of that call are matched — through the interface, and
	// through a concrete executor a second scheduler would hold.
	starts := regexp.MustCompile(`Executor\.Start\(|\.Start\([A-Za-z_.]+, Claim\{`)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range entries {
		name := d.Name()
		switch {
		case d.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go"):
			continue
		// sync.go holds the claim, sync_waiting.go the resume, and
		// runner.go the wrapper that puts a started run on the server's
		// context. Every other file in the package must not start a run.
		case name == "sync.go" || name == "sync_waiting.go" || name == "runner.go":
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if loc := starts.FindIndex(raw); loc != nil {
			line := 1 + strings.Count(string(raw[:loc[0]]), "\n")
			t.Errorf("%s:%d starts a run outside the sync loop; every clause of \"may I start work now?\" lives there", name, line)
		}
	}
}

// The worst outcome anyone found on this branch: a run the hub has
// cancelled running a turn anyway. The listing a sync resumes from is made
// before the hub is asked, and the hub's answer is acted on in between — so
// a cancel in that answer ends the run while the snapshot still says it is
// waiting. Acting on the snapshot wrote `claimed` over the cancelled row
// and started the harness, and the outbox's ON CONFLICT DO NOTHING
// swallowed the second result, so the hub never even saw the contradiction.
func TestACancelInThisSyncsAnswerStopsTheResumeInTheSameSync(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	waitingRow(t, e, "a")

	// Everything a resume needs is true: the limit has lifted and the run
	// is due. Only the cancel should stop it, and it arrives in the answer
	// to the very sync that would otherwise start it.
	if err := e.store.SetAccountLimit(ctx, db.SetAccountLimitParams{
		Harness: "claude", Label: "work",
		LimitedUntil: sql.NullInt64{Int64: time.Now().Add(-time.Minute).UnixMilli(), Valid: true},
		UpdatedAt:    time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	if view, err := e.api(t).Cancel(ctx, "a"); err != nil || view.CancelRequestedAt == nil {
		t.Fatalf("cancel: %+v %v", view, err)
	}
	syncAt(t, l, x, reset.Add(resumeSkew+time.Second))

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunCancelled {
		t.Fatalf("result = %+v, %v, want cancelled", res, ok)
	}
	if got, _ := x.Adapters.Lookup("claude"); len(got.(*fake.Adapter).Starts) != 1 {
		t.Errorf("the harness was started %d times, want the one turn before the cancel — the run the hub stopped ran anyway",
			len(got.(*fake.Adapter).Starts))
	}
	if got := localRun(t, e, "a").State; got != string(v1.RunCancelled) {
		t.Errorf("local state = %s, want cancelled", got)
	}
}

// A parked run whose limit has lifted must not lose its capacity to a fresh
// offer, for ever. The reservation takes every free unit and the hub fills
// whatever the request advertises, so a runner whose hub has a standing
// queue would hand each freed unit to a new run at every sync and the run
// that has already waited hours would never move — and with a max_wait
// would be timed out for a limit that had in fact lifted.
//
// The unit is taken before the request is built, so the hub is never
// offered it in the first place.
func TestAParkedRunKeepsItsCapacityFromTheHubsQueue(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	// One unit of capacity, and a hub with more work than that.
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), byHome(map[string]fake.Script{
		"work": limitScript("five_hour", reset, "native-1"),
	}))
	claimAndRun(t, l, x)
	waitingRow(t, e, "a")
	if err := e.store.SetAccountLimit(ctx, db.SetAccountLimitParams{
		Harness: "claude", Label: "work",
		LimitedUntil: sql.NullInt64{Int64: time.Now().Add(-time.Minute).UnixMilli(), Valid: true},
		UpdatedAt:    time.Now().UnixMilli(),
	}); err != nil {
		t.Fatal(err)
	}
	// The queue the parked run is competing with.
	e.enqueue(t, testRun("b", "s2"), testRun("c", "s3"))

	x2, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "resumed", NativeSessionID: "native-1"},
	}))
	var offered int
	l.Hub = hubFunc{Hub: l.Hub, sync: func(req v1.SyncRequest) { offered = req.Health.FreeCapacity.Total }}
	syncAt(t, l, x2, reset.Add(resumeSkew+time.Second))

	if offered != 0 {
		t.Errorf("the sync advertised %d free units while a parked run was due; the hub fills what it is offered", offered)
	}
	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunSucceeded {
		t.Fatalf("result = %+v, %v — the parked run lost its capacity to the hub's queue", res, ok)
	}
	if _, ok := e.store.GetRun(ctx, db.GetRunParams{Connection: "hub", ID: "b"}); ok == nil {
		t.Error("a queued run was claimed while the parked one was still waiting for capacity")
	}
}

// The collector ends a parked run past its cap from its own goroutine while
// a connection's sync may be resuming it, so "is it still waiting" has to be
// asked inside the transaction that ends it. Asked outside, the collector's
// write lands on a run that is already running and reports it timed out —
// a second terminal state for one run, against DOMAIN.md's rule that a run
// reaches exactly one.
//
// Driven through Exec.End directly, because the two goroutines cannot be
// made to interleave on demand: the property is that a row which stopped
// waiting is refused, whenever that happened.
func TestEndingAParkedRunRefusesOneThatIsNoLongerWaiting(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	row := waitingRow(t, e, "a")

	// The sync got there first: the run is out of waiting and running.
	if err := e.store.SetRunState(ctx, db.SetRunStateParams{
		State: string(v1.RunRunning), UpdatedAt: time.Now().UnixMilli(), Connection: "hub", ID: "a",
	}); err != nil {
		t.Fatal(err)
	}

	claim, err := claimFor(row, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The collector, acting on the row it listed a moment earlier.
	if err := x.End(ctx, claim, row, v1.RunTimedOut,
		&v1.RunError{Class: ClassMaxWait, Message: "too long"}, time.Now()); err == nil {
		t.Fatal("a run that had stopped waiting was ended anyway")
	}
	if got := localRun(t, e, "a").State; got != string(v1.RunRunning) {
		t.Errorf("local state = %s; the running run was overwritten", got)
	}
	if res, ok := outboxResult(t, e, "a"); ok {
		t.Errorf("a terminal result was written for a running run: %+v", res)
	}
}

// endsWith is an executor whose End always fails the given way: the
// collector losing the race to a connection's own sync, on demand.
type endsWith struct {
	Executor
	err error
}

func (e endsWith) End(context.Context, Claim, db.Run, v1.RunState, *v1.RunError, time.Time) error {
	return e.err
}

// The collector lists a parked run whose cap has run out and its
// connection's sync resumes it before the write lands. The guard refuses
// the write, which is right, and the run is then running perfectly well:
// saying it timed out is false, and on an unattended runner it is what an
// operator reads in the morning.
func TestTheCollectorSaysNothingWhenItLosesTheRace(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Second)

	run := testRun("a", "s1")
	run.MaxWaitMS = (30 * time.Minute).Milliseconds()
	l := e.loop(t, 1)
	e.enqueue(t, run)
	x, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	waitingRow(t, e, "a")

	var logged bytes.Buffer
	c := collectorFor(e, x)
	// Still waiting when it is listed; gone by the time the write happens.
	c.Runs = endsWith{Executor: x, err: errNoLongerWaiting}
	c.Log = slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c.Clock = &stepClock{now: time.Now().Add(45 * time.Minute)}
	if err := c.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logged.String(), "it is timed out") {
		t.Errorf("the collector announced a timeout it did not write:\n%s", logged.String())
	}

	// And when the write does land, it does say so.
	logged.Reset()
	c.Runs = x
	if err := c.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logged.String(), "it is timed out") {
		t.Errorf("the collector timed a run out and said nothing:\n%s", logged.String())
	}
}

// A run that stopped waiting is not a storage fault. Reported as one, an
// unattended runner logs ERROR for the ordinary outcome of two goroutines
// racing over a parked run — which is the thing an operator is woken by.
func TestEndingARunThatStoppedWaitingIsNotLoggedAsAFailure(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, logged := e.accountExecutor(t, accountConfig("work"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	row := waitingRow(t, e, "a")
	if err := e.store.SetRunState(ctx, db.SetRunStateParams{
		State: string(v1.RunRunning), UpdatedAt: time.Now().UnixMilli(), Connection: "hub", ID: "a",
	}); err != nil {
		t.Fatal(err)
	}
	logged.Reset()

	claim, err := claimFor(row, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.End(ctx, claim, row, v1.RunTimedOut, &v1.RunError{Class: ClassMaxWait, Message: "too long"}, time.Now()); err == nil {
		t.Fatal("a run that had stopped waiting was ended anyway")
	}
	for _, said := range []string{"result not recorded", "the run stays held"} {
		if strings.Contains(logged.String(), said) {
			t.Errorf("a refused write was logged as %q:\n%s", said, logged.String())
		}
	}
}

// A sync that cannot read its account states reads them once, not once per
// caller — a second read that succeeded where the first failed would leave
// holdWaiting judging against no accounts while health and the resume used
// real ones, which is the disagreement the single read exists to prevent.
//
// And it says what is actually lost. A run whose resume time has passed is
// due on the clock alone and still runs; only the early resume, on an
// account that freed before that time, waits for the next sync.
func TestASyncThatCannotReadItsAccountsReadsThemOnceAndSaysWhatIsLost(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	broken, err := store.Open(ctx, e.paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	broken.Close()

	var logged bytes.Buffer
	l := &Loop{Connection: "hub", Store: broken, Accounts: accountsOf(e.paths.Data, accountConfig("work")),
		Log: slog.New(slog.NewJSONHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	l.init()
	l.loadAccounts(ctx)
	l.loadAccounts(ctx)

	if l.accountsErr == nil {
		t.Fatal("the read did not fail; the rest of this test proves nothing")
	}
	if n := strings.Count(logged.String(), "could not read account states"); n != 1 {
		t.Errorf("the failed read was reported %d times in one sync, want once:\n%s", n, logged.String())
	}
	if strings.Contains(logged.String(), "resumes nothing") {
		t.Errorf("the warning says the sync resumes nothing:\n%s", logged.String())
	}
	// The claim the message must not make: a run past its resume time is
	// due without any account being consulted.
	past := time.Now().Add(-time.Hour)
	row := db.Run{ResumesAt: sql.NullInt64{Int64: past.UnixMilli(), Valid: true}}
	if !l.due(row, v1.Run{Harness: "claude"}, time.Now()) {
		t.Error("a run an hour past its resume time is not due with no account states; the warning would be right and the code wrong")
	}
}
