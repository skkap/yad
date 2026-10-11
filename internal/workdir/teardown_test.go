//go:build unix

package workdir

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// logs is a slog handler that keeps every record, for a test to read.
type logs struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (l *logs) Enabled(context.Context, slog.Level) bool { return true }
func (l *logs) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *logs) WithGroup(string) slog.Handler            { return l }
func (l *logs) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, r)
	return nil
}

// at returns the messages logged at level.
func (l *logs) at(level slog.Level) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, r := range l.recs {
		if r.Level == level {
			out = append(out, r.Message)
		}
	}
	return out
}

// withTeardown is an origin whose files include a teardown hook, made
// executable as commit makes the setup hook.
func withTeardown(t *testing.T, root, name, body string) *origin {
	t.Helper()
	return withHooks(t, root, name, "", body)
}

// withHooks is withTeardown with a setup hook as well, when setup is not "".
func withHooks(t *testing.T, root, name, setup, body string) *origin {
	t.Helper()
	files := map[string]string{"README": "x"}
	if setup != "" {
		files[hookPath] = setup
	}
	o := newOrigin(t, root, name, files)
	path := filepath.Join(o.work, teardownPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	o.commit("teardown", nil)
	return o
}

// recordEnv is a teardown that writes its WT_* variables and working
// directory to out, outside the worktree it is about to lose.
func recordEnv(out string) string {
	return fmt.Sprintf("#!/bin/sh\nenv | grep '^WT_' | sort > %q\npwd >> %q\necho ran >> %q.runs\n", out, out, out)
}

func (f *fixture) cache(name string) string {
	f.t.Helper()
	caches, _ := filepath.Glob(filepath.Join(f.m.Data, "repos", name+"-*.git"))
	if len(caches) != 1 {
		f.t.Fatalf("caches of %s: %v", name, caches)
	}
	return caches[0]
}

func (f *fixture) logs() *logs {
	l := &logs{}
	f.m.Log = slog.New(l)
	return l
}

func runs(t *testing.T, out string) int {
	t.Helper()
	b, err := os.ReadFile(out + ".runs")
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "ran\n")
}

// A reclaim runs the repository's teardown in the worktree with the values
// setup was given — the branch the run was on, even after the harness
// switched away from it — and then removes the worktree and frees the slot.
func TestTeardownHook(t *testing.T) {
	f := newFixture(t)
	l := f.logs()
	out := filepath.Join(t.TempDir(), "env")
	o := withTeardown(t, f.root, "acme", recordEnv(out))
	p, _, err := f.prepare("s1", gitSource(o.bare, "main", "feat/a"))
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	sh(t, p.Dir, "git", "checkout", "--quiet", "-b", "elsewhere")
	realDir, _ := filepath.EvalSymlinks(p.Dir)
	common := sh(t, p.Dir, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")

	if err := f.m.Reclaim(context.Background(), "hub", "s1", p.Dir); err != nil {
		t.Fatal(err)
	}
	env := read(t, out)
	for _, want := range []string{
		"WT_BRANCH=feat/a\n", "WT_SLUG=feat-a\n", "WT_REPO=acme\n", "WT_SLOT=1\n",
		"WT_ROOT=" + p.Dir + "\n", "WT_MAIN=" + common + "\n", realDir + "\n",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("the teardown's environment lacks %q:\n%s", want, env)
		}
	}
	if _, err := os.Stat(filepath.Join(p.Dir, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the worktree is still there: %v", err)
	}
	if f.slots.held() != 0 {
		t.Error("the session's slot was not freed")
	}
	if info := l.at(slog.LevelInfo); len(info) != 1 || !strings.Contains(info[0], "teardown hook ran") {
		t.Errorf("logged %q", info)
	}
}

// A teardown that fails or hangs is logged, and the reclaim goes on: the
// worktree is removed and the slot freed. The message is the runner's own
// words, with the repository, session and path in attributes only, since it
// reaches every hub in health's recent_errors.
func TestTeardownThatFailsOrHangs(t *testing.T) {
	for _, tc := range []struct {
		name, hook, want string
	}{
		{"exits non-zero", "#!/bin/sh\necho 'compose: no such project' >&2\nexit 3\n", "teardown hook failed"},
		{"hangs", "#!/bin/sh\necho stopping\nsleep 30\n", "did not finish within"},
		{"will not start", "#!/nonexistent/interpreter\n", "could not be started"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.name == "hangs" {
				f.m.TeardownTimeout = 300 * time.Millisecond
			}
			l := f.logs()
			o := withTeardown(t, f.root, "acme", tc.hook)
			p, _, err := f.prepare("s1", gitSource(o.bare, "", ""))
			if err != nil {
				t.Fatal(err)
			}
			p.Release()
			start := time.Now()
			if err := f.m.Reclaim(context.Background(), "hub", "s1", p.Dir); err != nil {
				t.Fatalf("a failing teardown failed the reclaim: %v", err)
			}
			if d := time.Since(start); d > 10*time.Second {
				t.Errorf("a teardown past its timeout held the reclaim for %s", d)
			}
			if _, err := os.Stat(filepath.Join(p.Dir, ".git")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the worktree is still there: %v", err)
			}
			if f.slots.held() != 0 {
				t.Error("the session's slot was not freed")
			}
			warns := l.at(slog.LevelWarn)
			if len(warns) != 1 || !strings.Contains(warns[0], tc.want) {
				t.Fatalf("warnings %q, want one saying %q", warns, tc.want)
			}
			for _, leak := range []string{"acme", "s1", f.m.Data, f.root} {
				if strings.Contains(warns[0], leak) {
					t.Errorf("the warning's message carries %q: %s", leak, warns[0])
				}
			}
		})
	}
}

