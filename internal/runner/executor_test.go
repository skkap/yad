package runner

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/config"
	hubdb "github.com/skkap/yad/internal/hub/store/db"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store/db"
)

// fakeHarness plays script for every run, as the harness "claude" the test
// runner advertises.
func fakeHarness(script fake.Script) *fake.Adapter {
	return &fake.Adapter{ID: "claude", Next: func(adapter.Spec) fake.Script { return script }}
}

// executor is a real Exec over the env's store, driving the given adapters.
func (e *env) executor(adapters ...adapter.Adapter) *Exec {
	return &Exec{
		Store: e.store, Adapters: NewRegistry(adapters...), Config: config.Default(), Data: e.paths.Data,
		Binary: func(h string) (string, bool) { return "/nonexistent/" + h, true },
		Grace:  20 * time.Millisecond, TermGrace: 20 * time.Millisecond,
	}
}

func (e *env) reporter(l *Loop) *Reporter {
	return NewReporter(l.Connection, l.Hub.(*hubclient.Client), e.store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
}

// claimAndRun takes the env's queued runs through the claim and hands them to
// x, then waits for every one of them to finish.
func claimAndRun(t *testing.T, l *Loop, x *Exec) {
	t.Helper()
	l.Executor = x
	mustSync(t, l)
	mustSync(t, l)
	x.Wait()
}

func localRun(t *testing.T, e *env, id string) db.Run {
	t.Helper()
	r, err := e.store.GetRun(context.Background(), db.GetRunParams{Connection: "hub", ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func outboxResult(t *testing.T, e *env, id string) (v1.Result, bool) {
	t.Helper()
	rows, err := e.store.DueOutbox(context.Background(), db.DueOutboxParams{Connection: "hub", NextAttemptAt: 1 << 62})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range rows {
		if o.RunID == id {
			var res v1.Result
			if err := json.Unmarshal([]byte(o.Body), &res); err != nil {
				t.Fatal(err)
			}
			return res, true
		}
	}
	return v1.Result{}, false
}

func hubResult(t *testing.T, e *env, id string) v1.Result {
	t.Helper()
	r, err := e.hubStore.GetResult(context.Background(), id)
	if err != nil {
		t.Fatalf("hub has no result for %s: %v", id, err)
	}
	var res v1.Result
	if err := json.Unmarshal([]byte(r.Body), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// One run, the whole path: claimed, prepared, streamed, spooled, finished into
// the outbox, and delivered — events first, then the result — to yad hub.
func TestExecutorRunsAClaimToTheHub(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x := e.executor(fakeHarness(fake.Script{
		Events: []v1.Event{
			{Kind: v1.EventText, Text: "looking"},
			{Kind: v1.EventToolCall, Tool: &v1.ToolEvent{ID: "t1", Name: "Bash", Input: "ls"}},
			{Kind: v1.EventToolResult, Tool: &v1.ToolEvent{ID: "t1", Output: strings.Repeat("x", v1.MaxToolOutputBytes+10)}},
		},
		Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "done", NativeSessionID: "native-1", APIRetries: 2,
			Usage: map[string]v1.Usage{"opus": {Model: "opus", Input: 10, Output: 5}}},
	}))
	claimAndRun(t, l, x)

	if got := localRun(t, e, "a"); got.State != "succeeded" {
		t.Fatalf("local state = %s", got.State)
	}
	if l.Pool.Free() != 1 {
		t.Errorf("capacity not released: free = %d", l.Pool.Free())
	}
	sess, err := e.store.GetSession(context.Background(), db.GetSessionParams{Connection: "hub", ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.NativeID.String != "native-1" {
		t.Errorf("native session id = %q", sess.NativeID.String)
	}
	if fi, err := os.Stat(sess.Workdir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Errorf("workdir %q: %v %v", sess.Workdir, fi, err)
	}
	res, ok := outboxResult(t, e, "a")
	if !ok || res.LastSeq != 3 || res.Metrics.ToolCalls != 1 || res.Metrics.APIRetries != 2 || res.FinalText != "done" {
		t.Fatalf("outbox = %+v, %v", res, ok)
	}

	r := e.reporter(l)
	r.Flush(context.Background())
	if _, ok := outboxResult(t, e, "a"); ok {
		t.Error("result still in the outbox after the hub acknowledged it")
	}
	if e.hubState(t, "a") != "succeeded" {
		t.Errorf("hub state = %s", e.hubState(t, "a"))
	}
	if got := hubResult(t, e, "a"); got.Usage.ByModel["opus"].Input != 10 {
		t.Errorf("hub result = %+v", got)
	}
	evs, err := e.hubStore.EventsAfter(context.Background(), hubdb.EventsAfterParams{RunID: "a", Seq: 0, Limit: 100})
	if err != nil || len(evs) != 3 {
		t.Fatalf("hub events = %d, %v", len(evs), err)
	}
	var last v1.Event
	if err := json.Unmarshal([]byte(evs[2].Body), &last); err != nil {
		t.Fatal(err)
	}
	if len(last.Tool.Output) != v1.MaxToolOutputBytes || !last.Tool.Truncated {
		t.Errorf("tool output reached the hub at %d bytes, truncated=%v", len(last.Tool.Output), last.Tool.Truncated)
	}
	// The run is over and reported: the next sync lists nothing.
	res2 := mustSync(t, l)
	if len(cancels(res2)) != 0 {
		t.Errorf("sync after the result was answered with cancels %v", cancels(res2))
	}
}

func cancels(res v1.SyncResponse) []string {
	var out []string
	for _, c := range res.Controls {
		if c.Kind == v1.ControlCancel {
			out = append(out, c.RunID)
		}
	}
	return out
}

// The watchdogs stop a turn and the run times out, whatever the stopped
// harness says last; a harness that ignores the interrupt loses its process
// group after the grace.
func TestWatchdogs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*v1.Run)
		owner time.Duration
		ad    adapter.Adapter
		class string
	}{
		{"inactivity set by the run", func(r *v1.Run) { r.InactivityMS = 30 }, 0,
			fakeHarness(fake.Script{Events: []v1.Event{{Kind: v1.EventText, Text: "hi"}}, Hang: true}), ClassInactivity},
		{"inactivity from the owner's default", func(r *v1.Run) {}, 30 * time.Millisecond,
			fakeHarness(fake.Script{Hang: true}), ClassInactivity},
		{"a run cannot raise the owner's inactivity", func(r *v1.Run) { r.InactivityMS = int64(time.Hour / time.Millisecond) }, 30 * time.Millisecond,
			fakeHarness(fake.Script{Hang: true}), ClassInactivity},
		{"wall clock", func(r *v1.Run) { r.WallClockMS = 60 }, 0,
			// Events keep coming, so only the wall clock can stop it.
			fakeHarness(fake.Script{Events: manyEvents(1000), Delay: 5 * time.Millisecond}), ClassWallClock},
		{"interrupt ignored", func(r *v1.Run) { r.InactivityMS = 30 }, 0, deafHarness{}, ClassInactivity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			run := testRun("a", "s1")
			tc.edit(&run)
			e.enqueue(t, run)
			x := e.executor(tc.ad)
			if tc.owner > 0 {
				x.Config.Supervise.Inactivity = config.Duration{Duration: tc.owner}
			}
			claimAndRun(t, l, x)
			res, ok := outboxResult(t, e, "a")
			if !ok || res.State != v1.RunTimedOut || res.Error == nil || res.Error.Class != tc.class {
				t.Fatalf("result = %+v (error %+v), %v", res, res.Error, ok)
			}
			if tc.class == ClassInactivity && res.Metrics.Stalls != 1 {
				t.Errorf("stalls = %d", res.Metrics.Stalls)
			}
			if got := localRun(t, e, "a"); got.State != "timed_out" || got.Reason.String == "" {
				t.Errorf("local run = %s %q", got.State, got.Reason.String)
			}
		})
	}
}

func manyEvents(n int) []v1.Event {
	out := make([]v1.Event, n)
	for i := range out {
		out[i] = v1.Event{Kind: v1.EventText, Text: "tick"}
	}
	return out
}

// deafHarness ignores Interrupt and ends only when its context is cancelled —
// the harness the kill after the grace exists for.
type deafHarness struct{}

func (deafHarness) Harness() string { return "claude" }

func (deafHarness) Start(ctx context.Context, _ adapter.Spec) (adapter.Turn, error) {
	t := &deafTurn{events: make(chan v1.Event), done: make(chan struct{})}
	go func() {
		<-ctx.Done()
		close(t.events)
		close(t.done)
	}()
	return t, nil
}

type deafTurn struct {
	events chan v1.Event
	done   chan struct{}
}

func (t *deafTurn) Events() <-chan v1.Event { return t.events }
func (t *deafTurn) Steer(string) error      { return nil }
func (t *deafTurn) NativeSessionID() string { return "" }
func (t *deafTurn) Interrupt() error        { return errors.New("not listening") }
func (t *deafTurn) Terminate() error        { return nil }
func (t *deafTurn) Wait() adapter.Outcome {
	<-t.done
	return adapter.Outcome{State: v1.RunCancelled}
}

// Every way a run can fail before or at its start still ends in a failed
// result in the outbox, with a class a hub can act on.
func TestExecutorFailuresAreResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		x      func(e *env) *Exec
		grants []v1.Grant
		class  string
	}{
		{"no adapter for the harness", func(e *env) *Exec { return e.executor() }, nil, ClassRefused},
		{"harness not installed", func(e *env) *Exec {
			x := e.executor(fakeHarness(fake.Script{}))
			x.Binary = func(string) (string, bool) { return "", false }
			return x
		}, nil, ClassStart},
		{"adapter refuses to start", func(e *env) *Exec { return e.executor(&fake.Adapter{ID: "claude"}) }, nil, ClassStart},
		{"an outcome that is not terminal", func(e *env) *Exec {
			return e.executor(fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunWaiting}}))
		}, nil, ClassAdapter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			run := testRun("a", "s1")
			run.Grants = tc.grants
			e.enqueue(t, run)
			claimAndRun(t, l, tc.x(e))
			res, ok := outboxResult(t, e, "a")
			if !ok || res.State != v1.RunFailed || res.Error == nil || res.Error.Class != tc.class {
				t.Fatalf("result = %+v (error %+v), %v", res, res.Error, ok)
			}
			if l.Pool.Free() != 1 {
				t.Errorf("capacity not released: free = %d", l.Pool.Free())
			}
		})
	}
}

