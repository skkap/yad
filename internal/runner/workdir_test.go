package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	hubdb "github.com/skkap/yad/internal/hub/store/db"
	"github.com/skkap/yad/internal/workdir"
)

// gitRepo makes a bare repository under root whose main branch holds one
// executable .worktree/setup with the given body. git reads none of the
// person's own config.
func gitRepo(t *testing.T, root, hook string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
	} {
		t.Setenv(k, v)
	}
	bare, work := filepath.Join(root, "acme.git"), t.TempDir()
	if err := os.MkdirAll(filepath.Join(work, ".worktree"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".worktree", "setup"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", "--initial-branch=main", bare},
		{"-C", work, "init", "--quiet", "--initial-branch=main"},
		{"-C", work, "add", "-A"},
		{"-C", work, "commit", "--quiet", "-m", "initial"},
		{"-C", work, "push", "--quiet", bare, "main"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return bare
}

// sourcedExec is an executor whose owner allows root.
func (e *env) sourcedExec(root string, ad adapter.Adapter) *Exec {
	x := e.executor(ad)
	x.Config.Workdirs.Roots = []string{root}
	return x
}

func hubEvents(t *testing.T, e *env, runID string) []v1.Event {
	t.Helper()
	rows, err := e.hubStore.EventsAfter(context.Background(), hubdb.EventsAfterParams{RunID: runID, Seq: 0, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var evs []v1.Event
	for i, r := range rows {
		var ev v1.Event
		if err := json.Unmarshal([]byte(r.Body), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: the stream has a gap", i, ev.Seq)
		}
		evs = append(evs, ev)
	}
	return evs
}

// A git source is checked out and set up before the harness starts, in the
// harness's working directory; what preparing did is the start of the run's
// stream, numbered on into the harness's own events.
func TestExecutorPreparesSources(t *testing.T) {
	e := newEnv(t)
	root := t.TempDir()
	bare := gitRepo(t, root, "#!/bin/sh\necho \"slot $WT_SLOT\" > note.txt\necho prepared\n")
	l := e.loop(t, 1)
	run := testRun("a", "s1")
	run.Sources = []v1.Source{{Git: &v1.GitSource{URL: bare, Branch: "work"}}}
	e.enqueue(t, run)

	var note string
	ad := &fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
		b, _ := os.ReadFile(filepath.Join(s.Workdir, "note.txt"))
		note = string(b)
		return fake.Script{Events: []v1.Event{{Kind: v1.EventText, Text: "read it"}},
			Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "ok"}}
	}}
	claimAndRun(t, l, e.sourcedExec(root, ad))

	if note != "slot 1\n" {
		t.Errorf("the harness found %q: the hook had not run in its workdir", note)
	}
	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunSucceeded {
		t.Fatalf("result %+v", res)
	}
	e.reporter(l).Flush(context.Background())
	evs := hubEvents(t, e, "a")
	if int64(len(evs)) != res.LastSeq {
		t.Errorf("last_seq %d, %d events", res.LastSeq, len(evs))
	}
	var kinds []string
	for _, ev := range evs {
		kinds = append(kinds, string(ev.Kind))
	}
	if got := strings.Join(kinds, " "); got != "status status tool_call tool_result text" {
		t.Errorf("event kinds %q: fetch, worktree, hook call and result, then the harness", got)
	}
	if evs[3].Tool == nil || evs[3].Tool.Output != "prepared\n" {
		t.Errorf("the hook's output: %+v", evs[3].Tool)
	}
}

// A preparation that fails is the run's result, with its own class, and the
// harness never starts.
func TestExecutorPrepareFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hook  string
		roots bool
		src   func(bare string) v1.Source
		class string
	}{
		{"setup hook fails", "#!/bin/sh\necho 'no database' >&2\nexit 1\n", true,
			func(bare string) v1.Source { return v1.Source{Git: &v1.GitSource{URL: bare}} }, workdir.ClassSetupFailed},
		{"local repository with no roots allowed", "#!/bin/sh\n", false,
			func(bare string) v1.Source { return v1.Source{Git: &v1.GitSource{URL: bare}} }, workdir.ClassSourceRefused},
		{"branch that is an option", "#!/bin/sh\n", true,
			func(bare string) v1.Source { return v1.Source{Git: &v1.GitSource{URL: bare, Branch: "--orphan"}} }, workdir.ClassSourceRefused},
		{"base that is not there", "#!/bin/sh\n", true,
			func(bare string) v1.Source { return v1.Source{Git: &v1.GitSource{URL: bare, Base: "nope"}} }, workdir.ClassSourceFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			root := t.TempDir()
			bare := gitRepo(t, root, tc.hook)
			l := e.loop(t, 1)
			run := testRun("a", "s1")
			run.Sources = []v1.Source{tc.src(bare)}
			e.enqueue(t, run)
			ad := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}})
			x := e.executor(ad)
			if tc.roots {
				x = e.sourcedExec(root, ad)
			}
			claimAndRun(t, l, x)
			res, ok := outboxResult(t, e, "a")
			if !ok || res.State != v1.RunFailed || res.Error == nil || res.Error.Class != tc.class {
				t.Fatalf("result %+v (error %+v)", res, res.Error)
			}
			if len(ad.Starts) != 0 {
				t.Error("the harness started in a workdir that was not prepared")
			}
			e.reporter(l).Flush(context.Background())
			if n := len(hubEvents(t, e, "a")); int64(n) != res.LastSeq {
				t.Errorf("last_seq %d, %d events", res.LastSeq, n)
			}
			if l.Pool.Free() != 1 {
				t.Error("capacity not released")
			}
		})
	}
}

