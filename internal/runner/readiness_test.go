package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
	"github.com/skkap/yad/internal/store/db"
)

// silentOrSpeaking is a harness that hangs without a word for the runs named
// in silent, and answers the rest.
func silentOrSpeaking(silent map[string]bool) *fake.Adapter {
	return &fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
		if silent[s.RunID] {
			return fake.Script{Hang: true}
		}
		return fake.Script{Events: []v1.Event{{Kind: v1.EventText, Text: "hi"}}, Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "hi"}}
	}}
}

// runEach takes each run through its own claim, one after another, as a hub
// sending them one at a time would.
func runEach(t *testing.T, e *env, l *Loop, x *Exec, ids ...string) {
	t.Helper()
	for _, id := range ids {
		e.enqueue(t, testRun(id, "s-"+id))
		claimAndRun(t, l, x)
	}
}

// healthNow is the health a sync would carry now, with the reservation it
// takes handed back, so a later sync can still claim.
func healthNow(ctx context.Context, l *Loop) v1.Health {
	res := l.Pool.Reserve(l.Connection)
	defer res.Close()
	// Read afresh, as each sync does: outside one, the last read stays.
	l.accounts, l.silences, l.accountsTried, l.accountsErr = nil, nil, false, nil
	// The executor dates a cool-down by the wall clock, as the daemon's
	// loop does; the fixture's loop is on a fixed day in the past.
	clock := l.Clock
	l.Clock = realClock{}
	defer func() { l.Clock = clock }()
	return l.health(ctx, heldNothing(res))
}

func claimed(t *testing.T, e *env, id string) bool {
	t.Helper()
	_, err := e.store.GetRun(context.Background(), db.GetRunParams{Connection: "hub", ID: id})
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return err == nil
}

// The harness DEV-170 found: a Claude on its own login that never says a
// word. Its third silent run in a row makes it not ready, so the runner stops
// claiming for it and the hub is told until when; the error of that run says
// so, with the command that offers it again at once (decision 0077).
func TestThreeSilentRunsMakeTheOwnLoginNotReady(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	l := e.loop(t, 1)
	x := e.executor(silentOrSpeaking(map[string]bool{"a": true, "b": true, "c": true, "d": true}))
	x.Config.Supervise.FirstOutput = config.Duration{Duration: 30 * time.Millisecond}

	runEach(t, e, l, x, "a", "b")
	if h := healthNow(ctx, l); len(h.Harnesses) != 1 || !h.Harnesses[0].Ready || h.Harnesses[0].SilentUntil != nil {
		t.Fatalf("after two silent runs, health = %+v; want ready", h.Harnesses)
	}
	before := time.Now()
	runEach(t, e, l, x, "c")
	res, ok := outboxResult(t, e, "c")
	if !ok || res.Error == nil || res.Error.Class != ClassHarnessSilent {
		t.Fatalf("result = %+v", res)
	}
	if !strings.Contains(res.Error.Message, "this is the 3rd run in a row on claude's own login to say nothing, so the runner reports it not ready until ") {
		t.Errorf("message %q does not say the harness is now not ready", res.Error.Message)
	}
	cmds := shellwordtest.Commands(res.Error.Message, "yad ")
	shellwordtest.Check(t, cmds[len(cmds)-1], "yad", "--profile", "default", "daemon", "restart")

	h := healthNow(ctx, l)
	if len(h.Harnesses) != 1 || h.Harnesses[0].Ready || h.Harnesses[0].SilentUntil == nil {
		t.Fatalf("after three, health = %+v; want not ready with silent_until", h.Harnesses)
	}
	if until := *h.Harnesses[0].SilentUntil; until.Before(before.Add(15*time.Minute)) || until.After(time.Now().Add(15*time.Minute)) {
		t.Errorf("silent until %s, want a quarter of an hour on", until)
	}
	// Left for the hub, not refused: there is nothing wrong with the run.
	runEach(t, e, l, x, "d")
	if claimed(t, e, "d") {
		t.Error("a run was claimed for a harness reported not ready")
	}
	if _, ok := outboxResult(t, e, "d"); ok {
		t.Error("the runner owes the hub a result for a run it never took")
	}
}

// One turn that says anything at all clears the count: two silent runs, one
// that answers, and two more silent ones leave the harness ready.
func TestARunThatSpeaksStartsTheCountAgain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	l := e.loop(t, 1)
	x := e.executor(silentOrSpeaking(map[string]bool{"a": true, "b": true, "d": true, "e": true}))
	x.Config.Supervise.FirstOutput = config.Duration{Duration: 30 * time.Millisecond}
	runEach(t, e, l, x, "a", "b", "c", "d", "e")
	if h := healthNow(ctx, l); !h.Harnesses[0].Ready {
		t.Errorf("health = %+v; a run that spoke between them should have started the count again", h.Harnesses)
	}
	all, err := account.LoadSilences(ctx, e.store.Queries)
	if err != nil {
		t.Fatal(err)
	}
	if s := all[account.Ref{Harness: "claude"}]; s.Runs != 2 {
		t.Errorf("record = %+v, want two silent runs since the one that spoke", s)
	}
}