// A run's effort reaches the adapter as the hub sent it; the runner checks no
// name. One carrying an effort for a harness whose adapter cannot apply it is
// refused rather than run at the harness's default, which would read to the
// hub as the effort it asked for (decision 0049).
func TestExecutorHandsOverTheEffortOrRefusesTheRun(t *testing.T) {
	for _, tc := range []struct {
		name     string
		noEffort bool
		effort   string
		state    v1.RunState
	}{
		{"an adapter that applies it", false, "some-level-yad-never-heard-of", v1.RunSucceeded},
		{"an adapter that cannot", true, "high", v1.RunFailed},
		{"no effort, an adapter that cannot", true, "", v1.RunSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			run := testRun("a", "s1")
			run.Effort = tc.effort
			e.enqueue(t, run)
			ad := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "ok"}})
			ad.NoEffort = tc.noEffort
			claimAndRun(t, l, e.executor(ad))
			res, ok := outboxResult(t, e, "a")
			if !ok || res.State != tc.state {
				t.Fatalf("result = %+v (error %+v), %v, want %s", res, res.Error, ok, tc.state)
			}
			if tc.state == v1.RunFailed {
				if res.Error == nil || res.Error.Class != ClassRefused || !strings.Contains(res.Error.Message, "effort") {
					t.Errorf("error %+v, want %s naming the effort", res.Error, ClassRefused)
				}
				if len(ad.Starts) != 0 {
					t.Errorf("the harness was started for a run it would have run at the wrong effort")
				}
				return
			}
			if len(ad.Starts) != 1 || ad.Starts[0].Effort != tc.effort {
				t.Errorf("the adapter was handed %+v, want effort %q", ad.Starts, tc.effort)
			}
		})
	}
}

