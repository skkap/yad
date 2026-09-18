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
		Grace:  20 * time.Millisecond,
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
func (t *deafTurn) Interrupt() error        { return errors.New("not listening") }
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
		{"a grant that would set PATH", func(e *env) *Exec { return e.executor(fakeHarness(fake.Script{})) },
			[]v1.Grant{{Name: "PATH", Value: "/evil", As: v1.GrantEnv}}, ClassPrepare},
		{"a grant for the dynamic loader", func(e *env) *Exec { return e.executor(fakeHarness(fake.Script{})) },
			[]v1.Grant{{Name: "LD_PRELOAD", Value: "/evil.so", As: v1.GrantFile}}, ClassPrepare},
		{"a grant name that is a path", func(e *env) *Exec { return e.executor(fakeHarness(fake.Script{})) },
			[]v1.Grant{{Name: "../../x_TOKEN", Value: "v", As: v1.GrantFile}}, ClassPrepare},
		{"a grant for a proxy", func(e *env) *Exec { return e.executor(fakeHarness(fake.Script{})) },
			[]v1.Grant{{Name: "HTTPS_PROXY", Value: "https://attacker", As: v1.GrantEnv}}, ClassPrepare},
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

// Grants reach the harness in its environment or as 0600 files, never in the
// spec a hub could read back, and the files are gone when the run ends.
func TestGrantsAreDeliveredAndDestroyed(t *testing.T) {
	e := newEnv(t)
	l := e.loop(t, 1)
	run := testRun("a", "s1")
	run.Grants = []v1.Grant{
		{Name: "ZUMINO_TOKEN", Value: "env-secret", As: v1.GrantEnv},
		{Name: "DEPLOY_KEY", Value: "file-secret", As: v1.GrantFile},
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
			if path, ok := strings.CutPrefix(kv, "DEPLOY_KEY="); ok {
				b, _ := os.ReadFile(path)
				fi, _ := os.Stat(path)
				fileBody, fileMode = string(b), fi.Mode().Perm()
			}
		}
		return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}}
	}}
	claimAndRun(t, l, e.executor(ad))

	mu.Lock()
	defer mu.Unlock()
	if !contains(seen.Env, "ZUMINO_TOKEN=env-secret") {
		t.Errorf("env grant missing from %v", seen.Env)
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
	for _, id := range []string{"..", ".", "../../.ssh", "a/b", "", ".hidden", strings.Repeat("x", 65), "_abc"} {
		got := pathName(id)
		if strings.ContainsAny(got, "/\\") || got == "." || got == ".." || !strings.HasPrefix(got, "_") {
			t.Errorf("pathName(%q) = %q", id, got)
		}
	}
	for _, id := range []string{"s1", "ZUM-19", "run.2026-09-19"} {
		if got := pathName(id); got != id {
			t.Errorf("pathName(%q) = %q, want it kept", id, got)
		}
	}
	if pathName("../a") == pathName("../b") {
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

// A grant is named as the secret it is. Everything that steers a harness —
// the loader, proxies, CA bundles, shell options, runtime hooks, the harness's
// own billing keys — is refused, whether or not anybody thought to list it.
func TestGrantNames(t *testing.T) {
	for name, ok := range map[string]bool{
		"ZUMINO_TOKEN": true, "GH_TOKEN": true, "DEPLOY_KEY": true, "DB_PASSWORD": true,
		"AWS_SECRET": true, "GCP_CREDENTIALS": true, "SERVICE_CREDENTIAL": true,
		"PATH": false, "HOME": false, "LD_PRELOAD": false, "DYLD_INSERT_LIBRARIES": false,
		"HTTPS_PROXY": false, "HTTP_PROXY": false, "ALL_PROXY": false, "NO_PROXY": false,
		"NODE_EXTRA_CA_CERTS": false, "NODE_TLS_REJECT_UNAUTHORIZED": false, "SSL_CERT_FILE": false,
		"REQUESTS_CA_BUNDLE": false, "CURL_CA_BUNDLE": false, "SHELLOPTS": false, "PS4": false,
		"BASH_ENV": false, "GCONV_PATH": false, "JAVA_TOOL_OPTIONS": false, "_JAVA_OPTIONS": false,
		"NODE_OPTIONS": false, "NODE_AUTH_TOKEN": false, "ANTHROPIC_API_KEY": false, "OPENAI_API_KEY": false,
		"CLAUDE_CODE_OAUTH_TOKEN": false, "CODEX_API_KEY": false, "YAD_TOKEN": false, "GIT_TOKEN": false,
		"LD_TOKEN": false, "zumino_token": false, "_TOKEN": false, "TOKEN": false, "A_TOKEN_X": false,
		"NPM_CONFIG__AUTH_TOKEN": false, "BUN_AUTH_TOKEN": false,
	} {
		if got := grantAllowed(name); got != ok {
			t.Errorf("grantAllowed(%q) = %v, want %v", name, got, ok)
		}
	}
}