// A run is preparing, not running, while its setup hook runs (Multica
// #3999), and a cancel reaches it there: the hook is stopped and the run
// ends cancelled without a harness.
func TestCancelWhilePreparing(t *testing.T) {
	e := newEnv(t)
	root := t.TempDir()
	started := filepath.Join(t.TempDir(), "hook-started")
	bare := gitRepo(t, root, "#!/bin/sh\ntouch "+started+"\nsleep 60\n")
	l := e.loop(t, 1)
	run := testRun("a", "s1")
	run.Sources = []v1.Source{{Git: &v1.GitSource{URL: bare}}}
	e.enqueue(t, run)
	ad := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}})
	x := e.sourcedExec(root, ad)
	l.Executor = x
	mustSync(t, l)
	mustSync(t, l)

	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the setup hook never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := localRun(t, e, "a").State; st != string(v1.RunPreparing) {
		t.Errorf("state while the hook runs = %s, want preparing", st)
	}
	begin := time.Now()
	x.Control(context.Background(), "hub", v1.Control{Kind: v1.ControlCancel, RunID: "a"})
	x.Wait()
	if d := time.Since(begin); d > 10*time.Second {
		t.Errorf("the cancel took %s to reach a run in its setup hook", d)
	}
	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunCancelled || res.Metrics.CancelLatencyMS == nil {
		t.Fatalf("result %+v", res)
	}
	if len(ad.Starts) != 0 {
		t.Error("the harness started after the cancel")
	}
}