// startErr is an adapter whose Start fails with err.
type startErr struct{ err error }

func (a startErr) Harness() string { return "claude" }
func (a startErr) Start(context.Context, adapter.Spec) (adapter.Turn, error) {
	return nil, a.err
}

// A failure that is the runner's own — a harness that will not exec, a
// directory under its data or an account home that cannot be made — reaches
// the hub as what failed, never as the error behind it: that names paths under
// the owner's home and, for a start, the exec error (DEV-67). The cause goes
// to the log the message sends the owner to. A start error whose next action
// is the hub's travels as it is, or the hub could not tell its own input was
// the problem.
func TestRunnerFailuresNameNoPathOnTheMachine(t *testing.T) {
	const bin = "/Users/someone/bin/claude"
	execErr := errors.New("start " + bin + ": fork/exec " + bin + ": permission denied")
	const hubsFault = `model "-x" is not a model name — ask the hub to send an alias such as sonnet`
	for _, tc := range []struct {
		name string
		// block makes a file of what the executor needs to be a directory
		// under Data, so making it fails with Data's path in the error.
		block    string
		accounts bool
		ad       adapter.Adapter
		grants   []v1.Grant
		class    string
		// want is the whole message when the error is the hub's to act on;
		// empty for a runner failure, which must name no path and send the
		// owner to the log.
		want string
	}{
		{name: "the harness will not exec", class: ClassStart,
			ad: startErr{&adapter.LocalError{Msg: "claude would not start on this runner", Err: execErr}}},
		{name: "a start error the hub acts on", class: ClassStart, want: hubsFault,
			ad: startErr{errors.New(hubsFault)}},
		{name: "the workdir cannot be made", class: ClassPrepare, block: "workdirs"},
		{name: "a grant cannot be delivered", class: ClassPrepare, block: "grants",
			grants: []v1.Grant{{Name: "DATABASE_URL", Value: "file-secret", As: v1.GrantFile}}},
		{name: "the account's home cannot be made", class: ClassPrepare, accounts: true,
			block: filepath.Join("transcripts", "claude")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			cfg := config.Default()
			if tc.accounts {
				cfg = accountConfig("work")
				plantCredential(t, e.paths.Data, "work")
			}
			if tc.block != "" {
				path := filepath.Join(e.paths.Data, tc.block)
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ad := tc.ad
			if ad == nil {
				ad = fakeHarness(fake.Script{})
			}
			x, logged := e.accountExecutor(t, cfg, ad)
			l := e.loop(t, 1)
			run := testRun("a", "s1")
			run.Grants = tc.grants
			e.enqueue(t, run)
			claimAndRun(t, l, x)
			res, ok := outboxResult(t, e, "a")
			if !ok || res.Error == nil || res.Error.Class != tc.class {
				t.Fatalf("result = %+v (error %+v), %v", res, res.Error, ok)
			}
			msg := res.Error.Message
			if tc.want != "" {
				if msg != tc.want {
					t.Errorf("message = %q, want the adapter's own %q", msg, tc.want)
				}
				return
			}
			for _, leak := range []string{bin, "/Users/", e.paths.Data, "fork/exec", "permission denied", "not a directory", "file-secret"} {
				if strings.Contains(msg, leak) {
					t.Errorf("message carries %q: %q", leak, msg)
				}
			}
			if !strings.Contains(msg, "`yad --profile default daemon logs`") {
				t.Errorf("message = %q, want it to send the owner to the log", msg)
			}
			if !strings.Contains(logged.String(), "permission denied") && !strings.Contains(logged.String(), "not a directory") {
				t.Errorf("the cause is not in the log the message points at:\n%s", logged)
			}
		})
	}
}

