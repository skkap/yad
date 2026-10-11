package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/store/db"
)

// childRunner is `yad daemon start --foreground` for profile p, as its own
// process with env added: two runners in one test, each with its own home.
func childRunner(t *testing.T, p *profile, env ...string) *child {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c := &child{out: &syncBuffer{}, done: make(chan error, 1), log: filepath.Join(p.data, "logs", "yad.log")}
	c.cmd = exec.Command(self, "daemon", "start", "--foreground")
	c.cmd.Env = append(append(os.Environ(), childYad+"=1", "YAD_CONFIG_DIR="+p.config, "YAD_DATA_DIR="+p.data), env...)
	c.cmd.Stdout, c.cmd.Stderr = c.out, c.out
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { c.done <- c.cmd.Wait() }()
	t.Cleanup(func() {
		c.cmd.Process.Signal(os.Interrupt)
		select {
		case <-c.done:
		case <-time.After(10 * time.Second):
			c.cmd.Process.Kill()
			<-c.done
		}
	})
	return c
}

// allowRoot lets profile p's runs reach root, as its owner would.
func allowRoot(t *testing.T, p *profile, root string) {
	t.Helper()
	paths := config.Paths{Config: p.config, Data: p.data}
	cfg, err := config.Load(paths)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Workdirs.Roots = []string{root}
	if err := config.Save(paths, cfg); err != nil {
		t.Fatal(err)
	}
}

// A session released by one runner resumes on another (decision 0080), end
// to end through the operator's commands, with two runners on one yad hub,
// each a process of its own with a Claude home of its own: the first run
// learns a word and pushes its work to the repository, `yad hub
// release-session` hands the session back, and the next run lands on the
// other runner, which opens the session from its archive — the conversation
// under the same id, the workdir on the branch the first worked on — and
// answers with what the first was told.
func TestE2EAReleasedSessionResumesOnAnotherRunner(t *testing.T) {
	gitPath, err := exec.LookPath("git")
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
	// The second profile first: making one clears every harness override,
	// and newMachine's points claude at the fake.
	other := newProfile(t)
	m := newMachine(t, claudeE2E)
	// Both profiles' PATH is the one empty directory newProfile made; git is
	// the one tool in it.
	if err := os.Symlink(gitPath, filepath.Join(os.Getenv("PATH"), "git")); err != nil {
		t.Fatal(err)
	}
	claudeE2E.remember(t, t.TempDir())
	t.Setenv(fakeClaudePush, "learned.txt")
	homes := map[string]string{"a": t.TempDir(), "b": t.TempDir()}
	for _, h := range homes {
		// What the fake's own login check reads: each home is logged in.
		if err := os.WriteFile(filepath.Join(h, ".credentials.json"), []byte(`{"stand_in":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	root := t.TempDir()
	bare, work := filepath.Join(root, "acme.git"), t.TempDir()
	os.WriteFile(filepath.Join(work, "README"), []byte("acme\n"), 0o644)
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", "--initial-branch=main", bare},
		{"-C", work, "init", "--quiet", "--initial-branch=main"},
		{"-C", work, "add", "-A"},
		{"-C", work, "commit", "--quiet", "-m", "initial"},
		{"-C", work, "push", "--quiet", bare, "main"},
	} {
		if out, err := exec.Command(gitPath, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	allowRoot(t, m.p, root)
	allowRoot(t, other, root)
	code, tok, errs := m.p.yad("", "hub", "token", "create")
	if code != 0 {
		t.Fatalf("hub token create: exit %d: %s", code, errs)
	}
	if code, _, errs := other.yad(tok, "connect", m.runnerURL, "--token", "-", "--name", "other"); code != 0 {
		t.Fatalf("connect the second runner: exit %d: %s", code, errs)
	}

	a := childRunner(t, m.p, "CLAUDE_CONFIG_DIR="+homes["a"])
	// The hub commands below read the hub's admin token from the first profile.
	t.Setenv("YAD_CONFIG_DIR", m.p.config)
	t.Setenv("YAD_DATA_DIR", m.p.data)
	learn := m.submitArgs("--run-id", "e2e-learn", "--new-session", "e2e-moving", "--git", "file://"+bare, "Remember the word: kumquat.")
	m.ok(learn...)
	if code, out, errs := m.watch("e2e-learn"); code != 0 {
		r, _ := m.hubDB.GetRunner(context.Background(), m.runnerID())
		t.Fatalf("watch exit %d: %s\n%s\nrunner a:\n%s\n%s\ndoc:\n%s", code, errs, out, a.logged(), a.out.String(), r.Capabilities)
	}

	// Only once the session is on a runner and its run is over is there
	// anything to release; the second runner starts now, so the first run
	// could go nowhere else.
	b := childRunner(t, other, "CLAUDE_CONFIG_DIR="+homes["b"])
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var out, errOut syncBuffer
	if code := run(ctx, []string{"hub", "release-session", "--hub", m.service, "e2e-moving"}, &out, &errOut); code != 0 {
		t.Fatalf("release-session exit %d: %s\n%s\nrunner a:\n%s", code, errOut.String(), out.String(), a.logged())
	}
	if !strings.Contains(out.String(), "is released from runner") {
		t.Errorf("release-session printed %q", out.String())
	}

	m.ok(m.submitArgs("--run-id", "e2e-recall", "--session", "e2e-moving", "Which word was it?")...)
	if code, out, errs := m.watch("e2e-recall"); code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\nrunner b:\n%s", code, errs, out, b.logged())
	}
	got, err := m.client().Run(context.Background(), "e2e-recall")
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || got.Result.FinalText != "earlier: Remember the word: kumquat." {
		t.Fatalf("the run on the second runner answered %+v", got.Result)
	}
	bID, err := os.ReadFile(filepath.Join(other.config, "runner-id"))
	if err != nil {
		t.Fatal(err)
	}
	if got.RunnerID != strings.TrimSpace(string(bID)) {
		t.Errorf("the second run ran on %s, want the second runner", got.RunnerID)
	}

	// The second runner's workdir is the first's work, from the remote, on
	// the branch the first worked on.
	bs := openStore(t, other)
	sess, err := bs.GetSession(context.Background(), db.GetSessionParams{Connection: "other", ID: "e2e-moving"})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(sess.Workdir, "learned.txt")); err != nil || !strings.Contains(string(b), "first runner") {
		t.Errorf("the second runner's workdir lacks the pushed work: %q, %v", b, err)
	}
	if branch, err := exec.Command(gitPath, "-C", sess.Workdir, "branch", "--show-current").Output(); err != nil || strings.TrimSpace(string(branch)) != "yad/home/e2e-moving" {
		t.Errorf("the second runner's workdir is on %q (%v), want the first's branch", branch, err)
	}
	// The conversation is in the second home now, and gone from the first.
	if found, _ := filepath.Glob(filepath.Join(homes["b"], "projects", "*", "*.jsonl")); len(found) != 1 {
		t.Errorf("the second home holds %v", found)
	}
	if found, _ := filepath.Glob(filepath.Join(homes["a"], "projects", "*", "*.jsonl")); len(found) != 0 {
		t.Errorf("the first home kept %v", found)
	}
	if _, err := m.hubDB.GetSessionArchive(context.Background(), "e2e-moving"); err == nil {
		t.Error("the hub kept the archive once the session opened from it ran")
	}
}

// openStore opens a profile's state database read-only, as the CLI does.
func openStore(t *testing.T, p *profile) *db.Queries {
	t.Helper()
	m := &machine{t: t, p: p}
	return m.runnerStore().Queries
}
