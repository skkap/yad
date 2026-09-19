//go:build unix

package workdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
)

// Error classes a failed preparation carries into the run's result. A hub acts
// on the class and shows the message; a new class is an addition, never a
// rename.
const (
	// ClassSourceRefused — a source breaks the owner's rules: a transport
	// this runner does not use, a path outside the allowed roots, a ref name
	// git would misread. Retrying the same run changes nothing.
	ClassSourceRefused = "source_refused"
	// ClassSourceFailed — git could not fetch or check out the source: the
	// network, the machine's credentials, a branch another session holds.
	ClassSourceFailed = "source_failed"
	// ClassSetupFailed — the repository's .worktree/setup exited non-zero or
	// ran out of time (decision 0034).
	ClassSetupFailed = "setup_failed"
)

// Error is a preparation that failed, with the class the run's result carries.
type Error struct {
	Class string
	Msg   string
}

func (e *Error) Error() string { return e.Msg }

// Slots allocates WT_SLOT. The runner's store is the one implementation.
type Slots interface {
	Slot(ctx context.Context, repo, connection, session string) (int64, error)
	ReleaseSlots(ctx context.Context, connection, session string) error
}

// Manager turns a run's sources into its working directory.
type Manager struct {
	// Data is the profile's data directory: bare caches live in repos/.
	Data string
	// Roots are the owner's directories a hub may reach (decision 0033).
	Roots []string
	// Git is the git executable; empty finds it on PATH.
	Git          string
	GitTimeout   time.Duration
	SetupTimeout time.Duration
	Slots        Slots

	once  sync.Once
	mu    sync.Mutex
	repos map[string]chan struct{}
}

func (m *Manager) init() {
	m.once.Do(func() {
		m.repos = map[string]chan struct{}{}
		if m.Git == "" {
			if p, err := exec.LookPath("git"); err == nil {
				m.Git = p
			} else {
				m.Git = "git"
			}
		}
		if m.GitTimeout <= 0 {
			m.GitTimeout = config.DefaultGitTimeout
		}
		if m.SetupTimeout <= 0 {
			m.SetupTimeout = config.DefaultSetupTimeout
		}
	})
}

// Request is one run's preparation.
type Request struct {
	// Dir is the session's workdir, which exists. Where it lives and how long
	// it is kept are the session store's business, not this package's.
	Dir        string
	Connection string
	Session    string
	Sources    []v1.Source
	// Emit puts a status or tool event into the run's stream. Never nil.
	Emit func(v1.Event)
}

// Prepared is a workdir ready for the harness.
type Prepared struct {
	// Dir is where the harness runs: the session's workdir, or — for a run
	// whose one source is a path — that path itself.
	Dir   string
	locks []func()
}

// Release lets go of the path sources' locks. The run calls it when it ends;
// calling it twice is harmless.
func (p *Prepared) Release() {
	for _, unlock := range p.locks {
		unlock()
	}
	p.locks = nil
}

// item is one source, checked, and where it goes.
type item struct {
	git    *remote
	base   string
	branch string
	path   string
	// dest is where a git worktree is added, or a path source linked; for
	// the only source of a run, the workdir itself.
	dest string
}