// Grants reach the harness in its environment or as 0600 files, never in the
// spec a hub could read back, and the files are gone however the run ends. The
// names are ones decision 0024 refused and 0038 accepts.
func TestGrantsAreDeliveredAndDestroyed(t *testing.T) {
	for _, state := range []v1.RunState{v1.RunSucceeded, v1.RunFailed, v1.RunCancelled} {
		t.Run(string(state), func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			run := testRun("a", "s1")
			run.Grants = []v1.Grant{
				{Name: "ZUMINO_TOKEN", Value: "env-secret", As: v1.GrantEnv},
				{Name: "GIT_SSH_COMMAND", Value: "ssh -i env-secret-key", As: v1.GrantEnv},
				{Name: "DATABASE_URL", Value: "file-secret", As: v1.GrantFile},
			}
			e.enqueue(t, run)
			var (
				mu       sync.Mutex
				seen     adapter.Spec
				fileBody string
				fileMode os.FileMode
			)
			ad := &fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
				mu.Lock()
				defer mu.Unlock()
				seen = s
				for _, kv := range s.Env {
					if path, ok := strings.CutPrefix(kv, "DATABASE_URL="); ok {
						b, _ := os.ReadFile(path)
						fi, _ := os.Stat(path)
						fileBody, fileMode = string(b), fi.Mode().Perm()
					}
				}
				return fake.Script{Outcome: adapter.Outcome{State: state}}
			}}
			claimAndRun(t, l, e.executor(ad))

			mu.Lock()
			defer mu.Unlock()
			for _, want := range []string{"ZUMINO_TOKEN=env-secret", "GIT_SSH_COMMAND=ssh -i env-secret-key"} {
				if !contains(seen.Env, want) {
					t.Errorf("env grant %q missing from %v", want, seen.Env)
				}
			}
			if fileBody != "file-secret" || fileMode != 0o600 {
				t.Errorf("file grant = %q at %v", fileBody, fileMode)
			}
			if _, err := os.Stat(filepath.Join(e.paths.Data, "grants", "hub", "a")); !os.IsNotExist(err) {
				t.Errorf("grant files outlived the run: %v", err)
			}
			for _, s := range []string{localRun(t, e, "a").Spec} {
				if strings.Contains(s, "secret") {
					t.Error("a grant reached the runner's database")
				}
			}
		})
	}
}

