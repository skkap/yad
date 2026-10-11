package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/skkap/yad/internal/store/db"
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
	// Bound before anything was made on disk: a runner that died here would
	// come back to a session that knows its sources.
	if src := session(t, e, "s1").Sources; !src.Valid || !strings.Contains(src.String, "acme.git") {
		t.Errorf("the session's sources while its first run is still preparing: %+v", src)
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

// A credential in a source's URL is the run's, not the session's (decision
// 0068): the run's spec and the session's sources are stored without it, a
// continuation sending the same URL with another credential, or none, names
// the same sources, and the refusal of one naming others names the session's
// without it — the refusal goes to the hub and the daemon's log (decision
// 0064). file://…@localhost is a URL with userinfo that reaches a repository
// on this machine, so no network.
func TestASourcesCredentialIsNotTheSessions(t *testing.T) {
	e := newEnv(t)
	root := t.TempDir()
	bare := gitRepo(t, root, "#!/bin/sh\n")
	l := e.loop(t, 1)
	h := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"}})
	x := e.sourcedExec(root, h)

	const token = "ghp_FAKEt0kenFAKEt0ken"
	withToken := func(tok string) []v1.Source {
		return []v1.Source{{Git: &v1.GitSource{URL: "file://" + tok + "@localhost" + bare}}}
	}
	first := testRun("a", "s1")
	first.Sources = withToken(token)
	runOne(t, e, l, x, first)
	if r := hubResult(t, e, "a"); r.State != v1.RunSucceeded {
		t.Fatalf("first run: %+v (error %+v)", r, r.Error)
	}
	if s := session(t, e, "s1").Sources.String; strings.Contains(s, "FAKEt0ken") || !strings.Contains(s, "file://localhost"+bare) {
		t.Errorf("the session's sources are recorded as %s", s)
	}
	row, err := e.store.GetRun(context.Background(), dbRun("hub", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(row.Spec, "FAKEt0ken") || row.HadGrants == 0 {
		t.Errorf("the run is stored as %s, had_grants %d; want no token, and a run a later process cannot rebuild", row.Spec, row.HadGrants)
	}

	for i, src := range [][]v1.Source{withToken("ghp_another"), {{Git: &v1.GitSource{URL: "file://localhost" + bare}}}} {
		id := fmt.Sprintf("same%d", i)
		same := continued(id, "s1")
		same.Sources = src
		runOne(t, e, l, x, same)
		if r := hubResult(t, e, id); r.State != v1.RunSucceeded {
			t.Errorf("a continuation naming the same repository with another credential or none: %+v (error %+v)", r, r.Error)
		}
	}

	changed := continued("b", "s1")
	changed.Sources = []v1.Source{{Path: root}}
	runOne(t, e, l, x, changed)
	r := hubResult(t, e, "b")
	if r.Error == nil || r.Error.Class != workdir.ClassSourceRefused {
		t.Fatalf("a continuation naming other sources: %+v", r)
	}
	if strings.Contains(r.Error.Message, "FAKEt0ken") || !strings.Contains(r.Error.Message, "file://localhost") {
		t.Errorf("message = %q, want the session's source named without its token", r.Error.Message)
	}
}

// A session is bound to its first run's sources whatever became of that run:
// a continuation after a failed setup hook runs the hook again in the same
// worktree, and a session begun with no sources refuses one named later.
func TestASessionIsBoundByItsFirstRun(t *testing.T) {
	e := newEnv(t)
	root := t.TempDir()
	ready := filepath.Join(t.TempDir(), "ready")
	bare := gitRepo(t, root, "#!/bin/sh\n[ -e "+ready+" ] || { echo 'not yet' >&2; exit 1; }\ntouch set-up\n")
	l := e.loop(t, 1)
	h := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"}})
	x := e.sourcedExec(root, h)

	first := testRun("a", "s1")
	first.Sources = []v1.Source{{Git: &v1.GitSource{URL: bare}}}
	runOne(t, e, l, x, first)
	if r := hubResult(t, e, "a"); r.Error == nil || r.Error.Class != workdir.ClassSetupFailed {
		t.Fatalf("first run: %+v", r)
	}
	os.WriteFile(ready, nil, 0o600)
	runOne(t, e, l, x, continued("b", "s1"))
	if r := hubResult(t, e, "b"); r.State != v1.RunSucceeded {
		t.Fatalf("the continuation after a failed hook: %+v", r)
	}
	dir := session(t, e, "s1").Workdir
	if _, err := os.Stat(filepath.Join(dir, "set-up")); err != nil || len(h.Starts) != 1 || h.Starts[0].Workdir != dir {
		t.Errorf("the hook did not run again in the session's worktree before the harness: %v, starts %v", err, h.Starts)
	}

	runOne(t, e, l, x, testRun("c", "s2"))
	later := continued("d", "s2")
	later.Sources = []v1.Source{{Path: root}}
	runOne(t, e, l, x, later)
	if r := hubResult(t, e, "d"); r.Error == nil || r.Error.Class != workdir.ClassSourceRefused {
		t.Errorf("a source named after a session began with none: %+v", r)
	}
	if len(h.Starts) != 2 {
		t.Errorf("%d starts; the refused run must not start the harness", len(h.Starts))
	}
}

// With no [workdirs] roots configured, a path source under the owner's home
// directory is taken — where decision 0033 refused every path source, 0038
// makes the home directory the root. Outside it is still refused, and a runner
// with no home to resolve reaches nothing rather than everything. With
// path_sources = false even the home directory is refused, and the result says
// which setting refused it (0062).
func TestPathSourceDefaultsToTheOwnersHome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inHome  bool // the source is under the home directory
		noHome  bool // the runner has no home directory
		off     bool // the owner set path_sources = false
		started bool
	}{
		{name: "under the owner's home", inHome: true, started: true},
		{name: "outside the owner's home", inHome: false, started: false},
		{name: "a runner with no home directory", inHome: true, noHome: true, started: false},
		{name: "under the owner's home, with path sources off", inHome: true, off: true, started: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, elsewhere := t.TempDir(), t.TempDir()
			dir := filepath.Join(elsewhere, "project")
			if tc.inHome {
				dir = filepath.Join(home, "project")
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.noHome {
				home = ""
			}
			t.Setenv("HOME", home)
			e := newEnv(t)
			l := e.loop(t, 1)
			h := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}})
			run := testRun("a", "s1")
			run.Sources = []v1.Source{{Path: dir}}
			// The owner has configured no roots: config.Default() is what the
			// executor builds its workdir manager from.
			x := e.executor(h)
			if len(x.Config.Workdirs.Roots) != 0 {
				t.Fatalf("the test's own config lists roots: %v", x.Config.Workdirs.Roots)
			}
			if tc.off {
				x.Config.Workdirs.PathSources = new(false)
			}
			runOne(t, e, l, x, run)

			r := hubResult(t, e, "a")
			if !tc.started {
				if r.State != v1.RunFailed || r.Error == nil || r.Error.Class != workdir.ClassSourceRefused {
					t.Fatalf("result %+v (error %+v), want the source refused", r, r.Error)
				}
				if tc.off && !strings.Contains(r.Error.Message, "path_sources = false") {
					t.Errorf("the refusal %q does not name the setting that refused it", r.Error.Message)
				}
				if len(h.Starts) != 0 {
					t.Errorf("the harness started for a refused source: %v", h.Starts)
				}
				return
			}
			if r.State != v1.RunSucceeded {
				t.Fatalf("result %+v (error %+v)", r, r.Error)
			}
			real, _ := filepath.EvalSymlinks(dir)
			if len(h.Starts) != 1 || !strings.HasPrefix(h.Starts[0].Workdir, real) {
				t.Errorf("the harness ran in %v, not in the path source %s", h.Starts, real)
			}
		})
	}
}