// Prepare builds the run's workdir from its sources. One git source is
// checked out as the workdir itself, so the harness starts at the
// repository's root; one path source is used in place. Several are laid out
// side by side under the workdir, each under its repository's or directory's
// name, a path source as a symlink to where it lives.
//
// A git source is fetched into the runner's bare cache for its repository and
// added as a worktree on the run's branch — cut from base when the branch is
// new, checked out as it stands when it exists. A workdir that already holds
// the worktree, because the session is continuing, is left as it is: what the
// previous run left there is the session's work. Its setup hook runs once the
// worktree exists, and again on a later run until it has succeeded once.
//
// Every path source is locked for as long as the run holds its Prepared; a
// second run for the same path waits its turn.
func (m *Manager) Prepare(ctx context.Context, req Request) (*Prepared, error) {
	m.init()
	items, err := m.plan(req)
	if err != nil {
		return nil, &Error{Class: ClassSourceRefused, Msg: err.Error()}
	}
	p := &Prepared{Dir: req.Dir}
	done := false
	defer func() {
		if !done {
			p.Release()
		}
	}()
	var paths []string
	for _, it := range items {
		if it.git == nil {
			paths = append(paths, it.path)
		}
	}
	if len(paths) > 0 {
		unlock, err := lockPaths(ctx, paths, req.Emit)
		if err != nil {
			return nil, err
		}
		p.locks = append(p.locks, unlock)
	}
	for _, it := range items {
		if it.git == nil {
			if len(items) == 1 {
				p.Dir = it.path
				continue
			}
			if err := link(it.path, it.dest); err != nil {
				return nil, &Error{Class: ClassSourceFailed, Msg: err.Error()}
			}
			continue
		}
		if err := m.checkout(ctx, req, it); err != nil {
			return nil, err
		}
	}
	done = true
	return p, nil
}

// plan checks every source before anything touches the disk, so a run with
// one bad source changes nothing.
func (m *Manager) plan(req Request) ([]item, error) {
	var items []item
	var names []string
	seen := map[string]bool{}
	for i, src := range req.Sources {
		if err := src.Validate(); err != nil {
			return nil, fmt.Errorf("sources[%d]: %w", i, err)
		}
		var it item
		var name string
		if src.Git != nil {
			r, err := parseRemote(src.Git.URL, m.Roots)
			if err != nil {
				return nil, fmt.Errorf("sources[%d]: %w", i, err)
			}
			if seen[r.key] {
				// Both would want the one slot the session holds for the
				// repository, and derive the same ports from it.
				return nil, fmt.Errorf("sources[%d]: %s is named twice; a run checks out a repository once", i, src.Git.URL)
			}
			seen[r.key] = true
			it.git, it.base, it.branch = &r, src.Git.Base, src.Git.Branch
			if it.base != "" {
				if err := checkRef("sources["+fmt.Sprint(i)+"].git.base", it.base); err != nil {
					return nil, err
				}
			}
			if it.branch == "" {
				// Session ids are unique only within a connection, and
				// every connection shares the cache.
				it.branch = "yad/" + safeComponent(req.Connection) + "/" + safeComponent(req.Session)
			} else if err := checkRef("sources["+fmt.Sprint(i)+"].git.branch", it.branch); err != nil {
				return nil, err
			}
			name = r.name
		} else {
			dir, err := inRoots(fmt.Sprintf("sources[%d].path", i), src.Path, m.Roots)
			if err != nil {
				return nil, err
			}
			it.path, name = dir, repoName(dir)
		}
		items = append(items, it)
		names = append(names, name)
	}
	// A run's own path sources may not nest: one is already reachable
	// through the other, and the locks would have the run wait on itself.
	for i, a := range items {
		for j, b := range items {
			if i != j && a.git == nil && b.git == nil && within(a.path, b.path) {
				return nil, fmt.Errorf("sources[%d] (%s) is inside sources[%d] (%s); name the outer directory once", j, b.path, i, a.path)
			}
		}
	}
	if len(items) == 1 {
		items[0].dest = req.Dir
		return items, nil
	}
	used := map[string]bool{}
	for i := range items {
		name := names[i]
		for n := 2; used[name]; n++ {
			name = fmt.Sprintf("%s-%d", names[i], n)
		}
		used[name] = true
		items[i].dest = filepath.Join(req.Dir, name)
	}
	return items, nil
}

// safeComponent turns a hub's session id into something a branch name may
// hold: readable when it already is, hashed when it is not.
func safeComponent(id string) string {
	s := unsafeName.ReplaceAllString(id, "-")
	s = strings.Trim(s, ".-")
	if s == "" || s != id || len(s) > 64 || strings.Contains(s, "..") || strings.HasSuffix(s, ".lock") {
		return "s-" + digest(id)
	}
	return s
}