// The fourth way a run ends: the runner is killed. A deferred cleanup cannot
// run through a SIGKILL, so the grant files are still on disk — 0600 files
// holding what a hub sent — and the next start is what deletes them.
//
// A SIGKILL cannot be staged in this process, so the leftovers are put back at
// the directory the executor itself chose for a real run with a real file
// grant: the path under test is the one a crash would leave, not one the test
// made up.
func TestGrantFilesAreSweptAfterACrash(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	run := testRun("a", "s1")
	run.Grants = []v1.Grant{{Name: "DEPLOY_KEY", Value: "file-secret", As: v1.GrantFile}}
	e.enqueue(t, run)
	var (
		mu   sync.Mutex
		path string
	)
	ad := &fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
		mu.Lock()
		defer mu.Unlock()
		for _, kv := range s.Env {
			if p, ok := strings.CutPrefix(kv, "DEPLOY_KEY="); ok {
				path = p
			}
		}
		return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}}
	}}
	claimAndRun(t, l, e.executor(ad))
	mu.Lock()
	defer mu.Unlock()
	if path == "" {
		t.Fatal("the run was never given a file grant")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("file-secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A start that claims nothing still sweeps: the runner has no connection,
	// and it is asked to go as soon as it has started.
	ctx := context.Background()
	var logged strings.Builder
	cfg := config.Default()
	if err := Serve(ctx, Options{
		Paths: e.paths, Config: cfg, RunnerID: "r",
		Capabilities: func() v1.Capabilities { return drivableDoc("r", 1) },
		Log:          slog.New(slog.NewTextHandler(&logged, nil)), Drain: drained(),
	}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	// The owner is told that secrets sat on disk after their run ended — by
	// count alone, since a grant's name says as much about a hub as its value.
	if !strings.Contains(logged.String(), "files=1") {
		t.Errorf("the sweep said nothing about what it deleted:\n%s", logged.String())
	}
	if strings.Contains(logged.String(), "DEPLOY_KEY") || strings.Contains(logged.String(), "file-secret") {
		t.Errorf("the sweep logged a grant's name or value:\n%s", logged.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a grant file a crash left is still on disk at %s: %v", path, err)
	}
	if _, err := os.Stat(filepath.Join(e.paths.Data, "grants")); !os.IsNotExist(err) {
		t.Errorf("the grants directory outlived the sweep: %v", err)
	}

	// An ordinary start — nothing left behind — says nothing: the line is a
	// signal, and a signal every start carries is noise.
	logged.Reset()
	if err := Serve(ctx, Options{
		Paths: e.paths, Config: cfg, RunnerID: "r",
		Capabilities: func() v1.Capabilities { return drivableDoc("r", 1) },
		Log:          slog.New(slog.NewTextHandler(&logged, nil)), Drain: drained(),
	}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if strings.Contains(logged.String(), "grant") {
		t.Errorf("a start with nothing to sweep still said something:\n%s", logged.String())
	}
}

// The owner's harness settings reach the adapter; nothing from the run does.
func TestSettingsComeFromTheOwner(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	ad := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}})
	x := e.executor(ad)
	x.Config.Harness = map[string]config.HarnessConfig{"claude": {PermissionMode: "acceptEdits"}}
	claimAndRun(t, l, x)
	if len(ad.Starts) != 1 || ad.Starts[0].Settings["permission_mode"] != "acceptEdits" || len(ad.Starts[0].Settings) != 1 {
		t.Fatalf("starts = %+v", ad.Starts)
	}
}

// A runner that stops mid-run owes no result for it: the run stays held, and
// the next start settles it.
func TestStoppingTheRunnerLeavesTheRunHeld(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	x := e.executor(fakeHarness(fake.Script{Hang: true}))
	l.Executor = x
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := l.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := l.SyncOnce(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	x.Wait()
	if got := localRun(t, e, "a"); got.State != "running" {
		t.Errorf("local state = %s, want running", got.State)
	}
	if _, ok := outboxResult(t, e, "a"); ok {
		t.Error("a result was written for a run the runner stopped itself")
	}
}

func TestPathNameKeepsHubIDsInsideTheirDirectory(t *testing.T) {
	// Upper case is hashed too: a case-folding file system would give "S1"
	// and "s1" one directory.
	for _, id := range []string{"..", ".", "../../.ssh", "a/b", "", ".hidden", strings.Repeat("x", 65), "_abc", "ZUM-19", "S1"} {
		got := pathName(id)
		if strings.ContainsAny(got, "/\\") || got == "." || got == ".." || !strings.HasPrefix(got, "_") {
			t.Errorf("pathName(%q) = %q", id, got)
		}
	}
	for _, id := range []string{"s1", "zum-19", "run.2026-09-19", "ses_abc234"} {
		if got := pathName(id); got != id {
			t.Errorf("pathName(%q) = %q, want it kept", id, got)
		}
	}
	if pathName("../a") == pathName("../b") || pathName("S1") == pathName("s1") {
		t.Error("two ids hashed alike")
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// Text, error messages and final text are capped at maxTextBytes each, and a
// status at maxStatusBytes, cut on
// a rune boundary, so one event or one result stays well under yad hub's body
// limit.
func TestTextIsCapped(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	e.enqueue(t, testRun("a", "s1"))
	huge := strings.Repeat("é", maxTextBytes) // two bytes a rune, so the cut lands mid-rune
	claimAndRun(t, l, e.executor(fakeHarness(fake.Script{
		Events: []v1.Event{
			{Kind: v1.EventText, Text: huge},
			{Kind: v1.EventError, Error: &v1.RunError{Class: "x", Message: huge}},
			{Kind: v1.EventStatus, Status: "warning: " + huge},
		},
		Outcome: adapter.Outcome{State: v1.RunFailed, FinalText: huge, Error: &v1.RunError{Class: "x", Message: huge}},
	})))
	rows, err := e.store.UnackedEvents(context.Background(), db.UnackedEventsParams{Connection: "hub", RunID: "a", Limit: 10})
	if err != nil || len(rows) != 3 {
		t.Fatalf("spool %d rows, %v", len(rows), err)
	}
	var text, errEv v1.Event
	if err := json.Unmarshal([]byte(rows[0].Body), &text); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(rows[1].Body), &errEv); err != nil {
		t.Fatal(err)
	}
	var status v1.Event
	if err := json.Unmarshal([]byte(rows[2].Body), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Status) > maxStatusBytes || len(status.Status) < maxStatusBytes-1 || !utf8.ValidString(status.Status) {
		t.Errorf("status is %d bytes, valid UTF-8 %v", len(status.Status), utf8.ValidString(status.Status))
	}
	res, _ := outboxResult(t, e, "a")
	for name, s := range map[string]string{"text": text.Text, "event error": errEv.Error.Message, "final text": res.FinalText, "result error": res.Error.Message} {
		if len(s) > maxTextBytes || len(s) < maxTextBytes-1 || !utf8.ValidString(s) {
			t.Errorf("%s is %d bytes, valid UTF-8 %v", name, len(s), utf8.ValidString(s))
		}
	}
}

// A grant that would break the run and reaches the executor anyway — a claim
// path that forgot to check — is refused there, before anything is delivered
// or started.
func TestExecutorRefusesGrantsThatWouldBreakTheRun(t *testing.T) {
	e := newEnv(t)
	ad := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}})
	x := e.executor(ad)
	ctx := context.Background()
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "s1", Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateRun(ctx, db.CreateRunParams{Connection: "hub", ID: "a", SessionID: "s1", Harness: "claude", Spec: "{}"}); err != nil {
		t.Fatal(err)
	}
	run := testRun("a", "s1")
	run.Grants = []v1.Grant{{Name: "LD_PRELOAD", Value: "/evil.so", As: v1.GrantEnv}}
	released := false
	x.Start(ctx, Claim{Connection: "hub", Run: run, Release: func() { released = true }})
	x.Wait()
	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunFailed || res.Error.Class != ClassRefused || !strings.Contains(res.Error.Message, "LD_") {
		t.Fatalf("result %+v %+v, %v", res, res.Error, ok)
	}
	if len(ad.Starts) != 0 || !released {
		t.Errorf("starts %d, released %v", len(ad.Starts), released)
	}
}