// gitIn is a harness's git in its workdir. Called from the fake harness, so
// it reports rather than stops.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Errorf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// branchIn is the branch a workdir has checked out; empty when detached.
func branchIn(dir string) string {
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// The ticket's case end to end (decision 0081): a run in s1 pushes branch
// work and ends; a run opening s2 names work, starts on s1's commit, pushes
// its own; s1's next run is back on work with s2's commit, and is told the
// branch moved.
func TestASecondSessionTakesABranchTheFirstLeftPushed(t *testing.T) {
	e := newEnv(t)
	root := t.TempDir()
	bare := gitRepo(t, root, "#!/bin/sh\n")
	l := e.loop(t, 1)
	src := []v1.Source{{Git: &v1.GitSource{URL: bare, Branch: "work"}}}

	var found []string
	h := &fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
		var names []string
		entries, _ := os.ReadDir(s.Workdir)
		for _, en := range entries {
			if strings.HasPrefix(en.Name(), "run-") {
				names = append(names, en.Name())
			}
		}
		found = append(found, strings.Join(names, " "))
		name := fmt.Sprintf("run-%d.txt", len(found))
		os.WriteFile(filepath.Join(s.Workdir, name), []byte(name), 0o600)
		gitIn(t, s.Workdir, "add", name)
		gitIn(t, s.Workdir, "commit", "--quiet", "-m", name)
		gitIn(t, s.Workdir, "push", "--quiet", "origin", "work")
		return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-" + filepath.Base(s.Workdir)}}
	}}
	x := e.sourcedExec(root, h)
	first := testRun("a", "s1")
	first.Sources = src
	runOne(t, e, l, x, first)
	second := testRun("b", "s2")
	second.Sources = src
	runOne(t, e, l, x, second)
	third := continued("c", "s1")
	third.Sources = src
	runOne(t, e, l, x, third)

	for _, id := range []string{"a", "b", "c"} {
		if r := localRun(t, e, id); r.State != string(v1.RunSucceeded) {
			t.Errorf("run %s ended %s: %s", id, r.State, r.Reason.String)
		}
	}
	if want := []string{"", "run-1.txt", "run-1.txt run-2.txt"}; strings.Join(found, "|") != strings.Join(want, "|") {
		t.Errorf("the runs found %q, want %q", found, want)
	}
	var statuses []string
	for _, ev := range hubEvents(t, e, "c") {
		if ev.Kind == v1.EventStatus {
			statuses = append(statuses, ev.Status)
		}
	}
	if got := strings.Join(statuses, "\n"); !strings.Contains(got, "back on work, which moved from") {
		t.Errorf("s1's second run was not told the branch moved:\n%s", got)
	}
}

