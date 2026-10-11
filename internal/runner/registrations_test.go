package runner

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// serving is Serve on a profile until stopped, as the daemon runs it, with a
// harness whose every turn succeeds.
func serving(t *testing.T, p config.Paths) (stop func()) {
	t.Helper()
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	id, err := p.RunnerID()
	if err != nil {
		t.Fatal(err)
	}
	d := NewDrain()
	done := make(chan error, 1)
	go func() {
		done <- Serve(context.Background(), Options{Paths: p, Config: cfg, RunnerID: id,
			Capabilities: func() v1.Capabilities { return drivableDoc(id, 1) },
			Adapters: NewRegistry(&fake.Adapter{ID: "claude", Next: func(adapter.Spec) fake.Script {
				return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}}
			}}),
			Log: slog.New(slog.DiscardHandler), Monitor: NewMonitor(), Drain: d})
	}()
	var once bool
	stop = func() {
		t.Helper()
		if once {
			return
		}
		once = true
		d.Stop("test")
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Serve did not return after a stop")
		}
	}
	t.Cleanup(stop)
	return stop
}

// localSession reads a session as the runner holds it, under conn.
func localSession(t *testing.T, s *store.Store, conn, id string) (db.Session, bool) {
	t.Helper()
	sess, err := s.GetSession(context.Background(), db.GetSessionParams{Connection: conn, ID: id})
	return sess, err == nil
}

// retiredNames are the connection names the store holds rows under that
// a retirement made.
func retiredNames(t *testing.T, s *store.Store) []string {
	t.Helper()
	rows, err := s.ListSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		if strings.HasPrefix(r.Connection, "home"+store.RetiredSeparator) {
			out = append(out, r.Connection)
		}
	}
	return out
}