// A run is never started before its start time; one whose time has passed
// starts at once.
func TestStartAtIsHonoured(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
	}{
		{"in the future", 150 * time.Millisecond},
		{"already past", -time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			run := testRun("a", "s1")
			at := time.Now().Add(tc.delay)
			run.StartAt = &at
			e.enqueue(t, run)
			var (
				mu      sync.Mutex
				started time.Time
			)
			ad := &fake.Adapter{ID: "claude", Next: func(adapter.Spec) fake.Script {
				mu.Lock()
				defer mu.Unlock()
				started = time.Now()
				return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}}
			}}
			claimAndRun(t, l, e.executor(ad))
			mu.Lock()
			defer mu.Unlock()
			if started.IsZero() || started.Before(at) {
				t.Errorf("started at %v, start time %v", started, at)
			}
			if res, _ := outboxResult(t, e, "a"); res.State != v1.RunSucceeded {
				t.Errorf("result %+v", res)
			}
		})
	}
}

// grantLocks are the ways a grant directory can refuse a plain RemoveAll, as
// the run's cleanup and the sweep each meet them. The first is one yad gets
// past; the second is one nothing gets past, and must be said out loud.
var grantLocks = []struct {
	name       string
	frozen     bool
	lock       func(t *testing.T, dir, file string)
	wantSecret bool
	wantLog    string
}{
	{
		name: "a directory without its write bit",
		lock: func(t *testing.T, dir, _ string) { os.Chmod(dir, 0o500) },
	},
	{
		name:   "an immutable directory holding a read-only file",
		frozen: true,
		lock: func(t *testing.T, dir, file string) {
			os.Chmod(file, 0o400)
			freeze(t, dir)
		},
		wantSecret: true,
		wantLog:    "the secrets a hub sent are still on disk",
	},
}