// link puts a path source into a workdir laid out for several sources. A link
// already there from an earlier run of the session is kept when it points to
// the same place.
func link(target, dest string) error {
	if cur, err := os.Readlink(dest); err == nil {
		if cur == target {
			return nil
		}
		return fmt.Errorf("%s already links to %s, not %s — the session's sources changed; start a new session", dest, cur, target)
	}
	if err := os.Symlink(target, dest); err != nil {
		return fmt.Errorf("link %s into the workdir: %w", target, err)
	}
	return nil
}

// checkout makes it.dest a worktree of the source's repository and runs the
// setup hook.
func (m *Manager) checkout(ctx context.Context, req Request, it item) error {
	r := it.git
	cache := filepath.Join(m.Data, "repos", r.name+"-"+digest(r.key)+".git")
	fresh, err := m.worktree(ctx, req, it, cache)
	if err != nil {
		return err
	}
	slot, err := m.Slots.Slot(ctx, filepath.Base(cache), req.Connection, req.Session)
	if err != nil {
		return &Error{Class: ClassSourceFailed, Msg: "no WT_SLOT could be allocated: " + err.Error()}
	}
	return m.setup(ctx, req, hookEnv{
		root: it.dest, main: cache, branch: it.branch, repo: r.name, slot: slot, fresh: fresh,
	})
}

// worktree adds the worktree, or finds it already there. fresh is true when
// it was added now.
func (m *Manager) worktree(ctx context.Context, req Request, it item, cache string) (fresh bool, err error) {
	failed := func(err error) (bool, error) {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, &Error{Class: ClassSourceFailed, Msg: err.Error()}
	}
	// The fetch, the ref the new worktree creates and a half-made worktree's
	// removal are one repository's business at a time; the run that follows
	// is not, and neither is the setup hook.
	unlock, err := m.lockRepo(ctx, cache)
	if err != nil {
		return false, err
	}
	defer unlock()
	if _, err := os.Lstat(filepath.Join(it.dest, ".git")); err == nil {
		common, err := m.git(ctx, it.dest, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return failed(fmt.Errorf("the session's workdir %s holds a checkout git cannot read (%v) — start a new session", it.dest, err))
		}
		if !samePath(common, cache) {
			return failed(fmt.Errorf("the session's workdir %s holds a checkout of another repository (%s) — a session keeps its sources; start a new session for %s", it.dest, common, it.git.name))
		}
		gitDir, err := m.git(ctx, it.dest, "rev-parse", "--absolute-git-dir")
		if err != nil {
			return failed(err)
		}
		if _, err := os.Stat(filepath.Join(gitDir, checkedOut)); err == nil {
			req.Emit(v1.Event{Kind: v1.EventStatus, Status: fmt.Sprintf("continuing in the session's worktree of %s", it.git.name)})
			return false, nil
		}
		// An add that was killed — a cancel, the git timeout, the runner
		// stopping — leaves .git in place and the tree half filled, and git's
		// own cleanup never ran under SIGKILL. No run has worked in it, so it
		// is removed and made again.
		req.Emit(v1.Event{Kind: v1.EventStatus, Status: fmt.Sprintf("the worktree of %s was left half made; making it again", it.git.name)})
		if _, err := m.git(ctx, cache, "worktree", "remove", "--force", "--force", "--", it.dest); err != nil {
			return failed(fmt.Errorf("%w — the half-made worktree at %s could not be removed; start a new session", err, it.dest))
		}
		if err := os.MkdirAll(it.dest, 0o700); err != nil {
			return failed(err)
		}
	}
	req.Emit(v1.Event{Kind: v1.EventStatus, Status: "fetching " + it.git.url})
	if err := m.fetch(ctx, cache, *it.git); err != nil {
		return failed(err)
	}
	args, from, err := m.addArgs(ctx, cache, it)
	if err != nil {
		return failed(err)
	}
	if _, err := m.git(ctx, cache, args...); err != nil {
		return failed(fmt.Errorf("%w — %s", err, "the branch may be checked out by another session, or the workdir not empty"))
	}
	gitDir, err := m.git(ctx, it.dest, "rev-parse", "--absolute-git-dir")
	if err == nil {
		err = os.WriteFile(filepath.Join(gitDir, checkedOut), nil, 0o600)
	}
	if err != nil {
		return failed(fmt.Errorf("the worktree's checkout could not be recorded: %w", err))
	}
	req.Emit(v1.Event{Kind: v1.EventStatus, Status: fmt.Sprintf("worktree of %s on %s from %s", it.git.name, it.branch, from)})
	return true, nil
}

