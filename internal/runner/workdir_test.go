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
