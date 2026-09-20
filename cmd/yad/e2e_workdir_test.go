package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/store/db"
)

// A run with a git source, end to end: the runner fetches the repository into
// its cache, checks the run's branch out as the session's workdir, runs the
// repository's setup hook there with its slot, and only then starts the
// harness — which reads the file the hook wrote, in the worktree it was
// given. The repository is a bare one on disk, inside the root the owner
// allowed; nothing leaves the machine.
func TestE2EGitSourceAndSetupHook(t *testing.T) { eachHarness(t, testE2EGitSourceAndSetupHook) }

func testE2EGitSourceAndSetupHook(t *testing.T, h *e2eHarness) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
	} {
		t.Setenv(k, v)
	}
	m := newMachine(t, h)
	// The profile's PATH is an empty directory, so no real harness is found;
	// the runner gets git there and nothing else.
	if err := os.Symlink(git, filepath.Join(os.Getenv("PATH"), "git")); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bare := filepath.Join(root, "acme.git")
	work := t.TempDir()
	hook := "#!/bin/sh\nprintf 'hello from the setup hook in slot %s on %s\\n' \"$WT_SLOT\" \"$WT_BRANCH\" > note.txt\necho set up\n"
	if err := os.MkdirAll(filepath.Join(work, ".worktree"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, ".worktree", "setup"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	// The hook writes note.txt; the repository must not track it, or setup
	// would leave the tree dirty.
	if err := os.WriteFile(filepath.Join(work, ".gitignore"), []byte("note.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", "--initial-branch=main", bare},
		{"-C", work, "init", "--quiet", "--initial-branch=main"},
		{"-C", work, "add", "-A"},
		{"-C", work, "commit", "--quiet", "-m", "initial"},
		{"-C", work, "push", "--quiet", bare, "main"},
	} {
		if out, err := exec.Command(git, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// The owner allows the root; nothing a hub sends could.
	paths := config.Paths{Config: m.p.config, Data: m.p.data}
	cfg, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workdirs.Roots = []string{root}
	if err := config.Save(paths, cfg); err != nil {
		t.Fatal(err)
	}

	argsFile := filepath.Join(t.TempDir(), "harness.starts")
	t.Setenv(h.starts, argsFile)
	t.Setenv(h.read, "note.txt")
	out := m.ok(m.submitArgs("--run-id", "e2e-git", "--git", "file://"+bare, "--branch", "e2e/hooked", h.instruction)...)
	if strings.TrimSpace(out) != "e2e-git" {
		t.Fatalf("submit printed %q", out)
	}
	d := m.daemon()

	code, watched, errs := m.watch("e2e-git")
	if code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, watched, d.out.String())
	}
	const answer = "hello from the setup hook in slot 1 on e2e/hooked"
	for _, want := range []string{"· fetching", "· worktree of acme on e2e/hooked", "→ setup hook", "← set up", answer, "── succeeded in"} {
		if !strings.Contains(watched, want) {
			t.Errorf("watch output lacks %q:\n%s", want, watched)
		}
	}

	run, err := m.client().Run(context.Background(), "e2e-git")
	if err != nil {
		t.Fatal(err)
	}
	evs := m.hubEvents("e2e-git")
	contiguous(t, evs)
	if run.Result == nil || run.Result.State != v1.RunSucceeded || run.Result.FinalText != answer || run.Result.LastSeq != int64(len(evs)) {
		t.Fatalf("result %+v with %d events", run.Result, len(evs))
	}

	// The harness ran at the worktree's root, which is the session's workdir.
	s := m.runnerStore()
	sess, err := s.GetSession(context.Background(), db.GetSessionParams{Connection: "home", ID: localRun(t, s, "e2e-git").SessionID})
	if err != nil {
		t.Fatal(err)
	}
	started, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	wd, _, _ := strings.Cut(string(started), " ")
	if !samePath(wd, sess.Workdir) {
		t.Errorf("the harness ran in %s, not the session's worktree %s", wd, sess.Workdir)
	}
	if b, err := exec.Command(git, "-C", sess.Workdir, "branch", "--show-current").Output(); err != nil || strings.TrimSpace(string(b)) != "e2e/hooked" {
		t.Errorf("the workdir is on %q, %v", b, err)
	}

	// Closed from the hub, the session gives everything back: the worktree
	// leaves its bare cache, its WT_SLOT is free, and the directory goes.
	caches, _ := filepath.Glob(filepath.Join(m.p.data, "repos", "*.git"))
	if len(caches) != 1 {
		t.Fatalf("bare caches: %v", caches)
	}
	m.ok("hub", "close-session", "--hub", m.service, sess.ID)
	eventually(t, "the session's workdir is reclaimed", func() bool {
		_, err := os.Stat(sess.Workdir)
		return os.IsNotExist(err)
	})
	// The worktree registration and the WT_SLOT row are given back after the
	// directory is gone, not with it, so the reclaimed workdir above is not a
	// sync point for either. Asserting them the moment it vanishes fails on a
	// machine with few cores -- every run under GOMAXPROCS=1 before this.
	eventually(t, "the bare cache has given up the session's worktree", func() bool {
		wts, err := exec.Command(git, "-C", caches[0], "worktree", "list", "--porcelain").Output()
		return err == nil && strings.Count(string(wts), "worktree ") == 1
	})
	eventually(t, "the session's WT_SLOT is released", func() bool {
		var slots int
		err := s.DB.QueryRow("SELECT count(*) FROM slots WHERE session_id = ?", sess.ID).Scan(&slots)
		return err == nil && slots == 0
	})
}

func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}