// fetch brings the bare cache up to date, creating it on first use. The cache
// is init --bare plus an origin with a normal fetch refspec, rather than
// clone --bare: a bare clone maps the remote's branches onto its own, where
// a fetch would move a branch a session has checked out underneath it.
func (m *Manager) fetch(ctx context.Context, cache string, r remote) error {
	// The remote's tags are copied, forced and pruned, into a namespace of
	// YAD's own that only resolves a base: a tag the remote moved is moved
	// there rather than refusing the fetch, one it deleted is gone, and a tag
	// no branch reaches is there too. refs/tags is left to git's own
	// following, which never forces or prunes, because every worktree of the
	// cache shares it and a harness may tag its own work there.
	fetch := []string{"fetch", "--quiet", "--prune", "--no-recurse-submodules", "origin"}
	if _, err := os.Stat(cache); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
			return err
		}
		// Built aside and renamed in, so a failed first clone leaves no
		// half-made cache to trip the next run.
		tmp, err := os.MkdirTemp(filepath.Dir(cache), ".new-"+filepath.Base(cache)+"-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		for _, args := range [][]string{
			{"init", "--quiet", "--bare"},
			{"config", "remote.origin.url", r.url},
			{"config", "--add", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"},
			{"config", "--add", "remote.origin.fetch", "+refs/tags/*:" + remoteTags + "*"},
			// origin/HEAD follows the remote's default branch on every fetch
			// (git 2.48 and later; an older git ignores the setting, and base
			// asks the remote when origin/HEAD is missing).
			{"config", "remote.origin.followRemoteHEAD", "always"},
		} {
			if _, err := m.git(ctx, tmp, args...); err != nil {
				return err
			}
		}
		if _, err := m.git(ctx, tmp, fetch...); err != nil {
			return fetchFailed(err)
		}
		if err := os.Rename(tmp, cache); err != nil {
			return err
		}
		return nil
	} else if err != nil {
		return err
	}
	if _, err := m.git(ctx, cache, fetch...); err != nil {
		return fetchFailed(err)
	}
	return nil
}

// remoteTags is where the cache keeps the remote's tags, as the remote has
// them now. Outside refs/tags, so a fetch never touches a tag a harness made.
const remoteTags = "refs/yad/origin-tags/"

// checkedOut marks a worktree whose add finished, in the worktree's own git
// directory. Without it, a worktree is one an add left half made.
const checkedOut = "yad-checked-out"

// fetchFailed says what the owner checks. The URL is the hub's, so it is
// never written into a command for someone to paste: a URL holding "$(…)" is
// valid to git and would run in their shell.
func fetchFailed(err error) error {
	return fmt.Errorf("%w — the runner reaches repositories with its own credentials (gh, SSH); check that its user can run git ls-remote on the repository's URL", err)
}