// Silence belongs to a home, so a silent account is passed over for one that
// works, and the harness stays ready while one can take a run. Only when the
// last goes silent too is it not ready.
func TestASilentAccountIsPassedOverForOneThatWorks(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, label := range []string{"work", "spare"} {
		plantCredential(t, e.paths.Data, label)
	}
	now := time.Now()
	for range account.SilentRuns {
		if _, err := account.NoteSilent(ctx, e.store.Queries, "claude", "work", now); err != nil {
			t.Fatal(err)
		}
	}
	cfg := accountConfig("work", "spare")
	l := e.loop(t, 1)
	l.Accounts = accountsOf(e.paths.Data, cfg)
	ad := silentOrSpeaking(nil)
	x, _ := e.accountExecutor(t, cfg, ad)
	x.Accounts = l.Accounts
	runEach(t, e, l, x, "a")
	if len(ad.Starts) != 1 || ad.Starts[0].Account != "spare" {
		t.Fatalf("starts = %+v, want one on spare", ad.Starts)
	}
	h := healthNow(ctx, l)
	if !h.Harnesses[0].Ready {
		t.Errorf("health = %+v; spare can take a run", h.Harnesses)
	}
	if got := h.Harnesses[0].Accounts[0]; got.Label != "work" || got.State != v1.AccountFree || got.SilentUntil == nil {
		t.Errorf("work reads %+v, want free with silent_until", got)
	}

	for range account.SilentRuns {
		if _, err := account.NoteSilent(ctx, e.store.Queries, "claude", "spare", now); err != nil {
			t.Fatal(err)
		}
	}
	h = healthNow(ctx, l)
	if h.Harnesses[0].Ready || h.Harnesses[0].SilentUntil != nil {
		t.Errorf("health = %+v; want not ready, and silent_until on the accounts rather than the harness", h.Harnesses)
	}
	// A run that reached the executor anyway — claimed in the moment before
	// — is refused with the reason, not run on a silent home.
	err := fmt.Sprint(func() error { _, _, _, err := x.pickAccount(ctx, "claude", "z"); return err }())
	if !strings.Contains(err, "work silent until") || !strings.Contains(err, "spare silent until") {
		t.Errorf("pick = %s", err)
	}
}

// A daemon start is the owner's word that the cause is fixed: every cool-down
// ends, and the count stays, so a home still wedged is not ready again after
// its very next silent run rather than three more.
func TestADaemonStartEndsTheCoolDownAndKeepsTheCount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for range account.SilentRuns {
		if _, err := account.NoteSilent(ctx, e.store.Queries, "claude", "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := Serve(ctx, Options{
		Paths: e.paths, Config: config.Default(), RunnerID: "r",
		Capabilities: func() v1.Capabilities { return drivableDoc("r", 1) },
		Drain:        drained(),
	}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	all, err := account.LoadSilences(ctx, e.store.Queries)
	if err != nil {
		t.Fatal(err)
	}
	if s := all[account.Ref{Harness: "claude"}]; s.Runs != account.SilentRuns || s.Holds(time.Now()) {
		t.Errorf("after a start the record is %+v; want the count kept and the cool-down over", s)
	}
}

// A harness that gets its first word out while the cancel ladder is already
// stopping it has still failed its run harness_silent, and the run counts as
// silent: a start that keeps landing just past the timeout must not reset the
// count it is failing every run of.
func TestAWordAfterTheStopDoesNotClearTheCount(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	l := e.loop(t, 1)
	late := &fake.Adapter{ID: "claude", Next: func(adapter.Spec) fake.Script {
		return fake.Script{Events: []v1.Event{{Kind: v1.EventStatus, Status: "started"}}, Delay: 150 * time.Millisecond, Hang: true, IgnoreInterrupt: true}
	}}
	x := e.executor(late)
	x.Config.Supervise.FirstOutput = config.Duration{Duration: 30 * time.Millisecond}
	// Long enough for the late word to arrive before SIGTERM's rung.
	x.Grace = time.Second
	runEach(t, e, l, x, "a", "b", "c")
	res, ok := outboxResult(t, e, "c")
	if !ok || res.Error == nil || res.Error.Class != ClassHarnessSilent || res.Metrics.FirstEventMS <= 0 {
		t.Fatalf("result = %+v (error %+v); want harness_silent with a late first event", res, res.Error)
	}
	if h := healthNow(ctx, l); h.Harnesses[0].Ready {
		t.Errorf("health = %+v; three runs stopped silent should leave it not ready", h.Harnesses)
	}
}
