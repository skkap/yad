//go:build unix

package workdir

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// Every git here — the test's own and the one under test — reads no config of
// the person running the tests: a signing key, a credential helper or an
// insteadOf rewrite in ~/.gitconfig would make the suite theirs.
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
	} {
		os.Setenv(k, v)
	}
	os.Exit(m.Run())
}

// fakeSlots is the runner store's allocation, in memory.
type fakeSlots struct {
	mu    sync.Mutex
	slots map[string]int64 // repo/connection/session → slot
}

func (f *fakeSlots) Slot(_ context.Context, repo, connection, session string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.slots == nil {
		f.slots = map[string]int64{}
	}
	key := repo + "/" + connection + "/" + session
	if s, ok := f.slots[key]; ok {
		return s, nil
	}
	used := map[int64]bool{}
	for k, s := range f.slots {
		if strings.HasPrefix(k, repo+"/") {
			used[s] = true
		}
	}
	s := int64(1)
	for used[s] {
		s++
	}
	f.slots[key] = s
	return s, nil
}

func (f *fakeSlots) ReleaseSlots(_ context.Context, connection, session string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.slots {
		if strings.HasSuffix(k, "/"+connection+"/"+session) {
			delete(f.slots, k)
		}
	}
	return nil
}

func (f *fakeSlots) held() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.slots)
}

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
}

// sh runs a test's own git or shell step.
func sh(t *testing.T, dir string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// origin is a repository a hub names, as a bare repository under root, made
// from files on main. A setup hook is one of the files, at mode 0755.
type origin struct {
	t    *testing.T
	bare string
	work string
}

func newOrigin(t *testing.T, root, name string, files map[string]string) *origin {
	t.Helper()
	o := &origin{t: t, bare: filepath.Join(root, name+".git"), work: filepath.Join(t.TempDir(), name)}
	sh(t, root, "git", "init", "--quiet", "--bare", "--initial-branch=main", o.bare)
	sh(t, "", "git", "clone", "--quiet", o.bare, o.work)
	sh(t, o.work, "git", "checkout", "--quiet", "-b", "main")
	o.commit("initial", files)
	return o
}

func (o *origin) commit(msg string, files map[string]string) {
	o.t.Helper()
	for name, body := range files {
		path := filepath.Join(o.work, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			o.t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if name == hookPath {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			o.t.Fatal(err)
		}
	}
	sh(o.t, o.work, "git", "add", "-A")
	sh(o.t, o.work, "git", "commit", "--quiet", "--allow-empty", "-m", msg)
	sh(o.t, o.work, "git", "push", "--quiet", "origin", "HEAD")
}

// events collects what a preparation emitted.
type events struct {
	mu  sync.Mutex
	evs []v1.Event
}

func (e *events) emit(ev v1.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.evs = append(e.evs, ev)
}

func (e *events) statuses() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var s []string
	for _, ev := range e.evs {
		if ev.Kind == v1.EventStatus {
			s = append(s, ev.Status)
		}
	}
	return strings.Join(s, "\n")
}

func (e *events) tool(kind v1.EventKind) *v1.ToolEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.evs {
		if ev.Kind == kind {
			return ev.Tool
		}
	}
	return nil
}

type fixture struct {
	t     *testing.T
	root  string // the owner's allowed root
	m     *Manager
	slots *fakeSlots
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	needGit(t)
	f := &fixture{t: t, root: t.TempDir(), slots: &fakeSlots{}}
	f.m = &Manager{Data: t.TempDir(), Roots: []string{f.root}, Slots: f.slots, GitTimeout: time.Minute, SetupTimeout: time.Minute}
	return f
}

