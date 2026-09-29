//go:build unix

package workdir

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/skkap/yad/protocol/v1"
)

// A version before decision 0068 kept a source URL's credential: in the bare
// cache's remote.origin.url, in the cache's directory name, which was keyed by
// the URL whole, and in state.db. What is here finds it in the caches and says
// what to cut from the text the store holds; the daemon does both at its start
// (runner.scrubSourceCredentials), since it alone writes state.db (decision
// 0043).

// Scrub is one credential an earlier version wrote into text: Old is the start
// of a URL up to and including its userinfo's '@', New the same start as
// withoutCredential leaves it. Replacing Old with New wherever it appears takes
// the credential out of every copy of the URL, whatever follows it — git's
// reason quotes the URL with a trailing slash, an event with none.
type Scrub struct{ Old, New string }

// scrubOf is the Scrub for raw, when it carries a credential.
func scrubOf(raw string) (Scrub, bool) {
	if withoutCredential(raw) == raw {
		return Scrub{}, false
	}
	// withoutCredential cuts one span out of the authority, which ends at
	// its last '@': the URL up to that '@' holds the whole difference.
	scheme, rest, _ := strings.Cut(raw, "://")
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	old := scheme + "://" + rest[:strings.LastIndex(rest[:end], "@")+1]
	return Scrub{Old: old, New: withoutCredential(old)}, true
}

// ScrubsOf is a Scrub for every git source in sources whose URL carries a
// credential.
func ScrubsOf(sources []v1.Source) []Scrub {
	var out []Scrub
	for _, s := range sources {
		if s.Git == nil {
			continue
		}
		if sc, ok := scrubOf(s.Git.URL); ok {
			out = append(out, sc)
		}
	}
	return out
}

// Forms is s as each encoding the store's text holds it may spell it: as it
// is, and inside a JSON string — a run's spec, an event's body — where Go
// escapes '&', '<' and '>', which a URL's userinfo may hold.
func (s Scrub) Forms() []Scrub {
	forms := []Scrub{s}
	if j := (Scrub{Old: jsonString(s.Old), New: jsonString(s.New)}); j != s {
		forms = append(forms, j)
	}
	return forms
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b[1 : len(b)-1])
}

// StaleCache is a bare cache an earlier version made from a URL whose userinfo
// carried a credential: the URL whole is its remote.origin.url, and its
// directory is named by a key no run makes any more.
type StaleCache struct {
	// Path is where it is.
	Path string
	// Target is the cache the URL without its credential names now, where
	// this one moves so the next run from the repository finds its branches.
	// Empty when that name is taken — by a cache made since, or another stale
	// one from the same repository with another credential: it stays where it
	// is, and a session with a worktree of it continues there (worktree).
	Target string
	// Scrub is what comes out of the text the store holds.
	Scrub Scrub
	// origin is remote.origin.url without the credential.
	origin string
}