// addArgs is the worktree add for the run's branch: the branch as it stands
// when the cache already has it — a session's own, or one another session left
// — else the remote's branch of that name, else a new branch cut from base.
// from says which, for the run's events.
func (m *Manager) addArgs(ctx context.Context, cache string, it item) (args []string, from string, err error) {
	has := func(ref string) bool {
		_, err := m.git(ctx, cache, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
		return err == nil
	}
	switch {
	case has("refs/heads/" + it.branch):
		return []string{"worktree", "add", "--quiet", "--", it.dest, it.branch}, "the branch as it stands", nil
	case has("refs/remotes/origin/" + it.branch):
		return []string{"worktree", "add", "--quiet", "--track", "-b", it.branch, "--", it.dest, "refs/remotes/origin/" + it.branch}, "origin/" + it.branch, nil
	}
	base, err := m.base(ctx, cache, it, has)
	if err != nil {
		return nil, "", err
	}
	from = strings.TrimPrefix(strings.TrimPrefix(base, "refs/remotes/"), remoteTags)
	return []string{"worktree", "add", "--quiet", "--no-track", "-b", it.branch, "--", it.dest, base}, from, nil
}

// base resolves where a new branch is cut from: the remote's default branch
// when the run names none, else the base as a branch of the remote, a tag, or
// a commit.
func (m *Manager) base(ctx context.Context, cache string, it item, has func(string) bool) (string, error) {
	if it.base == "" {
		if !has("refs/remotes/origin/HEAD") {
			// Asked of the remote only when needed: it is one more round
			// trip, and a remote whose default branch never moves has
			// answered once.
			if _, err := m.git(ctx, cache, "remote", "set-head", "origin", "--auto"); err != nil {
				return "", fmt.Errorf("%w — the run names no base, and the remote's default branch could not be found; name one", err)
			}
		}
		return "refs/remotes/origin/HEAD", nil
	}
	for _, ref := range []string{"refs/remotes/origin/" + it.base, remoteTags + it.base} {
		if has(ref) {
			return ref, nil
		}
	}
	if isHex(it.base) && has(it.base) {
		return it.base, nil
	}
	return "", fmt.Errorf("base %q is not a branch, tag or commit of %s", it.base, it.git.url)
}

func isHex(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}

// samePath compares two paths as the filesystem resolves them: a temporary
// directory on macOS is reached through /var and named by git as
// /private/var.
func samePath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && ra == rb
}

// lockRepo holds one repository's cache while it is changed — a fetch, a
// worktree added or removed. git's own lockfiles (config.lock, packed-refs.lock)
// fail rather than wait, so two changes at once would fail one run for nothing.
// It gives up when the run is cancelled. The cache's name is the key: git
// names the same directory by its resolved path.
func (m *Manager) lockRepo(ctx context.Context, cache string) (func(), error) {
	key := filepath.Base(cache)
	m.mu.Lock()
	ch, ok := m.repos[key]
	if !ok {
		ch = make(chan struct{}, 1)
		m.repos[key] = ch
	}
	m.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Reclaim undoes what Prepare left in a session's workdir: its worktrees are
// removed from their bare caches — uncommitted work in them included — and the
// session's slots are freed for the next worktree. Branches stay in the cache;
// a path source is never touched, and a symlink to one is not followed.
// Deleting the workdir itself is the caller's, once this has returned.
//
// Workdir collection (DEV-18) calls it; nothing does yet.
func (m *Manager) Reclaim(ctx context.Context, connection, session, dir string) error {
	m.init()
	candidates := []string{dir}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() && e.Type()&os.ModeSymlink == 0 {
				candidates = append(candidates, filepath.Join(dir, e.Name()))
			}
		}
	}
	repos, err := filepath.EvalSymlinks(filepath.Join(m.Data, "repos"))
	var errs []error
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, err)
	}
	for _, wt := range candidates {
		if repos == "" {
			break
		}
		if fi, err := os.Lstat(filepath.Join(wt, ".git")); err != nil || fi.IsDir() {
			// A worktree's .git is a file; a directory is somebody's clone.
			continue
		}
		common, err := m.git(ctx, wt, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", wt, err))
			continue
		}
		if c, err := filepath.EvalSymlinks(common); err != nil || !within(repos, c) {
			continue
		}
		unlock, err := m.lockRepo(ctx, common)
		if err != nil {
			return errors.Join(append(errs, err)...)
		}
		// Twice forced: a worktree git has marked locked is still this
		// session's to remove.
		if _, err := m.git(ctx, common, "worktree", "remove", "--force", "--force", "--", wt); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", wt, err))
		}
		unlock()
	}
	if err := m.Slots.ReleaseSlots(ctx, connection, session); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