// A continued session runs in the worktree its first run was given, on the
// same branch, with that run's commit and its uncommitted work in place:
// nothing is fetched, no second worktree is added, and the setup hook does
// not run again.
func TestAContinuedSessionReusesItsWorktree(t *testing.T) {
	e := newEnv(t)
	root := t.TempDir()
	hookRuns := filepath.Join(t.TempDir(), "hook-runs")
	bare := gitRepo(t, root, "#!/bin/sh\necho run >> "+hookRuns+"\n")
	l := e.loop(t, 1)
	src := []v1.Source{{Git: &v1.GitSource{URL: bare, Branch: "work"}}}

	var firstCommit string
	h := &fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
		if firstCommit == "" {
			// The first run's work: one commit, and one file left uncommitted.
			os.WriteFile(filepath.Join(s.Workdir, "done.txt"), []byte("done"), 0o600)
			for _, args := range [][]string{{"add", "done.txt"}, {"commit", "--quiet", "-m", "first run"}} {
				if out, err := exec.Command("git", append([]string{"-C", s.Workdir}, args...)...).CombinedOutput(); err != nil {
					t.Errorf("git %v: %v %s", args, err, out)
				}
			}
			out, _ := exec.Command("git", "-C", s.Workdir, "rev-parse", "HEAD").Output()
			firstCommit = strings.TrimSpace(string(out))
			os.WriteFile(filepath.Join(s.Workdir, "wip.txt"), []byte("wip"), 0o600)
		}
		return fake.Script{Events: []v1.Event{{Kind: v1.EventText, Text: "ok"}},
			Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"}}
	}}
	x := e.sourcedExec(root, h)
	first := testRun("a", "s1")
	first.Sources = src
	runOne(t, e, l, x, first)
	second := continued("b", "s1")
	second.Sources = src
	runOne(t, e, l, x, second)

	if len(h.Starts) != 2 {
		t.Fatalf("%d starts", len(h.Starts))
	}
	dir := session(t, e, "s1").Workdir
	if h.Starts[0].Workdir != dir || h.Starts[1].Workdir != dir {
		t.Errorf("runs started in %s and %s; want both in the session's workdir %s", h.Starts[0].Workdir, h.Starts[1].Workdir, dir)
	}
	git := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	if b := git("branch", "--show-current"); b != "work" {
		t.Errorf("the second run is on %q", b)
	}
	if head := git("rev-parse", "HEAD"); head != firstCommit {
		t.Errorf("HEAD is %s, not the first run's commit %s", head, firstCommit)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "wip.txt")); err != nil || string(b) != "wip" {
		t.Errorf("the first run's uncommitted work: %q, %v", b, err)
	}
	if n := strings.Count(git("worktree", "list", "--porcelain"), "worktree "); n != 2 {
		t.Errorf("the cache lists %d worktrees; want itself and the session's one", n)
	}
	if b, _ := os.ReadFile(hookRuns); string(b) != "run\n" {
		t.Errorf("the setup hook ran %q times over two runs", b)
	}
	var statuses []string
	for _, ev := range hubEvents(t, e, "b") {
		if ev.Kind == v1.EventStatus {
			statuses = append(statuses, ev.Status)
		}
	}
	if got := strings.Join(statuses, "\n"); !strings.Contains(got, "continuing in the session's worktree") || strings.Contains(got, "fetching") {
		t.Errorf("the second run said: %s", got)
	}
}

// A session keeps the sources its workdir was built from: a continuing run
// that names none runs in the same path, locked again, and one naming others
// is refused.
func TestASessionKeepsItsSources(t *testing.T) {
	e := newEnv(t)
	root := t.TempDir()
	dir := filepath.Join(root, "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(dir)
	other := filepath.Join(root, "other")
	os.MkdirAll(other, 0o755)
	l := e.loop(t, 1)
	h := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"}})
	x := e.sourcedExec(root, h)

	first := testRun("a", "s1")
	first.Sources = []v1.Source{{Path: dir}}
	runOne(t, e, l, x, first)
	runOne(t, e, l, x, continued("b", "s1"))
	if len(h.Starts) != 2 || h.Starts[1].Workdir != real {
		t.Fatalf("a continuation naming no sources started in %v; want the session's path %s", h.Starts, real)
	}

	changed := continued("c", "s1")
	changed.Sources = []v1.Source{{Path: other}}
	runOne(t, e, l, x, changed)
	if r := hubResult(t, e, "c"); r.State != v1.RunFailed || r.Error == nil || r.Error.Class != workdir.ClassSourceRefused {
		t.Errorf("a continuation naming other sources: %+v", r)
	}
	if len(h.Starts) != 2 {
		t.Error("the harness started for a run whose sources were refused")
	}
	same := continued("d", "s1")
	same.Sources = []v1.Source{{Path: dir}}
	runOne(t, e, l, x, same)
	if len(h.Starts) != 3 || h.Starts[2].Workdir != real {
		t.Errorf("a continuation naming the same sources: %v", h.Starts)
	}
}