// The sweep at the start of Serve goes through destroyGrants, so a directory
// whose write bit is gone no longer keeps a secret on disk from one start to
// the next — before DEV-76 it failed there exactly as the run's own cleanup
// had. When nothing gets through, the line says so, naming no grant: an
// fs.PathError from the removal names the file it could not unlink.
func TestTheSweepDestroysLeftGrants(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits these cases take away")
	}
	for _, tc := range grantLocks {
		t.Run(tc.name, func(t *testing.T) {
			if tc.frozen && !canFreeze {
				t.Skip("no immutable flag an unprivileged user may set on this OS")
			}
			e := newEnv(t)
			root := filepath.Join(e.paths.Data, "grants")
			dir := filepath.Join(root, "hub", "a")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, testGrantName)
			if err := os.WriteFile(file, []byte(testGrantValue), 0o600); err != nil {
				t.Fatal(err)
			}
			unlockTree(t, root)
			tc.lock(t, dir, file)

			var logged strings.Builder
			if err := Serve(context.Background(), Options{
				Paths: e.paths, Config: config.Default(), RunnerID: "r",
				Capabilities: func() v1.Capabilities { return drivableDoc("r", 1) },
				Log:          slog.New(slog.NewTextHandler(&logged, nil)), Drain: drained(),
			}); err != nil {
				t.Fatalf("Serve: %v", err)
			}
			got := logged.String()
			if left := secretLeft(t, root, testGrantValue); left != tc.wantSecret {
				t.Errorf("secret left on disk = %v, want %v", left, tc.wantSecret)
			}
			if tc.wantLog == "" && !strings.Contains(got, "deleted grant files an earlier run left behind") {
				t.Errorf("a sweep that deleted grants said nothing:\n%s", got)
			}
			if tc.wantLog != "" && !strings.Contains(got, tc.wantLog) {
				t.Errorf("a sweep that could not finish did not say %q:\n%s", tc.wantLog, got)
			}
			if strings.Contains(got, testGrantName) || strings.Contains(got, testGrantValue) {
				t.Errorf("the sweep's log names a grant:\n%s", got)
			}
		})
	}
}