// The slot stays the session's while its teardown runs, so no other worktree
// of the repository is given the ports whose stacks are still going down —
// and the repository is not locked meanwhile, so another session prepares a
// worktree of it without waiting.
func TestTeardownHoldsTheSlotNotTheRepository(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "go")
	hook := fmt.Sprintf("#!/bin/sh\ntouch %q\ni=0\nwhile [ ! -e %q ] && [ $i -lt 600 ]; do sleep 0.05; i=$((i+1)); done\n", started, release)
	o := withTeardown(t, f.root, "acme", hook)
	p, _, err := f.prepare("s1", gitSource(o.bare, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	repo := filepath.Base(f.cache("acme"))

	done := make(chan error, 1)
	go func() { done <- f.m.Reclaim(context.Background(), "hub", "s1", p.Dir) }()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the teardown never started")
		}
	}
	if slot, held, _ := f.slots.HeldSlot(context.Background(), repo, "hub", "s1"); !held || slot != 1 {
		t.Errorf("during its teardown the session holds slot %d (%v), want its 1", slot, held)
	}
	prepared := make(chan error, 1)
	go func() {
		p2, _, err := f.prepare("s2", gitSource(o.bare, "", ""))
		if err == nil {
			p2.Release()
		}
		prepared <- err
	}()
	select {
	case err := <-prepared:
		if err != nil {
			t.Errorf("another session's preparation: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("another session's preparation waited for the teardown")
	}
	if slot, _, _ := f.slots.HeldSlot(context.Background(), repo, "hub", "s2"); slot != 2 {
		t.Errorf("the next worktree was given slot %d while the first was still tearing down, want 2", slot)
	}
	os.WriteFile(release, nil, 0o600)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, held, _ := f.slots.HeldSlot(context.Background(), repo, "hub", "s1"); held {
		t.Error("the slot was not freed once the teardown finished")
	}
}

// What a teardown is not run for: a worktree whose add never finished, a
// session that holds no slot for the repository, a teardown that is not
// executable, a path source, and a runner that could not link its docker.
func TestTeardownNotRun(t *testing.T) {
	for _, tc := range []struct {
		name  string
		path  bool // the run also has a path source holding a teardown
		after func(t *testing.T, f *fixture, p *Prepared)
		info  string // what is logged at Info, if anything
		warn  string
	}{
		{name: "half made", after: func(t *testing.T, f *fixture, p *Prepared) {
			gitDir := sh(t, p.Dir, "git", "rev-parse", "--absolute-git-dir")
			if err := os.Remove(filepath.Join(gitDir, checkedOut)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "no slot", after: func(t *testing.T, f *fixture, p *Prepared) {
			f.slots.ReleaseSlots(context.Background(), "hub", "s1")
		}},
		{name: "not executable", after: func(t *testing.T, f *fixture, p *Prepared) {
			if err := os.Chmod(filepath.Join(p.Dir, teardownPath), 0o644); err != nil {
				t.Fatal(err)
			}
		}, info: "is not run"},
		{name: "path source", path: true},
		{name: "links", after: func(t *testing.T, f *fixture, p *Prepared) {
			override := filepath.Join(t.TempDir(), "docker-27")
			if err := os.WriteFile(override, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("YAD_DOCKER_PATH", override)
			links := filepath.Join(f.m.Data, "host-tools")
			os.RemoveAll(links)
			if err := os.WriteFile(links, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, warn: "was not run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			l := f.logs()
			out := filepath.Join(t.TempDir(), "env")
			o := withTeardown(t, f.root, "acme", recordEnv(out))
			sources := []v1.Source{gitSource(o.bare, "", "")}
			mine := filepath.Join(f.root, "mine")
			if tc.path {
				if err := os.MkdirAll(filepath.Join(mine, ".worktree"), 0o755); err != nil {
					t.Fatal(err)
				}
				sh(t, mine, "git", "init", "--quiet")
				if err := os.WriteFile(filepath.Join(mine, teardownPath), []byte(recordEnv(out+"-path")), 0o755); err != nil {
					t.Fatal(err)
				}
				sources = append(sources, v1.Source{Path: mine})
			}
			p, _, err := f.prepare("s1", sources...)
			if err != nil {
				t.Fatal(err)
			}
			p.Release()
			wt := p.Dir
			if tc.path {
				wt = filepath.Join(p.Dir, "acme")
				// The git source's teardown runs; the path source's never does.
				defer func() {
					if runs(t, out) != 1 {
						t.Error("the git source's teardown did not run beside the path source")
					}
					if runs(t, out+"-path") != 0 {
						t.Error("the path source's teardown ran")
					}
					if _, err := os.Stat(filepath.Join(mine, teardownPath)); err != nil {
						t.Errorf("the path source was touched: %v", err)
					}
				}()
			}
			if tc.after != nil {
				tc.after(t, f, &Prepared{Dir: wt})
			}
			if err := f.m.Reclaim(context.Background(), "hub", "s1", p.Dir); err != nil {
				t.Fatal(err)
			}
			if !tc.path && runs(t, out) != 0 {
				t.Errorf("the teardown ran")
			}
			if _, err := os.Stat(filepath.Join(wt, ".git")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the worktree is still there: %v", err)
			}
			if f.slots.held() != 0 {
				t.Error("the session's slot was not freed")
			}
			for level, want := range map[slog.Level]string{slog.LevelInfo: tc.info, slog.LevelWarn: tc.warn} {
				got := strings.Join(l.at(level), "\n")
				if want == "" && level == slog.LevelWarn && got != "" || want != "" && !strings.Contains(got, want) {
					t.Errorf("logged at %s %q, want %q", level, got, want)
				}
			}
		})
	}
}

// A teardown runs once per reclaim: a removal that fails after it does not
// run it again when the reclaim is tried again.
func TestTeardownRunsOnceAcrossARetry(t *testing.T) {
	f := newFixture(t)
	out := filepath.Join(t.TempDir(), "env")
	o := withTeardown(t, f.root, "acme", recordEnv(out))
	p, _, err := f.prepare("s1", gitSource(o.bare, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = remove ] && { echo 'fatal: index.lock exists' >&2; exit 128; }; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.m.Git = wrapper
	if err := f.m.Reclaim(context.Background(), "hub", "s1", p.Dir); err == nil {
		t.Fatal("reclaim succeeded with the worktree still registered")
	}
	if f.slots.held() != 1 {
		t.Errorf("%d slots held after a failed removal, want the session's 1", f.slots.held())
	}
	f.m.Git = real
	if err := f.m.Reclaim(context.Background(), "hub", "s1", p.Dir); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if n := runs(t, out); n != 1 {
		t.Errorf("the teardown ran %d times, want once", n)
	}
	if f.slots.held() != 0 {
		t.Error("the retry did not free the slot")
	}
}

// A reclaim cancelled while its teardown runs — the daemon stopping — keeps
// the worktree and the slot, and the next reclaim runs the teardown again.
func TestACancelledTeardownKeepsTheWorktree(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	started, release, count := filepath.Join(dir, "started"), filepath.Join(dir, "go"), filepath.Join(dir, "count")
	hook := fmt.Sprintf("#!/bin/sh\necho ran >> %q\n[ -e %q ] && exit 0\ntouch %q\nsleep 30\n", count, release, started)
	o := withTeardown(t, f.root, "acme", hook)
	p, _, err := f.prepare("s1", gitSource(o.bare, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, err := os.Stat(started); err == nil {
				break
			}
		}
		cancel()
	}()
	start := time.Now()
	if err := f.m.Reclaim(ctx, "hub", "s1", p.Dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled reclaim = %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 15*time.Second {
		t.Errorf("the cancel took %s to stop the teardown", d)
	}
	if _, err := os.Stat(filepath.Join(p.Dir, ".git")); err != nil {
		t.Errorf("the worktree went with a cancelled teardown: %v", err)
	}
	if f.slots.held() != 1 {
		t.Errorf("%d slots held after a cancelled teardown, want the session's 1", f.slots.held())
	}
	os.WriteFile(release, nil, 0o600)
	if err := f.m.Reclaim(context.Background(), "hub", "s1", p.Dir); err != nil {
		t.Fatalf("the next reclaim: %v", err)
	}
	if n := strings.Count(read(t, count), "ran\n"); n != 2 {
		t.Errorf("the teardown ran %d times, want again after the cancel", n)
	}
	if f.slots.held() != 0 {
		t.Error("the slot was not freed")
	}
}

// A run with two repositories tears each down in its own worktree, with its
// own slot.
func TestTeardownOfSeveralSources(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	a := withTeardown(t, f.root, "acme", recordEnv(filepath.Join(dir, "acme")))
	b := withTeardown(t, f.root, "beta", recordEnv(filepath.Join(dir, "beta")))
	// Another session holds beta's first slot, so the two differ.
	if _, _, err := f.prepare("other", gitSource(b.bare, "", "")); err != nil {
		t.Fatal(err)
	}
	p, _, err := f.prepare("s1", gitSource(a.bare, "", ""), gitSource(b.bare, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	if err := f.m.Reclaim(context.Background(), "hub", "s1", p.Dir); err != nil {
		t.Fatal(err)
	}
	for name, slot := range map[string]string{"acme": "1", "beta": "2"} {
		env := read(t, filepath.Join(dir, name))
		for _, want := range []string{"WT_REPO=" + name + "\n", "WT_SLOT=" + slot + "\n", "WT_ROOT=" + filepath.Join(p.Dir, name) + "\n"} {
			if !strings.Contains(env, want) {
				t.Errorf("%s's teardown lacks %q:\n%s", name, want, env)
			}
		}
	}
	if f.slots.held() != 1 {
		t.Errorf("%d slots held, want only the other session's", f.slots.held())
	}
}

// A worktree made before the setup values were recorded tears down with its
// current branch and the repository's name read from its cache's.
func TestTeardownOfAnOlderWorktree(t *testing.T) {
	f := newFixture(t)
	out := filepath.Join(t.TempDir(), "env")
	o := withTeardown(t, f.root, "my-app", recordEnv(out))
	p, _, err := f.prepare("s1", gitSource(o.bare, "", "feat/a"))
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	gitDir := sh(t, p.Dir, "git", "rev-parse", "--absolute-git-dir")
	if err := os.Remove(filepath.Join(gitDir, hookEnvFile)); err != nil {
		t.Fatal(err)
	}
	sh(t, p.Dir, "git", "checkout", "--quiet", "-b", "fix/b")
	if err := f.m.Reclaim(context.Background(), "hub", "s1", p.Dir); err != nil {
		t.Fatal(err)
	}
	env := read(t, out)
	for _, want := range []string{"WT_BRANCH=fix/b\n", "WT_SLUG=fix-b\n", "WT_REPO=my-app\n", "WT_SLOT=1\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("the teardown's environment lacks %q:\n%s", want, env)
		}
	}
}

func TestRepoOfCache(t *testing.T) {
	for cache, want := range map[string]string{
		cacheName(remote{name: "acme", key: "k"}):   "acme",
		cacheName(remote{name: "my-app", key: "k"}): "my-app",
		"plain.git":              "plain",
		"named-like-this.git":    "named-like-this",
		"short-0123456789ab.git": "short-0123456789ab",
	} {
		if got := repoOfCache(cache); got != want {
			t.Errorf("repoOfCache(%q) = %q, want %q", cache, got, want)
		}
	}
}

// A setup that fails half way may already have started something, so its
// worktree is torn down all the same.
func TestTeardownAfterAFailedSetup(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	started, out := filepath.Join(dir, "started"), filepath.Join(dir, "env")
	setup := fmt.Sprintf("#!/bin/sh\necho up > %q\necho 'migrations failed' >&2\nexit 1\n", started)
	o := withHooks(t, f.root, "acme", setup, recordEnv(out))
	p, _, err := f.prepare("s1", gitSource(o.bare, "", ""))
	if class(err) != ClassSetupFailed {
		t.Fatalf("err = %v, want the setup to fail", err)
	}
	if _, err := os.Stat(started); err != nil {
		t.Fatalf("the setup started nothing: %v", err)
	}
	wt := filepath.Join(f.m.Data, "workdirs", "hub", "s1")
	if p != nil {
		wt = p.Dir
	}
	if err := f.m.Reclaim(context.Background(), "hub", "s1", wt); err != nil {
		t.Fatal(err)
	}
	if runs(t, out) != 1 || !strings.Contains(read(t, out), "WT_SLOT=1\n") {
		t.Errorf("the teardown did not run, with its slot, after a failed setup")
	}
	if f.slots.held() != 0 {
		t.Error("the session's slot was not freed")
	}
}

// lockedBuffer is a log destination a hook's goroutines and the test share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Nothing a teardown prints reaches the runner's log, in a message or an
// attribute: a hook that echoes a credential before failing must not put it
// in yad.log or yad status.
func TestTeardownOutputStaysOutOfTheLog(t *testing.T) {
	for _, tc := range []struct{ name, hook string }{
		{"exits non-zero", "#!/bin/sh\necho yad_secret_out\necho yad_secret_err >&2\nexit 3\n"},
		{"hangs", "#!/bin/sh\necho yad_secret_out\nsleep 30\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.m.TeardownTimeout = 2 * time.Second
			buf := &lockedBuffer{}
			f.m.Log = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			o := withTeardown(t, f.root, "acme", tc.hook)
			p, _, err := f.prepare("s1", gitSource(o.bare, "", ""))
			if err != nil {
				t.Fatal(err)
			}
			p.Release()
			if err := f.m.Reclaim(context.Background(), "hub", "s1", p.Dir); err != nil {
				t.Fatal(err)
			}
			got := buf.String()
			if !strings.Contains(got, "teardown hook") {
				t.Fatalf("nothing was logged about the teardown:\n%s", got)
			}
			if strings.Contains(got, "yad_secret") {
				t.Errorf("the teardown's output reached the log:\n%s", got)
			}
		})
	}
}

// A record a runner killed mid-write left partial is written again on the
// session's next run, rather than kept for a teardown that cannot read it.
func TestAPartialHookEnvIsWrittenAgain(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", map[string]string{"README": "x"})
	p, _, err := f.prepare("s1", gitSource(o.bare, "", "feat/a"))
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	gitDir := sh(t, p.Dir, "git", "rev-parse", "--absolute-git-dir")
	if err := os.WriteFile(filepath.Join(gitDir, hookEnvFile), []byte(`{"branch":"fe`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _, err = f.prepare("s1", gitSource(o.bare, "", "feat/a"))
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	if saved, ok := readHookEnv(gitDir); !ok || saved.Branch != "feat/a" || saved.Repo != "acme" {
		t.Errorf("after the next run the record is %+v (%v), want feat/a in acme", saved, ok)
	}
	if left, _ := filepath.Glob(filepath.Join(gitDir, ".yad-hook-env-*")); len(left) != 0 {
		t.Errorf("a temporary record was left behind: %v", left)
	}
}