// A run that parks keeps its branch: it comes back to the tree it left. One
// a restart reports lost lets go of it, as a run that ends does.
func TestAParkedRunKeepsItsBranchAndALostOneLetsGo(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plantCredential(t, e.paths.Data, "work")
	root := t.TempDir()
	bare := gitRepo(t, root, "#!/bin/sh\n")
	reset := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	l := e.loop(t, 1)
	run := testRun("a", "s1")
	run.Sources = []v1.Source{{Git: &v1.GitSource{URL: bare, Branch: "work"}}}
	e.enqueue(t, run)
	h := &fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
		gitIn(t, s.Workdir, "push", "--quiet", "origin", "work")
		return limitScript("five_hour", reset, "native-1")
	}}
	x, _ := e.accountExecutor(t, accountConfig("work"), h)
	x.Config.Workdirs.Roots = []string{root}
	claimAndRun(t, l, x)

	waitingRow(t, e, "a")
	dir := session(t, e, "s1").Workdir
	if b := branchIn(dir); b != "work" {
		t.Fatalf("a parked run's workdir is on %q, want its branch", b)
	}

	// The process dies with the run in hand, and the next start reports it
	// lost.
	if _, err := e.store.DB.ExecContext(ctx, `UPDATE runs SET state = 'running' WHERE connection = 'hub' AND id = 'a'`); err != nil {
		t.Fatal(err)
	}
	l2 := restartedAs(t, e, l, x)
	// Settled while the run is still held, so a runner stopping between the
	// two finds it again at its next start rather than never.
	var heldAtSettle string
	l2.Settle = func(ctx context.Context, dir string) error {
		heldAtSettle = localRun(t, e, "a").State
		return x.Workdirs.Settle(ctx, dir)
	}
	if err := l2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if res, ok := outboxResult(t, e, "a"); !ok || res.State != v1.RunLost {
		t.Fatalf("result = %+v, %v, want lost", res, ok)
	}
	if b := branchIn(dir); b != "" {
		t.Errorf("a lost run's workdir is still on %q, which it left pushed and clean", b)
	}
	if heldAtSettle != string(v1.RunRunning) {
		t.Errorf("the branch was let go of with the run %q; want it still held, so a restart in between retries it", heldAtSettle)
	}
}