// The cleanup at the end of every run goes through destroyGrants too. The
// lock is taken inside the run, while it still holds its grant file, which is
// how a harness that chmods its surroundings leaves it; before DEV-76 the
// secret outlived the run, and the sweep at the next start failed on it the
// same way.
func TestARunsCleanupDestroysItsGrants(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits these cases take away")
	}
	for _, tc := range grantLocks {
		t.Run(tc.name, func(t *testing.T) {
			if tc.frozen && !canFreeze {
				t.Skip("no immutable flag an unprivileged user may set on this OS")
			}
			e := newEnv(t)
			root := filepath.Join(e.paths.Data, "grants")
			// Registered before the run, so it runs after any freeze is
			// thawed and t.TempDir can remove what is left.
			unlockTree(t, root)
			l := e.loop(t, 1)
			run := testRun("a", "s1")
			run.Grants = []v1.Grant{{Name: testGrantName, Value: testGrantValue, As: v1.GrantFile}}
			e.enqueue(t, run)
			var (
				mu   sync.Mutex
				file string
			)
			ad := &fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
				mu.Lock()
				defer mu.Unlock()
				for _, kv := range s.Env {
					if p, ok := strings.CutPrefix(kv, testGrantName+"="); ok {
						file = p
					}
				}
				if file != "" {
					tc.lock(t, filepath.Dir(file), file)
				}
				return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}}
			}}
			var logged strings.Builder
			x := e.executor(ad)
			x.Log = slog.New(slog.NewTextHandler(&logged, nil))
			claimAndRun(t, l, x)

			mu.Lock()
			defer mu.Unlock()
			if file == "" {
				t.Fatal("the run was never given a file grant")
			}
			got := logged.String()
			if left := secretLeft(t, root, testGrantValue); left != tc.wantSecret {
				t.Errorf("secret left on disk = %v, want %v", left, tc.wantSecret)
			}
			if tc.wantLog == "" && strings.Contains(got, "grant files could not be removed") {
				t.Errorf("a cleanup that got through reported failing:\n%s", got)
			}
			if tc.wantLog != "" && !strings.Contains(got, tc.wantLog) {
				t.Errorf("a cleanup that could not finish did not say %q:\n%s", tc.wantLog, got)
			}
			if strings.Contains(got, testGrantName) || strings.Contains(got, testGrantValue) {
				t.Errorf("the cleanup's log names a grant:\n%s", got)
			}
		})
	}
}

// drained is a way down already under way, so a Serve with no connection
// returns as soon as it has started.
func drained() *Drain {
	d := NewDrain()
	d.Begin("test")
	return d
}