// StaleCaches finds the stale caches under <data>/repos. It only reads: the
// daemon takes the credential out of state.db first, and then CleanCache, so
// a start that stops between the two finds the cache again next time, and the
// Scrub with it.
func (m *Manager) StaleCaches(ctx context.Context) ([]StaleCache, error) {
	m.init()
	// Resolved, so a path here is spelled as git spells the ones it writes
	// into a worktree's .git.
	repos, err := filepath.EvalSymlinks(filepath.Join(m.Data, "repos"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	caches, err := filepath.Glob(filepath.Join(repos, "*.git"))
	if err != nil {
		return nil, err
	}
	var out []StaleCache
	var errs []error
	taken := map[string]bool{}
	for _, cache := range caches {
		// Read before git is asked: every daemon start comes here, and a
		// config with no '@' in it holds no userinfo.
		b, err := os.ReadFile(filepath.Join(cache, "config"))
		if err != nil {
			// A directory without one is no cache of this runner's; one
			// that cannot be read may still hold a credential.
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if !bytes.Contains(b, []byte("@")) {
			continue
		}
		origin, err := m.git(ctx, cache, "config", "--get", "remote.origin.url")
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %s", cache, redactURLs(err.Error())))
			continue
		}
		sc, ok := scrubOf(origin)
		if !ok {
			continue
		}
		c := StaleCache{Path: cache, Scrub: sc, origin: withoutCredential(origin)}
		notLocal := func(string, string) (string, error) { return "", errors.New("not a remote") }
		if r, err := parseRemote(c.origin, notLocal); err == nil {
			target := filepath.Join(repos, cacheName(r))
			if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) && !taken[target] {
				c.Target = target
				taken[target] = true
			}
		}
		out = append(out, c)
	}
	return out, errors.Join(errs...)
}

// CleanCache moves c to its Target when it has one, then takes the
// credential out of its config. The config is last because it is how the
// cache is found: a start that stops before it finds the cache again, under
// either name, and finishes — at the new name it is its own target, which is
// taken, so it stays.
//
// The move keeps every session with a worktree of the cache: each worktree's
// .git file, which names the cache's path, is pointed at the new one before
// the rename. Until the rename it names a directory that is not there yet,
// which the next attempt finds and leaves as it is.
func (m *Manager) CleanCache(ctx context.Context, c StaleCache) error {
	m.init()
	unlock, err := m.lockRepo(ctx, c.Path)
	if err != nil {
		return err
	}
	defer unlock()
	cache := c.Path
	if c.Target != "" {
		moved, err := m.move(ctx, c.Path, c.Target)
		if err != nil {
			return err
		}
		if moved {
			cache = c.Target
		}
	}
	if _, err := m.git(ctx, cache, "config", "remote.origin.url", c.origin); err != nil {
		return fmt.Errorf("%s: %s", cache, redactURLs(err.Error()))
	}
	return nil
}

// move renames the cache at from to to, with its worktrees pointed there
// first; it reports false, having changed nothing, when to exists.
func (m *Manager) move(ctx context.Context, from, to string) (bool, error) {
	// Held, no run can make the target meanwhile: a fetch makes a cache
	// under its lock.
	unlock, err := m.lockRepo(ctx, to)
	if err != nil {
		return false, err
	}
	defer unlock()
	if _, err := os.Lstat(to); !errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	admins, err := filepath.Glob(filepath.Join(from, "worktrees", "*"))
	if err != nil {
		return false, err
	}
	moved := func(admin string) string { return filepath.Join(to, "worktrees", filepath.Base(admin)) }
	// Pointed back on any failure before the rename, so the sessions go on
	// in the cache where it is until a later start moves it.
	back := func(err error, pointed []string) (bool, error) {
		errs := []error{err}
		for _, admin := range pointed {
			if err := repoint(admin, moved(admin), admin); err != nil {
				errs = append(errs, fmt.Errorf("the worktree of %s could not be pointed back at it (%w) — its session fails until a later start moves the cache", admin, err))
			}
		}
		return false, errors.Join(errs...)
	}
	for i, admin := range admins {
		if err := repoint(admin, admin, moved(admin)); err != nil {
			return back(err, admins[:i])
		}
	}
	if err := os.Rename(from, to); err != nil {
		return back(err, admins)
	}
	return true, nil
}

// repoint makes the worktree whose admin directory in the cache is admin name
// to instead of from in its .git file. The admin directory's gitdir file
// names the worktree's .git, and neither moves with the cache, so both still
// hold after the rename. A .git that names anything but from — to, already,
// or a directory this is not — is left as it is.
func repoint(admin, from, to string) error {
	b, err := os.ReadFile(filepath.Join(admin, "gitdir"))
	if err != nil {
		// Not a worktree git still knows; git worktree prune's to drop.
		return nil
	}
	dotGit := strings.TrimSpace(string(b))
	if !filepath.IsAbs(dotGit) {
		dotGit = filepath.Join(admin, dotGit)
	}
	cur, err := os.ReadFile(dotGit)
	if err != nil {
		// The worktree is gone: nothing to point anywhere.
		return nil
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(cur)), "gitdir: ")
	if !ok {
		return nil
	}
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(dotGit), gitdir)
	}
	// Compared as spelled first: one side does not exist, before or after
	// the rename, and cannot be resolved.
	if filepath.Clean(gitdir) != filepath.Clean(from) && !samePath(gitdir, from) {
		return nil
	}
	return writeFileAtomic(dotGit, []byte("gitdir: "+to+"\n"))
}

// writeFileAtomic replaces path whole or not at all: a .git file half
// written is a worktree git cannot open.
func writeFileAtomic(path string, b []byte) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".yad-git-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(fi.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// sameOrigin reports whether common, a worktree's git directory, is a bare
// cache under <data>/repos whose origin, without a credential, is url: the
// same repository in a cache an earlier version named by the URL whole.
func (m *Manager) sameOrigin(ctx context.Context, common, url string) bool {
	repos, err := filepath.EvalSymlinks(filepath.Join(m.Data, "repos"))
	if err != nil {
		return false
	}
	c, err := filepath.EvalSymlinks(common)
	if err != nil || filepath.Dir(c) != repos {
		return false
	}
	origin, err := m.git(ctx, c, "config", "--get", "remote.origin.url")
	return err == nil && withoutCredential(origin) == url
}