// After `yad disconnect`, the name comes free, and a different hub connected
// under it may pick the ids the first one used — a hub with sequential ids
// does on its first run. Its run is accepted, and the first hub's rows stay
// under a retired name. A restart without a new registration moves nothing
// (DEV-166, decision 0079).
func TestANameReusedForAnotherHubStartsClean(t *testing.T) {
	noTools(t)
	first, second := newEnv(t), newEnv(t)
	p := first.paths
	ctx := context.Background()

	if _, _, _, err := Connect(ctx, p, first.url, first.token(t), "home"); err != nil {
		t.Fatal(err)
	}
	// After the connect, which probes the machine: the runs need a harness
	// binary to be found, and the fake adapter never starts it.
	t.Setenv("YAD_CLAUDE_PATH", "/bin/sh")
	stop := serving(t, p)
	first.enqueue(t, testRun("run-1", "ses-1"))
	eventually(t, "the first hub's run succeeds", func() bool { return first.hubState(t, "run-1") == string(v1.RunSucceeded) })
	old, ok := localSession(t, first.store, "home", "ses-1")
	if !ok || old.Workdir == "" {
		t.Fatalf("the first hub's session %+v has no workdir", old)
	}
	stop()

	if _, err := Disconnect(ctx, p, "home", false, "test"); err != nil {
		t.Fatal(err)
	}
	// No daemon was told: its next start ends what the connection left, which
	// is what lets `yad connect` give the name out again.
	stop = serving(t, p)
	eventually(t, "the removed connection's session is closed, owed to nobody and reclaimed", func() bool {
		s, ok := localSession(t, first.store, "home", "ses-1")
		return ok && s.State == "closed" && s.ReportedAt.Valid && s.ReclaimedAt.Valid
	})
	stop()

	t.Setenv("YAD_CLAUDE_PATH", "")
	if _, _, _, err := Connect(ctx, p, second.url, second.token(t), "home"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAD_CLAUDE_PATH", "/bin/sh")
	stop = serving(t, p)
	second.enqueue(t, testRun("run-1", "ses-1"))
	eventually(t, "the second hub's run, reusing the first's ids, succeeds", func() bool {
		return second.hubState(t, "run-1") == string(v1.RunSucceeded)
	})
	stop()

	retired := retiredNames(t, first.store)
	if len(retired) != 1 {
		t.Fatalf("retired names %v, want the first hub's one", retired)
	}
	was, ok := localSession(t, first.store, retired[0], "ses-1")
	if !ok || was.CreatedAt != old.CreatedAt || !was.ReclaimedAt.Valid {
		t.Errorf("under %s the first hub's session is %+v (%v), want it kept as it was, reclaimed", retired[0], was, ok)
	}
	if _, err := first.store.GetRun(ctx, db.GetRunParams{Connection: retired[0], ID: "run-1"}); err != nil {
		t.Errorf("the first hub's run is not kept under %s: %v", retired[0], err)
	}
	now, ok := localSession(t, first.store, "home", "ses-1")
	if !ok || now.CreatedAt == old.CreatedAt {
		t.Errorf("under the reused name the session is %+v (%v), want the second hub's", now, ok)
	}
	if _, err := os.Stat(now.Workdir); err != nil {
		t.Errorf("the second hub's session lost its workdir: %v", err)
	}

	// A restart is not a new registration.
	stop = serving(t, p)
	stop()
	if got := retiredNames(t, first.store); len(got) != 1 {
		t.Errorf("a restart retired again: %v", got)
	}
	if _, ok := localSession(t, first.store, "home", "ses-1"); !ok {
		t.Error("a restart moved the second hub's session")
	}
}

// A new session's workdir is never one an earlier registration's session
// recorded and has not reclaimed: the collector deletes that one by its path,
// and would take the new session's files with it.
func TestANewSessionDoesNotTakeARetiredSessionsWorkdir(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	x := e.executor()
	held := filepath.Join(e.paths.Data, "workdirs", "hub", "s1")
	if err := os.MkdirAll(held, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, s := range []db.CreateSessionParams{
		{Connection: "hub~1", ID: "s1", Harness: "claude", Workdir: held, CreatedAt: 1, LastUsedAt: 1},
		{Connection: "hub", ID: "s1", Harness: "claude", CreatedAt: 2, LastUsedAt: 2},
	} {
		if err := e.store.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	c := Claim{Connection: "hub", Run: testRun("r1", "s1")}
	dir, _, _, err := x.workdir(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if dir != held+"~1" || retakeOf(dir, "s1") != "1" {
		t.Errorf("the new session's workdir is %s (mark %q); the retired session still holds %s", dir, retakeOf(dir, "s1"), held)
	}
	// Reclaimed or not, the retired session's branches are still in the
	// caches under the plain name, so a third registration's s1 moves on.
	if err := e.store.SetSessionReclaimed(ctx, db.SetSessionReclaimedParams{ReclaimedAt: sql.NullInt64{Int64: 5, Valid: true}, Connection: "hub~1", ID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub~2", ID: "s1", Harness: "claude", Workdir: dir, CreatedAt: 4, LastUsedAt: 4}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DB.Exec(`DELETE FROM sessions WHERE connection = 'hub' AND id = 's1'`); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "s1", Harness: "claude", CreatedAt: 6, LastUsedAt: 6}); err != nil {
		t.Fatal(err)
	}
	third, _, _, err := x.workdir(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if third != held+"~2" || retakeOf(third, "s1") != "2" {
		t.Errorf("a third registration's s1 has workdir %s; the two before it took %s and %s", third, held, dir)
	}
	// A later run of the session reads its mark back from the recorded path.
	again, _, _, err := x.workdir(ctx, c)
	if err != nil || again != third {
		t.Errorf("a later run's workdir is %s (%v), want the recorded %s", again, err, third)
	}

	// The retired session recorded its path under another spelling of the
	// data directory — a symlink, or a directory moved since. Its branch is
	// in the cache all the same, so the mark is still taken.
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub~3", ID: "s4", Harness: "claude", Workdir: "/elsewhere/workdirs/hub/s4", CreatedAt: 7, LastUsedAt: 7}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "s4", Harness: "claude", CreatedAt: 8, LastUsedAt: 8}); err != nil {
		t.Fatal(err)
	}
	moved, _, _, err := x.workdir(ctx, Claim{Connection: "hub", Run: testRun("r4", "s4")})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(e.paths.Data, "workdirs", "hub", "s4~1"); moved != want {
		t.Errorf("with the retired session's path spelled differently, s4 got %s, want %s", moved, want)
	}

	// A path nobody else holds is the session's as named.
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "s2", Harness: "claude", CreatedAt: 3, LastUsedAt: 3}); err != nil {
		t.Fatal(err)
	}
	dir2, _, _, err := x.workdir(ctx, Claim{Connection: "hub", Run: testRun("r2", "s2")})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(e.paths.Data, "workdirs", "hub", "s2"); dir2 != want {
		t.Errorf("a session nobody else's workdir is in got %s, want %s", dir2, want)
	}
}

// The whole path through the executor: a second hub under a reused name
// opens the session id the first one used, with a git source on the default
// branch. It runs on a branch of its own, in a workdir of its own, and does
// not start from the first hub's commit, which stays where it was.
func TestARetakenSessionGetsItsOwnBranchThroughTheExecutor(t *testing.T) {
	first := newEnv(t)
	root := t.TempDir()
	bare := gitRepo(t, root, "#!/bin/sh\n")
	src := []v1.Source{{Git: &v1.GitSource{URL: bare}}}
	ctx := context.Background()

	type seen struct{ dir, branch string }
	var starts []seen
	h := &fake.Adapter{ID: "claude", Next: func(s adapter.Spec) fake.Script {
		out, _ := exec.Command("git", "-C", s.Workdir, "branch", "--show-current").Output()
		starts = append(starts, seen{s.Workdir, strings.TrimSpace(string(out))})
		if len(starts) == 1 {
			os.WriteFile(filepath.Join(s.Workdir, "first-hub.txt"), []byte("the first hub's work"), 0o600)
			for _, args := range [][]string{{"add", "first-hub.txt"}, {"commit", "--quiet", "-m", "first hub"}} {
				if out, err := exec.Command("git", append([]string{"-C", s.Workdir}, args...)...).CombinedOutput(); err != nil {
					t.Errorf("git %v: %v %s", args, err, out)
				}
			}
		}
		return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}}
	}}
	x := first.sourcedExec(root, h)
	run := testRun("a", "s1")
	run.Sources = src
	runOne(t, first, first.loop(t, 1), x, run)

	// What the daemon's start does once config.toml names a new registration.
	if r, err := first.store.AdoptRegistration(ctx, "hub", 2, time.Now()); err != nil || r.As == "" {
		t.Fatalf("retirement %+v, %v", r, err)
	}

	second := newEnv(t)
	l := second.loop(t, 1)
	l.Store = first.store
	again := testRun("a", "s1")
	again.Sources = src
	second.enqueue(t, again)
	claimAndRun(t, l, x)

	if len(starts) != 2 {
		t.Fatalf("%d starts, want one per hub", len(starts))
	}
	if starts[1].dir == starts[0].dir || starts[1].branch != "yad/hub.1/s1" {
		t.Errorf("the second hub's s1 ran in %s on %q; the first hub's ran in %s on %q", starts[1].dir, starts[1].branch, starts[0].dir, starts[0].branch)
	}
	if _, err := os.Stat(filepath.Join(starts[1].dir, "first-hub.txt")); err == nil {
		t.Error("the second hub's session starts from the first hub's commit")
	}
	if r, err := first.store.GetRun(ctx, db.GetRunParams{Connection: "hub", ID: "a"}); err != nil || r.State != string(v1.RunSucceeded) {
		t.Errorf("the second hub's run here: %+v, %v", r, err)
	}
}