// A parked run ended from outside it — the collector past its max_wait, a
// cancel — while a sync is resuming it is refused by End's own check, and
// its branch stays where the resumed harness is working on it.
func TestEndingARunNoLongerWaitingLeavesItsBranch(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	bare := gitRepo(t, root, "#!/bin/sh\n")
	l := e.loop(t, 1)
	run := testRun("a", "s1")
	run.Sources = []v1.Source{{Git: &v1.GitSource{URL: bare, Branch: "work"}}}
	x := e.sourcedExec(root, fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"}}))
	runOne(t, e, l, x, run)
	dir := session(t, e, "s1").Workdir
	// Unpushed at its end, so kept; pushed now, as a resumed harness would.
	gitIn(t, dir, "push", "--quiet", "origin", "work")
	if _, err := e.store.DB.ExecContext(ctx, `UPDATE runs SET state = 'running' WHERE connection = 'hub' AND id = 'a'`); err != nil {
		t.Fatal(err)
	}

	c := Claim{Connection: "hub", Release: func() {}, Run: v1.Run{RunID: "a", Harness: "claude", Session: v1.SessionRef{ID: "s1"}}}
	if err := x.End(ctx, c, localRun(t, e, "a"), v1.RunTimedOut, &v1.RunError{Class: ClassMaxWait, Message: "past max_wait"}, time.Now()); !errors.Is(err, errNoLongerWaiting) {
		t.Fatalf("End = %v, want it refused", err)
	}
	if b := branchIn(dir); b != "work" {
		t.Errorf("a run being resumed was taken off its branch: on %q", b)
	}
}

// A parked run ended from outside it is settled once its result is
// recorded, when the reporter may already have delivered it and the
// session's next run been claimed: that run keeps the branch. With none, the
// branch is let go of as at any run's end.
func TestASettleAfterTheResultLeavesTheBranchToTheSessionsNextRun(t *testing.T) {
	for _, next := range []bool{true, false} {
		t.Run(fmt.Sprintf("next run claimed %v", next), func(t *testing.T) {
			e := newEnv(t)
			ctx := context.Background()
			root := t.TempDir()
			bare := gitRepo(t, root, "#!/bin/sh\n")
			l := e.loop(t, 1)
			run := testRun("a", "s1")
			run.Sources = []v1.Source{{Git: &v1.GitSource{URL: bare, Branch: "work"}}}
			x := e.sourcedExec(root, fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, NativeSessionID: "native-1"}}))
			runOne(t, e, l, x, run)
			dir := session(t, e, "s1").Workdir
			// Unpushed at its end, so kept; pushed since.
			gitIn(t, dir, "push", "--quiet", "origin", "work")
			if next {
				if err := e.store.CreateRun(ctx, db.CreateRunParams{Connection: "hub", ID: "b", SessionID: "s1", Harness: "claude", Model: "opus", Spec: "{}", CreatedAt: 1, UpdatedAt: 1}); err != nil {
					t.Fatal(err)
				}
			}
			x.settle(ctx, Claim{Connection: "hub", Run: v1.Run{RunID: "a", Session: v1.SessionRef{ID: "s1"}}}, true)
			want := ""
			if next {
				want = "work"
			}
			if b := branchIn(dir); b != want {
				t.Errorf("on %q, want %q", b, want)
			}
		})
	}
}
