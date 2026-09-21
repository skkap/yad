package runner

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/store/db"
)

// restartedAs is a second Loop for the same runner and credential: the next
// process, which has none of the first's memory.
func restartedAs(t *testing.T, e *env, l *Loop, x Executor) *Loop {
	t.Helper()
	return &Loop{
		Connection: l.Connection, RunnerID: l.RunnerID, Hub: l.Hub, Store: e.store,
		Pool: NewPool(v1.Capacity{Total: 1}), Capabilities: l.Capabilities,
		Executor: x, Clock: e.clock, Rand: l.Rand,
	}
}

// A run reported lost is the one a hub can only ask "how much?" about, and
// the answer used to be nothing: no duration, no wait, no switches, and —
// because the row's `spent` carries usage — zero tokens for a run that may
// have spent an account's whole window before the runner stopped. Those are
// tokens the owner paid for that no hub was ever told about.
func TestALostRunReportsWhatItsRowKnowsItCost(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	mustSync(t, l)
	mustSync(t, l) // claimed and listed; the process dies while it runs

	// What a previous process would have left in the row: an hour of
	// running, half an hour of it parked, one move, and real usage.
	started := time.Now().Add(-time.Hour)
	ended := time.Now().Add(-10 * time.Minute)
	spent, err := json.Marshal(spent{
		Usage:      map[string]v1.Usage{"opus": {Model: "opus", Input: 900, Output: 400}},
		ToolCalls:  7,
		APIRetries: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DB.ExecContext(ctx, `UPDATE runs SET state = 'running', started_at = ?,
		waited_ms = ?, account_switches = 1, spent = ?, updated_at = ?
		WHERE connection = 'hub' AND id = 'a'`,
		started.UnixMilli(), (30 * time.Minute).Milliseconds(), string(spent), ended.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	if err := restartedAs(t, e, l, e.exec).Recover(ctx); err != nil {
		t.Fatal(err)
	}

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunLost {
		t.Fatalf("result = %+v, %v, want lost", res, ok)
	}
	if got := res.Usage.ByModel["opus"]; got.Input != 900 || got.Output != 400 {
		t.Errorf("usage = %+v; the tokens the run spent are not reported anywhere else", got)
	}
	if res.Metrics.WaitedMS != (30 * time.Minute).Milliseconds() {
		t.Errorf("waited_ms = %d, want the half hour the row records", res.Metrics.WaitedMS)
	}
	if res.Metrics.AccountSwitches != 1 {
		t.Errorf("account_switches = %d, want 1", res.Metrics.AccountSwitches)
	}
	if res.Metrics.ToolCalls != 7 || res.Metrics.APIRetries != 2 {
		t.Errorf("tool_calls = %d, api_retries = %d, want 7 and 2", res.Metrics.ToolCalls, res.Metrics.APIRetries)
	}
	// Measured to the last moment anything was known to be true of the run,
	// which is fifty minutes after it started — not to now, which would add
	// however long the runner was down.
	want := ended.Sub(started).Milliseconds()
	if res.Metrics.DurationMS != want {
		t.Errorf("duration_ms = %d, want %d — measured to the run's last write, not to the restart",
			res.Metrics.DurationMS, want)
	}
}

// The floor is the later of the run's last write and its last event, because
// runs.updated_at moves only when a column does: a turn that streamed for
// three hours and touched no column since reaching `running` would otherwise
// report a duration of nearly zero.
func TestALostRunsDurationReachesItsLastEvent(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	mustSync(t, l)
	mustSync(t, l)

	started := time.Now().Add(-3 * time.Hour)
	rowWritten := started.Add(time.Second) // reaching `running`, and nothing after
	spoke := time.Now().Add(-5 * time.Minute)
	if _, err := e.store.DB.ExecContext(ctx, `UPDATE runs SET state = 'running', started_at = ?, updated_at = ?
		WHERE connection = 'hub' AND id = 'a'`, started.UnixMilli(), rowWritten.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(v1.Event{Seq: 1, At: spoke, Kind: v1.EventText, Text: "still here"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.AppendEvent(ctx, db.AppendEventParams{
		Connection: "hub", RunID: "a", Seq: 1, Body: string(body),
	}); err != nil {
		t.Fatal(err)
	}

	if err := restartedAs(t, e, l, e.exec).Recover(ctx); err != nil {
		t.Fatal(err)
	}
	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunLost {
		t.Fatalf("result = %+v, %v, want lost", res, ok)
	}
	if want := spoke.Sub(started).Milliseconds(); res.Metrics.DurationMS != want {
		t.Errorf("duration_ms = %d, want %d — the run was still speaking long after its row was last written",
			res.Metrics.DurationMS, want)
	}
	if res.LastSeq != 1 {
		t.Errorf("last_seq = %d, want 1", res.LastSeq)
	}
}

// account_switches counts a move across a park, which is the case no test
// covered: the run is parked by one process and resumed by another, onto a
// different account, so the count has to come back from the row rather than
// from the memory of a process that is gone.
func TestAMoveAcrossAParkIsCountedAfterARestart(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, label := range []string{"work", "spare"} {
		plantCredential(t, e.paths.Data, label)
	}
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)

	// work runs first and hits its limit; spare cannot take over yet, so
	// the run parks rather than moving within the process.
	if err := account.SetState(ctx, e.store.Queries, "claude", "spare", v1.AccountNeedsLogin, time.Now()); err != nil {
		t.Fatal(err)
	}
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x, _ := e.accountExecutor(t, accountConfig("work", "spare"), fakeHarness(limitScript("five_hour", reset, "native-1")))
	claimAndRun(t, l, x)
	row := waitingRow(t, e, "a")
	if row.AccountSwitches != 0 {
		t.Fatalf("account_switches = %d before any move", row.AccountSwitches)
	}

	// A new process, and by the time it runs, spare has been logged in.
	// The park is backdated so the wait is a real hour rather than the
	// microsecond this test would otherwise measure — the point is that
	// both figures survive a restart, and one of them cannot be asserted
	// if it is only ever zero.
	if _, err := e.store.DB.ExecContext(ctx,
		`UPDATE runs SET waiting_since = ? WHERE connection = 'hub' AND id = 'a'`,
		time.Now().Add(-time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if err := account.SetState(ctx, e.store.Queries, "claude", "spare", v1.AccountFree, time.Now()); err != nil {
		t.Fatal(err)
	}
	x2, _ := e.accountExecutor(t, accountConfig("work", "spare"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"},
	}))
	restarted := restartedAs(t, e, l, x2)
	if err := restarted.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	syncAt(t, restarted, x2, time.Now())

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunSucceeded {
		t.Fatalf("result = %+v, %v — the run should have resumed on spare", res, ok)
	}
	if res.Metrics.AccountSwitches != 1 {
		t.Errorf("account_switches = %d, want 1 — the run resumed on an account it did not start on", res.Metrics.AccountSwitches)
	}
	if want := time.Hour.Milliseconds(); res.Metrics.WaitedMS < want-time.Minute.Milliseconds() {
		t.Errorf("waited_ms = %d, want about %d — the wait served before the restart", res.Metrics.WaitedMS, want)
	}
}

// And a run that parks and comes back on the same account reports no move:
// the count is of accounts changed, not of parks served.
func TestAParkThatResumesOnTheSameAccountIsNotASwitch(t *testing.T) {
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
	x2, _ := e.accountExecutor(t, accountConfig("work"), fakeHarness(fake.Script{
		Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"},
	}))
	syncAt(t, restartedAs(t, e, l, x2), x2, reset.Add(resumeSkew+time.Second))

	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunSucceeded {
		t.Fatalf("result = %+v, %v", res, ok)
	}
	if res.Metrics.AccountSwitches != 0 {
		t.Errorf("account_switches = %d, want 0 — the run came back on the account it left", res.Metrics.AccountSwitches)
	}
}

// A `spent` blob that will not decode must leave nothing behind.
// json.Unmarshal fills what it parsed before it failed, so a half-decoded
// row would ship half a number under a log line saying it shipped none of
// it — and half a usage figure is worse than no usage figure, because a hub
// cannot tell it from a whole one.
func TestAnUnreadableSpentLeavesNoHalfDecodedNumbers(t *testing.T) {
	// Valid JSON as far as the counters, then a type error: exactly what a
	// truncated or hand-edited row looks like to the decoder.
	row := db.Run{
		Connection: "hub", ID: "a",
		Spent: sql.NullString{Valid: true, String: `{"tool_calls":7,"api_retries":2,"usage":"not an object"}`},
	}
	var logged bytes.Buffer
	p := progressOf(row, time.Now(), slog.New(slog.NewJSONHandler(&logged, nil)))

	if p.spent.ToolCalls != 0 || p.spent.APIRetries != 0 || len(p.spent.Usage) != 0 {
		t.Errorf("a failed decode left %+v behind; the run's result would carry part of a number", p.spent)
	}
	if !strings.Contains(logged.String(), "carries none of it") {
		t.Errorf("the failure was not reported:\n%s", logged.String())
	}
}

// A run that moves accounts in process and is lost before it ever parks
// (DEV-86). What it cost is written as each turn ends and each move is made,
// not only by a park, so the restart that reports it lost still has every
// finished turn's usage and every move to report. It used to report zeros:
// the park was the only writer, and this run never parked.
//
// The turn the runner dies in contributes nothing, and that is the honest
// answer rather than a gap: a harness reports usage when its turn ends, so
// a turn that never ended has no figure anybody could have written down.
func TestARunLostAfterAMoveReportsEveryTurnItFinished(t *testing.T) {
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	limited := func(input, output int64, tool string, retries int) fake.Script {
		s := limitScript("five_hour", reset, "native-1")
		s.Events = []v1.Event{{Kind: v1.EventToolCall, Tool: &v1.ToolEvent{ID: tool, Name: "Bash", Input: "ls"}}}
		s.Outcome.Usage = map[string]v1.Usage{"opus": {Model: "opus", Input: input, Output: output}}
		s.Outcome.APIRetries = retries
		return s
	}
	hangs := fake.Script{Hang: true, Outcome: adapter.Outcome{NativeSessionID: "native-1"}}
	tests := []struct {
		name     string
		accounts []string
		scripts  map[string]fake.Script
		// dies is the account whose turn the runner is killed in.
		dies      string
		usage     v1.Usage
		switches  int
		toolCalls int
		retries   int
	}{
		{
			name:     "one move, lost on the second account",
			accounts: []string{"work", "personal"},
			scripts:  map[string]fake.Script{"work": limited(900, 400, "t1", 2), "personal": hangs},
			dies:     "personal",
			usage:    v1.Usage{Model: "opus", Input: 900, Output: 400},
			switches: 1, toolCalls: 1, retries: 2,
		},
		{
			name:     "two moves, lost on the third account",
			accounts: []string{"work", "personal", "spare"},
			scripts: map[string]fake.Script{
				"work": limited(900, 400, "t1", 2), "personal": limited(70, 30, "t2", 1), "spare": hangs,
			},
			dies:     "spare",
			usage:    v1.Usage{Model: "opus", Input: 970, Output: 430},
			switches: 2, toolCalls: 2, retries: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			for _, label := range tt.accounts {
				plantCredential(t, e.paths.Data, label)
			}
			l := e.loop(t, 1)
			e.enqueue(t, testRun("a", "s1"))
			ad := byHome(tt.scripts)
			x, _ := e.accountExecutor(t, accountConfig(tt.accounts...), ad)
			l.Executor = x
			mustSync(t, l)
			// The run's goroutine lives on the context of the sync that
			// starts it, so cancelling that is the runner going down under it.
			runCtx, kill := context.WithCancel(ctx)
			defer kill()
			if _, err := l.SyncOnce(runCtx); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the turn on "+tt.dies+" starts", func() bool {
				return len(ad.Turns()) == len(tt.accounts)
			})
			kill()
			x.Wait()
			if got := filepath.Base(ad.Starts[len(ad.Starts)-1].Home); got != tt.dies {
				t.Fatalf("the last turn ran on %q, want %q", got, tt.dies)
			}
			if r := localRun(t, e, "a"); r.State != string(v1.RunRunning) {
				t.Fatalf("the run is %s before the restart, want running: it must never have parked", r.State)
			}

			if err := restartedAs(t, e, l, e.exec).Recover(ctx); err != nil {
				t.Fatal(err)
			}
			res, ok := outboxResult(t, e, "a")
			if !ok || res.State != v1.RunLost {
				t.Fatalf("result = %+v, %v, want lost", res, ok)
			}
			if got := res.Usage.ByModel["opus"]; got != tt.usage {
				t.Errorf("usage = %+v, want %+v: every turn that finished before the runner died", got, tt.usage)
			}
			m := res.Metrics
			if m.AccountSwitches != tt.switches || m.ToolCalls != tt.toolCalls || m.APIRetries != tt.retries {
				t.Errorf("account_switches = %d, tool_calls = %d, api_retries = %d; want %d, %d, %d",
					m.AccountSwitches, m.ToolCalls, m.APIRetries, tt.switches, tt.toolCalls, tt.retries)
			}
		})
	}
}