// prepare runs one preparation for a session into a fresh or existing
// workdir under the manager's data directory.
func (f *fixture) prepare(session string, sources ...v1.Source) (*Prepared, *events, error) {
	f.t.Helper()
	dir := filepath.Join(f.m.Data, "workdirs", "hub", session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	ev := &events{}
	p, err := f.m.Prepare(context.Background(), Request{Dir: dir, Connection: "hub", Session: session, Sources: sources, Emit: ev.emit})
	if err == nil {
		f.t.Cleanup(p.Release)
	}
	return p, ev, err
}

func gitSource(url, base, branch string) v1.Source {
	return v1.Source{Git: &v1.GitSource{URL: url, Base: base, Branch: branch}}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func class(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ""
}

// A git source becomes a worktree on the run's branch, cut from its base,
// with the session's workdir as the repository's root; a continuing session
// finds its worktree as it left it; a second session gets its own worktree
// from the same cache, fetched first, and its own slot.
func TestGitSource(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", map[string]string{"README": "v1\n"})
	sh(t, o.work, "git", "checkout", "--quiet", "-b", "release")
	o.commit("release", map[string]string{"README": "release\n"})
	sh(t, o.work, "git", "checkout", "--quiet", "main")

	p, ev, err := f.prepare("s1", gitSource(o.bare, "release", "feature/x"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Dir != filepath.Join(f.m.Data, "workdirs", "hub", "s1") {
		t.Errorf("the harness would start in %s, not the session's workdir", p.Dir)
	}
	if got := read(t, filepath.Join(p.Dir, "README")); got != "release\n" {
		t.Errorf("README = %q: the branch was not cut from base", got)
	}
	if b := sh(t, p.Dir, "git", "branch", "--show-current"); b != "feature/x" {
		t.Errorf("worktree on %q", b)
	}
	realBare, _ := filepath.EvalSymlinks(o.bare)
	for _, want := range []string{"fetching " + realBare, "worktree of acme on feature/x from origin/release", "no setup hook in acme"} {
		if !strings.Contains(ev.statuses(), want) {
			t.Errorf("events lack %q:\n%s", want, ev.statuses())
		}
	}

	// The session continues: nothing is fetched, and its work is where it
	// left it.
	os.WriteFile(filepath.Join(p.Dir, "wip.txt"), []byte("mine"), 0o600)
	p2, ev2, err := f.prepare("s1", gitSource(o.bare, "release", "feature/x"))
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(p2.Dir, "wip.txt")) != "mine" || strings.Contains(ev2.statuses(), "fetching") {
		t.Errorf("a continuing session was not left as it was: %s", ev2.statuses())
	}
	if !strings.Contains(ev2.statuses(), "continuing") {
		t.Errorf("continuing run says: %s", ev2.statuses())
	}

	// A new commit on origin, then a second session with no base or branch
	// named: the cache is fetched first, the remote's default branch is the
	// base, and the branch is the session's own.
	o.commit("v2", map[string]string{"README": "v2\n"})
	p3, _, err := f.prepare("s2", gitSource("file://"+o.bare, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(p3.Dir, "README")); got != "v2\n" {
		t.Errorf("README = %q: the cache was not fetched before the checkout", got)
	}
	if b := sh(t, p3.Dir, "git", "branch", "--show-current"); b != "yad/hub/s2" {
		t.Errorf("unnamed branch is %q, want yad/hub/s2", b)
	}
	caches, _ := filepath.Glob(filepath.Join(f.m.Data, "repos", "*.git"))
	if len(caches) != 1 {
		t.Errorf("caches %v: a path and its file:// URL are one repository", caches)
	}
	if n := f.slots.held(); n != 2 {
		t.Errorf("%d slots held, want one per session", n)
	}
}

// A branch the remote already has is checked out as it stands, tracking it —
// a run continuing a pull request — not cut again from base.
func TestExistingRemoteBranch(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", map[string]string{"README": "main\n"})
	sh(t, o.work, "git", "checkout", "--quiet", "-b", "pr-7")
	o.commit("pr", map[string]string{"README": "pr\n"})

	p, _, err := f.prepare("s1", gitSource(o.bare, "main", "pr-7"))
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(p.Dir, "README")); got != "pr\n" {
		t.Errorf("README = %q: the remote's branch was not checked out", got)
	}
	if up := sh(t, p.Dir, "git", "rev-parse", "--abbrev-ref", "@{upstream}"); up != "origin/pr-7" {
		t.Errorf("upstream %q", up)
	}

	// Two sessions cannot hold one branch; the second is told why.
	_, _, err = f.prepare("s2", gitSource(o.bare, "main", "pr-7"))
	if class(err) != ClassSourceFailed || !strings.Contains(err.Error(), "another session") {
		t.Errorf("a branch another session holds: %v", err)
	}
}

func TestBadBase(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", nil)
	_, _, err := f.prepare("s1", gitSource(o.bare, "nope", "x"))
	if class(err) != ClassSourceFailed || !strings.Contains(err.Error(), `base "nope"`) {
		t.Errorf("err = %v", err)
	}
	// A tag and a commit are bases too.
	sh(t, o.work, "git", "tag", "v1")
	sh(t, o.work, "git", "push", "--quiet", "origin", "v1")
	if _, _, err := f.prepare("s2", gitSource(o.bare, "v1", "from-tag")); err != nil {
		t.Errorf("a tag as base: %v", err)
	}
	sha := sh(t, o.work, "git", "rev-parse", "HEAD")
	if _, _, err := f.prepare("s3", gitSource(o.bare, sha, "from-sha")); err != nil {
		t.Errorf("a commit as base: %v", err)
	}
}

// A source that breaks the owner's rules is refused before anything is
// fetched or written.
func TestRefusedSources(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", nil)
	for name, srcs := range map[string][]v1.Source{
		"option as branch":   {gitSource(o.bare, "", "--orphan")},
		"option as base":     {gitSource(o.bare, "--upload-pack=x", "b")},
		"helper transport":   {gitSource("ext::sh", "", "")},
		"path outside":       {{Path: t.TempDir()}},
		"both":               {{Git: &v1.GitSource{URL: o.bare}, Path: f.root}},
		"a repository twice": {gitSource(o.bare, "", "a"), gitSource("file://"+o.bare, "", "b")},
	} {
		_, _, err := f.prepare("s-"+strings.ReplaceAll(name, " ", "-"), srcs...)
		if class(err) != ClassSourceRefused {
			t.Errorf("%s: err = %v, want %s", name, err, ClassSourceRefused)
		}
	}
	if caches, _ := filepath.Glob(filepath.Join(f.m.Data, "repos", "*")); len(caches) != 0 {
		t.Errorf("a refused run left caches: %v", caches)
	}

	f.m.Roots = nil
	if _, _, err := f.prepare("s9", v1.Source{Path: f.root}); class(err) != ClassSourceRefused || !strings.Contains(err.Error(), "allowed none") {
		t.Errorf("a path with no roots configured: %v", err)
	}
}

// git never waits for a person: a remote that asks for a password fails at
// once. The server is loopback, in process.
func TestGitNeverPrompts(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	start := time.Now()
	_, _, err := f.prepare("s1", gitSource(srv.URL+"/acme.git", "", ""))
	if class(err) != ClassSourceFailed {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "prompts disabled") && !strings.Contains(err.Error(), "could not read Username") {
		t.Errorf("err = %v; want git's refusal to prompt", err)
	}
	// The next action names no command holding the hub's URL: pasted into a
	// shell, a URL can run code.
	if strings.Contains(err.Error(), "ls-remote "+srv.URL) || strings.Contains(err.Error(), "ls-remote https") {
		t.Errorf("the next action puts the hub's URL in a command: %v", err)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("took %s", d)
	}
	if caches, _ := filepath.Glob(filepath.Join(f.m.Data, "repos", "*")); len(caches) != 0 {
		t.Errorf("a failed first fetch left %v", caches)
	}
}

func TestGitTimeout(t *testing.T) {
	f := newFixture(t)
	// A remote that accepts the connection and never answers.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	f.m.GitTimeout = 500 * time.Millisecond
	_, _, err := f.prepare("s1", gitSource(srv.URL+"/acme.git", "", ""))
	if class(err) != ClassSourceFailed || !strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("err = %v", err)
	}
	srv.CloseClientConnections()
}

// The setup hook runs in the new worktree with the WT_* contract, its output
// becomes the run's events, and it does not run again once it has succeeded.
func TestSetupHook(t *testing.T) {
	f := newFixture(t)
	hook := `#!/bin/sh
echo "starting"
echo "to stderr" >&2
env | grep '^WT_' | sort > "$WT_ROOT/wt.env"
pwd >> "$WT_ROOT/wt.env"
echo run >> "$WT_ROOT/runs"
echo "set up slot $WT_SLOT"
`
	o := newOrigin(t, f.root, "acme", map[string]string{hookPath: hook})
	p, ev, err := f.prepare("s1", gitSource(o.bare, "main", "feat/a"))
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := filepath.Glob(filepath.Join(f.m.Data, "repos", "acme-*.git"))
	realDir, _ := filepath.EvalSymlinks(p.Dir)
	env := read(t, filepath.Join(p.Dir, "wt.env"))
	for _, want := range []string{
		"WT_BRANCH=feat/a\n", "WT_SLUG=feat-a\n", "WT_REPO=acme\n", "WT_SLOT=1\n",
		"WT_ROOT=" + p.Dir + "\n", "WT_MAIN=" + cache[0] + "\n", realDir + "\n",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("the hook's environment lacks %q:\n%s", want, env)
		}
	}
	call, res := ev.tool(v1.EventToolCall), ev.tool(v1.EventToolResult)
	if call == nil || res == nil || call.ID != res.ID || !strings.Contains(call.Input, "WT_SLOT=1") {
		t.Fatalf("hook events: %+v %+v", call, res)
	}
	if res.Output != "starting\nto stderr\nset up slot 1\n" {
		t.Errorf("hook output %q", res.Output)
	}
	if st := sh(t, p.Dir, "git", "status", "--porcelain", "--untracked-files=no"); st != "" {
		t.Errorf("the tree is dirty after setup: %s", st)
	}

	if _, ev2, err := f.prepare("s1", gitSource(o.bare, "main", "feat/a")); err != nil || ev2.tool(v1.EventToolCall) != nil {
		t.Errorf("a continuing session ran the hook again: %v", err)
	}
	if runs := read(t, filepath.Join(p.Dir, "runs")); runs != "run\n" {
		t.Errorf("hook ran %q", runs)
	}
	p2, _, err := f.prepare("s2", gitSource(o.bare, "main", "feat/b"))
	if err != nil {
		t.Fatal(err)
	}
	if env := read(t, filepath.Join(p2.Dir, "wt.env")); !strings.Contains(env, "WT_SLOT=2\n") {
		t.Errorf("a second live worktree of the repository has %s", env)
	}
}

// A hook that fails fails the preparation with setup_failed; the worktree is
// kept, and the session's next run runs the hook again.
func TestSetupHookFails(t *testing.T) {
	f := newFixture(t)
	flag := filepath.Join(t.TempDir(), "ok")
	hook := fmt.Sprintf("#!/bin/sh\necho trying\n[ -e %q ] || { echo 'database is not up' >&2; exit 3; }\necho fine\n", flag)
	o := newOrigin(t, f.root, "acme", map[string]string{hookPath: hook})
	_, ev, err := f.prepare("s1", gitSource(o.bare, "", ""))
	if class(err) != ClassSetupFailed || !strings.Contains(err.Error(), "exited 3: database is not up") {
		t.Fatalf("err = %v", err)
	}
	if res := ev.tool(v1.EventToolResult); res == nil || !strings.Contains(res.Output, "database is not up") {
		t.Errorf("the hook's output is not in the run's events: %+v", res)
	}
	os.WriteFile(flag, nil, 0o600)
	_, ev2, err := f.prepare("s1", gitSource(o.bare, "", ""))
	if err != nil {
		t.Fatalf("the next run: %v", err)
	}
	if res := ev2.tool(v1.EventToolResult); res == nil || res.Output != "trying\nfine\n" {
		t.Errorf("the hook did not run again: %+v", res)
	}
}

func TestSetupHookTimeoutAndCap(t *testing.T) {
	f := newFixture(t)
	f.m.SetupTimeout = 300 * time.Millisecond
	o := newOrigin(t, f.root, "slow", map[string]string{hookPath: "#!/bin/sh\necho begun\nsleep 30\n"})
	start := time.Now()
	_, _, err := f.prepare("s1", gitSource(o.bare, "", ""))
	if class(err) != ClassSetupFailed || !strings.Contains(err.Error(), "did not finish within") {
		t.Errorf("err = %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("a hook past its timeout held the run for %s", d)
	}

	f.m.SetupTimeout = time.Minute
	loud := newOrigin(t, f.root, "loud", map[string]string{hookPath: "#!/bin/sh\ni=0\nwhile [ $i -lt 5000 ]; do echo line $i; i=$((i+1)); done\necho LAST\n"})
	_, ev, err := f.prepare("s2", gitSource(loud.bare, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	res := ev.tool(v1.EventToolResult)
	if res == nil || !res.Truncated || len(res.Output) > v1.MaxToolOutputBytes || !strings.HasSuffix(res.Output, "LAST\n") {
		t.Errorf("a long hook's output: truncated %v, %d bytes", res != nil && res.Truncated, len(res.Output))
	}
}

func TestHookNotRun(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(t.TempDir(), "evil")
	os.WriteFile(outside, []byte("#!/bin/sh\ntouch \"$WT_ROOT/ran\"\n"), 0o755)
	for name, tc := range map[string]struct {
		files map[string]string
		after func(o *origin)
		why   string
	}{
		"not executable": {files: map[string]string{".worktree/setup.txt": "x"}, after: func(o *origin) {
			sh(t, o.work, "git", "mv", ".worktree/setup.txt", hookPath)
			o.commit("hook", nil)
		}, why: "not executable"},
		"links out": {after: func(o *origin) {
			os.MkdirAll(filepath.Join(o.work, ".worktree"), 0o755)
			os.Symlink(outside, filepath.Join(o.work, hookPath))
			o.commit("hook", nil)
		}, why: "links outside"},
	} {
		o := newOrigin(t, f.root, strings.ReplaceAll(name, " ", "-"), tc.files)
		tc.after(o)
		p, ev, err := f.prepare("s-"+strings.ReplaceAll(name, " ", "-"), gitSource(o.bare, "", ""))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(ev.statuses(), tc.why) {
			t.Errorf("%s: %s", name, ev.statuses())
		}
		if _, err := os.Stat(filepath.Join(p.Dir, "ran")); err == nil {
			t.Errorf("%s: the hook ran", name)
		}
	}
}

// A path source runs in place, only inside the owner's roots, and one run at
// a time.
func TestPathSource(t *testing.T) {
	f := newFixture(t)
	dir := filepath.Join(f.root, "project")
	os.MkdirAll(dir, 0o755)
	real, _ := filepath.EvalSymlinks(dir)

	p, _, err := f.prepare("s1", v1.Source{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	if p.Dir != real {
		t.Errorf("the harness would run in %s, not the path %s", p.Dir, real)
	}

	// A second run for the path waits for the first to let go.
	got := make(chan error, 1)
	var second *Prepared
	var ev *events
	go func() {
		var err error
		second, ev, err = f.prepare("s2", v1.Source{Path: dir + "/"})
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("the second run was not held: %v", err)
	case <-time.After(3 * lockPoll):
	}
	p.Release()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second run never got the path")
	}
	if !strings.Contains(ev.statuses(), "waiting for "+real) {
		t.Errorf("the wait was not reported: %s", ev.statuses())
	}

	// A cancelled run stops waiting.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := f.m.Prepare(ctx, Request{Dir: t.TempDir(), Connection: "hub", Session: "s3", Sources: []v1.Source{{Path: dir}}, Emit: func(v1.Event) {}})
		done <- err
	}()
	time.Sleep(2 * lockPoll)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled run kept waiting")
	}
	second.Release()
}

// Two profiles are two Managers with two data directories; a path is still
// one run's at a time between them.
func TestPathLockHoldsAcrossProfiles(t *testing.T) {
	f := newFixture(t)
	other := &Manager{Data: t.TempDir(), Roots: f.m.Roots, Slots: &fakeSlots{}}
	dir := filepath.Join(f.root, "project")
	os.MkdirAll(dir, 0o755)
	p, _, err := f.prepare("s1", v1.Source{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*lockPoll)
	defer cancel()
	_, err = other.Prepare(ctx, Request{Dir: t.TempDir(), Connection: "hub", Session: "s1", Sources: []v1.Source{{Path: dir}}, Emit: func(v1.Event) {}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("another profile took a path this one holds: %v", err)
	}
	p.Release()
	q, err := other.Prepare(context.Background(), Request{Dir: t.TempDir(), Connection: "hub", Session: "s1", Sources: []v1.Source{{Path: dir}}, Emit: func(v1.Event) {}})
	if err != nil {
		t.Fatalf("once released: %v", err)
	}
	q.Release()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("locking wrote into the owner's directory: %v", entries)
	}
}

// A worktree add killed partway leaves .git and a half-filled tree; the
// session's next run makes it again rather than working in it.
func TestHalfMadeWorktreeIsMadeAgain(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", map[string]string{"README": "x", "src/main.go": "package main"})
	p, _, err := f.prepare("s1", gitSource(o.bare, "", "b1"))
	if err != nil {
		t.Fatal(err)
	}
	// What a SIGKILL during the add leaves: no record of a finished
	// checkout, and files missing from the tree.
	gitDir := sh(t, p.Dir, "git", "rev-parse", "--absolute-git-dir")
	os.Remove(filepath.Join(gitDir, checkedOut))
	os.RemoveAll(filepath.Join(p.Dir, "src"))

	p2, ev, err := f.prepare("s1", gitSource(o.bare, "", "b1"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ev.statuses(), "half made") || strings.Contains(ev.statuses(), "continuing") {
		t.Errorf("statuses: %s", ev.statuses())
	}
	if read(t, filepath.Join(p2.Dir, "src", "main.go")) != "package main" {
		t.Error("the tree was not made again")
	}
	if b := sh(t, p2.Dir, "git", "branch", "--show-current"); b != "b1" {
		t.Errorf("on %q", b)
	}
	// And a finished one is continued, not made again.
	if _, ev3, err := f.prepare("s1", gitSource(o.bare, "", "b1")); err != nil || !strings.Contains(ev3.statuses(), "continuing") {
		t.Errorf("a finished worktree: %v %s", err, ev3.statuses())
	}
}

// A path held by one run keeps out a run on a directory above or below it;
// runs on two directories side by side go on together.
func TestNestedPathLocks(t *testing.T) {
	f := newFixture(t)
	parent := filepath.Join(f.root, "src")
	child, sibling := filepath.Join(parent, "app"), filepath.Join(parent, "lib")
	for _, d := range []string{child, sibling} {
		os.MkdirAll(d, 0o755)
	}
	try := func(path string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*lockPoll)
		defer cancel()
		p, err := f.m.Prepare(ctx, Request{Dir: t.TempDir(), Connection: "hub", Session: "x", Sources: []v1.Source{{Path: path}}, Emit: func(v1.Event) {}})
		if err == nil {
			p.Release()
		}
		return err
	}
	for _, tc := range []struct {
		held, other string
		waits       bool
	}{
		{parent, child, true},
		{child, parent, true},
		{child, sibling, false},
		{child, child, true},
	} {
		p, _, err := f.prepare("holder", v1.Source{Path: tc.held})
		if err != nil {
			t.Fatal(err)
		}
		err = try(tc.other)
		if tc.waits && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s held, %s: %v — want it to wait", tc.held, tc.other, err)
		}
		if !tc.waits && err != nil {
			t.Errorf("%s held, %s: %v — want it to go on", tc.held, tc.other, err)
		}
		p.Release()
	}
	// A run's own sources may not nest.
	if _, _, err := f.prepare("nested", v1.Source{Path: parent}, v1.Source{Path: child}); class(err) != ClassSourceRefused {
		t.Errorf("nested sources in one run: %v", err)
	}
}

// A tag the remote moves is moved in the cache, and one it deletes is gone,
// rather than failing every fetch or leaving a stale base.
func TestTagsFollowTheRemote(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", map[string]string{"README": "one"})
	sh(t, o.work, "git", "tag", "rel")
	sh(t, o.work, "git", "push", "--quiet", "origin", "rel")
	if _, _, err := f.prepare("s1", gitSource(o.bare, "rel", "b1")); err != nil {
		t.Fatal(err)
	}
	o.commit("two", map[string]string{"README": "two"})
	sh(t, o.work, "git", "tag", "-f", "rel")
	sh(t, o.work, "git", "push", "--quiet", "--force", "origin", "rel")
	p, _, err := f.prepare("s2", gitSource(o.bare, "rel", "b2"))
	if err != nil {
		t.Fatalf("a moved tag: %v", err)
	}
	if read(t, filepath.Join(p.Dir, "README")) != "two" {
		t.Error("the moved tag still names its old commit")
	}
	// A tag a harness made in its worktree is its own: no fetch moves or
	// prunes it, and a local tag named like the remote's does not shadow it.
	sh(t, p.Dir, "git", "tag", "scratch")
	sh(t, p.Dir, "git", "tag", "-f", "rel", "HEAD~1")
	if _, _, err := f.prepare("s4", gitSource(o.bare, "rel", "b4")); err != nil {
		t.Fatal(err)
	}
	if got := sh(t, p.Dir, "git", "tag", "--list", "scratch"); got != "scratch" {
		t.Errorf("a fetch removed the harness's tag: %q", got)
	}
	if local, remote := sh(t, p.Dir, "git", "rev-parse", "rel"), sh(t, o.work, "git", "rev-parse", "rel"); local == remote {
		t.Error("a fetch overwrote the harness's own tag with the remote's")
	}
	sh(t, o.work, "git", "push", "--quiet", "origin", "--delete", "rel")
	if _, _, err := f.prepare("s3", gitSource(o.bare, "rel", "b3")); class(err) != ClassSourceFailed || !strings.Contains(err.Error(), `base "rel"`) {
		t.Errorf("a deleted tag: %v", err)
	}
}

// A run naming no base is cut from the remote's default branch as it is now,
// not as it was at the first fetch.
func TestDefaultBranchFollowsTheRemote(t *testing.T) {
	f := newFixture(t)
	var major, minor int
	fmt.Sscanf(strings.TrimPrefix(sh(t, "", "git", "version"), "git version "), "%d.%d", &major, &minor)
	if major < 2 || major == 2 && minor < 48 {
		t.Skip("remote.origin.followRemoteHEAD needs git 2.48")
	}
	o := newOrigin(t, f.root, "acme", map[string]string{"README": "main"})
	if _, _, err := f.prepare("s1", gitSource(o.bare, "", "")); err != nil {
		t.Fatal(err)
	}
	sh(t, o.work, "git", "checkout", "--quiet", "-b", "trunk")
	o.commit("trunk", map[string]string{"README": "trunk"})
	sh(t, o.bare, "git", "symbolic-ref", "HEAD", "refs/heads/trunk")
	p, _, err := f.prepare("s2", gitSource(o.bare, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(p.Dir, "README")) != "trunk" {
		t.Error("the new session was cut from the old default branch")
	}
}

// Two hubs may name a session alike; each gets its own branch.
func TestUnnamedBranchesAreConnectionScoped(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", map[string]string{"README": "x"})
	for _, conn := range []string{"zumino", "yashiki"} {
		dir := filepath.Join(f.m.Data, "workdirs", conn, "work")
		os.MkdirAll(dir, 0o700)
		p, err := f.m.Prepare(context.Background(), Request{Dir: dir, Connection: conn, Session: "work", Sources: []v1.Source{gitSource(o.bare, "", "")}, Emit: func(v1.Event) {}})
		if err != nil {
			t.Fatalf("%s: %v", conn, err)
		}
		if b := sh(t, p.Dir, "git", "branch", "--show-current"); b != "yad/"+conn+"/work" {
			t.Errorf("%s: on %q", conn, b)
		}
	}
}

// A tag on a commit no branch reaches is still a base.
func TestTagOffEveryBranch(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", map[string]string{"README": "main"})
	sh(t, o.work, "git", "checkout", "--quiet", "-b", "gone")
	o.commit("release", map[string]string{"README": "released"})
	sh(t, o.work, "git", "tag", "v9")
	sh(t, o.work, "git", "push", "--quiet", "origin", "v9")
	sh(t, o.work, "git", "push", "--quiet", "origin", "--delete", "gone")
	p, _, err := f.prepare("s1", gitSource(o.bare, "v9", "from-tag"))
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(p.Dir, "README")) != "released" {
		t.Error("the branch was not cut from the tag")
	}
}

// Two repositories of one name in a run are two hooks, told apart by id.
func TestHookIDsAreDistinct(t *testing.T) {
	f := newFixture(t)
	var ids []string
	var srcs []v1.Source
	for _, sub := range []string{"a", "b"} {
		root := filepath.Join(f.root, sub)
		os.MkdirAll(root, 0o755)
		o := newOrigin(t, root, "app", map[string]string{hookPath: "#!/bin/sh\necho ok\n"})
		srcs = append(srcs, gitSource(o.bare, "", ""))
	}
	_, ev, err := f.prepare("s1", srcs...)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ev.evs {
		if e.Kind == v1.EventToolCall {
			ids = append(ids, e.Tool.ID)
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Errorf("hook ids %v", ids)
	}
}

// Several sources lie side by side under the workdir.
func TestSeveralSources(t *testing.T) {
	f := newFixture(t)
	a := newOrigin(t, f.root, "app", map[string]string{"A": "a"})
	dir := filepath.Join(f.root, "app")
	os.MkdirAll(dir, 0o755)
	p, _, err := f.prepare("s1", gitSource(a.bare, "", ""), v1.Source{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(p.Dir, "app", "A")) != "a" {
		t.Error("the git source is not under its name")
	}
	real, _ := filepath.EvalSymlinks(dir)
	if l, err := os.Readlink(filepath.Join(p.Dir, "app-2")); err != nil || l != real {
		t.Errorf("the path source is linked as %q, %v", l, err)
	}
	p.Release()
	if _, _, err := f.prepare("s1", gitSource(a.bare, "", ""), v1.Source{Path: dir}); err != nil {
		t.Errorf("the session's next run: %v", err)
	}
}

// Sessions on one repository prepare at once; the cache is shared.
func TestConcurrentSessions(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", map[string]string{"README": "x"})
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := range 6 {
		wg.Go(func() {
			_, _, err := f.prepare(fmt.Sprintf("s%d", i), gitSource(o.bare, "main", ""))
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if n := f.slots.held(); n != 6 {
		t.Errorf("%d slots for 6 live worktrees", n)
	}
}

// Reclaim removes the session's worktrees from their caches and frees its
// slots, and never touches a path source.
func TestReclaim(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", map[string]string{"README": "x"})
	dir := filepath.Join(f.root, "mine")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "keep"), nil, 0o600)
	p, _, err := f.prepare("s1", gitSource(o.bare, "", "b1"), v1.Source{Path: dir})
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	if err := f.m.Reclaim(context.Background(), "hub", "s1", p.Dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Dir, "acme")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the worktree is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keep")); err != nil {
		t.Errorf("the path source was touched: %v", err)
	}
	if f.slots.held() != 0 {
		t.Error("the session's slot was not freed")
	}
	cache, _ := filepath.Glob(filepath.Join(f.m.Data, "repos", "acme-*.git"))
	if wts := sh(t, cache[0], "git", "worktree", "list", "--porcelain"); strings.Contains(wts, "worktree "+p.Dir) {
		t.Errorf("git still lists the worktree:\n%s", wts)
	}
	// The branch stays, and a new session may take it up.
	if _, _, err := f.prepare("s2", gitSource(o.bare, "", "b1")); err != nil {
		t.Errorf("the reclaimed session's branch: %v", err)
	}
}
