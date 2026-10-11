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

	v1 "github.com/skkap/yad/protocol/v1"
)

// A branch the hub names is one a later session may name too: one run builds
// it, another verifies or merges it. git lets one worktree have a branch
// checked out at a time, so a session lets go of its branch when a run of it
// ends with the branch pushed and clean, and takes it back at its next run
// (decision 0080).

// Holders names the session a workdir belongs to, for a branch another
// session holds to be refused naming it. The runner's store is the one
// implementation.
type Holders interface {
	SessionOf(ctx context.Context, workdir string) (connection, session string, ok bool, err error)
}

const (
	// namedBranch marks, in a worktree's own git directory, the branch the
	// hub named for it: the one branch Settle may let go of. A session's own
	// yad/<connection>/<session> has none, since no other session names it.
	namedBranch = "yad-branch"
	// releasedBranch marks a worktree Settle took off its branch, and holds
	// the branch's name. It is written before the detach, so a runner that
	// stops between the two comes back to a mark beside a HEAD still on the
	// branch, and drops it. A detached HEAD with no mark is the harness's own
	// doing, and is left as it is.
	releasedBranch = "yad-detached"
)

// PushedAndClean reports whether the worktree at dir, on branch, leaves
// nothing behind that only it has: no change to a tracked file, and HEAD
// contained in origin/<branch> as the cache last saw it. A push to origin
// moves that; a push to a URL does not, so such a worktree reads as unpushed.
// Untracked files do not count: they stay in the worktree whatever becomes of
// the branch. When it is not, why says what is left, for a person.
func (m *Manager) PushedAndClean(ctx context.Context, dir, branch string) (ok bool, why string, err error) {
	m.init()
	// No optional locks: the worktree may be another session's, with a
	// harness's own git at work in it, and a status that refreshes the index
	// would take index.lock out from under it.
	out, err := m.git(ctx, dir, "--no-optional-locks", "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, "", err
	}
	if out != "" {
		return false, "uncommitted changes on it", nil
	}
	remote := "refs/remotes/origin/" + branch
	if _, err := m.git(ctx, dir, "rev-parse", "--verify", "--quiet", remote); err != nil {
		if exitCode(err) == 1 {
			return false, "commits on it never pushed to origin", nil
		}
		return false, "", err
	}
	in, err := m.ancestor(ctx, dir, "HEAD", remote)
	if err != nil {
		return false, "", err
	}
	if !in {
		return false, "commits on it that origin/" + branch + " does not have", nil
	}
	return true, "", nil
}

// Settle lets go of the branch the hub named for each of the session's
// worktrees under dir, when the run that has just ended left it pushed and
// clean: the worktree is detached at the commit it is on, so not a file in it
// changes, and another session may check the branch out. The session's next
// run puts it back (reattach). A worktree with changes or unpushed commits
// keeps its branch, and a session naming the branch is refused, naming this
// one. It is called when a run ends, never when it parks: a parked run comes
// back to the tree it left, branch and all.
func (m *Manager) Settle(ctx context.Context, dir string) error {
	return m.SettleUnless(ctx, dir, nil)
}

// SettleUnless is Settle for a caller that does not hold the session: busy
// is asked under each cache's lock, just before a worktree is let go of, and
// true keeps it as it is — a run of the session claimed meanwhile, whose
// preparation takes the same lock and so has either finished, and is to be
// left alone, or not started, and will find the release mark.
func (m *Manager) SettleUnless(ctx context.Context, dir string, busy func(context.Context) bool) error {
	m.init()
	if dir == "" {
		return nil
	}
	repos, err := filepath.EvalSymlinks(filepath.Join(m.Data, "repos"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	candidates := []string{dir}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() && e.Type()&os.ModeSymlink == 0 {
				candidates = append(candidates, filepath.Join(dir, e.Name()))
			}
		}
	}
	var errs []error
	for _, wt := range candidates {
		if err := m.settle(ctx, repos, wt, busy); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", wt, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) settle(ctx context.Context, repos, wt string, busy func(context.Context) bool) error {
	if fi, err := os.Lstat(filepath.Join(wt, ".git")); err != nil || fi.IsDir() {
		return nil
	}
	common, err := m.git(ctx, wt, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if c, err := filepath.EvalSymlinks(common); err != nil || !within(repos, c) {
		return nil
	}
	gitDir, err := m.git(ctx, wt, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	branch, ok := readMark(gitDir, namedBranch)
	if !ok {
		return nil
	}
	unlock, err := m.lockRepo(ctx, common)
	if err != nil {
		return err
	}
	defer unlock()
	if busy != nil && busy(ctx) {
		return nil
	}
	head, err := m.git(ctx, wt, "symbolic-ref", "--quiet", "HEAD")
	if exitCode(err) == 1 {
		// Released already, or detached by the harness: either way not a
		// branch to let go of.
		return nil
	}
	if err != nil {
		return err
	}
	mark := filepath.Join(gitDir, releasedBranch)
	if head != "refs/heads/"+branch {
		return nil
	}
	ok, _, err = m.PushedAndClean(ctx, wt, branch)
	if err != nil || !ok {
		// One left by a runner that stopped mid-release would read as a
		// release that never happened, for a branch that is kept.
		_ = os.Remove(mark)
		return err
	}
	if err := os.WriteFile(mark, []byte(branch+"\n"), 0o600); err != nil {
		return err
	}
	if _, err := m.git(ctx, wt, "switch", "--quiet", "--detach"); err != nil {
		_ = os.Remove(mark)
		return err
	}
	return nil
}

// reattach puts a worktree Settle released back on its branch, for the
// session's next run. The cache is fetched first — a failure is said and
// gone past, since a continuing run naming no sources has no credential for
// a URL that needed one (0068) — and the branch is then taken as take
// takes it. Called with the cache locked.
func (m *Manager) reattach(ctx context.Context, req Request, it item, cache, gitDir string) error {
	branch, ok := readMark(gitDir, releasedBranch)
	if !ok {
		return nil
	}
	mark := filepath.Join(gitDir, releasedBranch)
	if err := checkRef("the released branch", branch); err != nil {
		return err
	}
	_, err := m.git(ctx, it.dest, "symbolic-ref", "--quiet", "HEAD")
	if err == nil {
		// The runner stopped between the mark and the detach: the branch
		// was never let go.
		if err := os.Remove(mark); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if exitCode(err) != 1 {
		return err
	}
	left, err := m.git(ctx, it.dest, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	req.Emit(v1.Event{Kind: v1.EventStatus, Status: "fetching " + shownURL(it.git.url)})
	if err := m.fetch(ctx, cache, *it.git); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req.Emit(v1.Event{Kind: v1.EventStatus, Status: fmt.Sprintf("%s could not be fetched, so %s is taken as this runner last saw it: %s",
			shownURL(it.git.url), branch, redactURLs(redactSource(err.Error(), it.git.url)))})
	}
	if err := m.take(ctx, req, it, cache, branch); err != nil {
		return err
	}
	tip, err := m.git(ctx, it.dest, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return err
	}
	in, err := m.ancestor(ctx, it.dest, left, tip)
	if err != nil {
		return err
	}
	if _, err := m.git(ctx, it.dest, "switch", "--quiet", branch); err != nil {
		return m.switchRefused(ctx, it.dest, branch, left, tip)
	}
	if err := os.Remove(mark); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	switch {
	case !in:
		req.Emit(v1.Event{Kind: v1.EventStatus, Status: fmt.Sprintf("back on %s at %s; it was rewritten while this session was away and no longer has %s, where this session left it — that commit is still in the worktree's reflog",
			branch, short(tip), short(left))})
	case tip != left:
		req.Emit(v1.Event{Kind: v1.EventStatus, Status: fmt.Sprintf("back on %s, which moved from %s to %s while this session was away", branch, short(left), short(tip))})
	default:
		req.Emit(v1.Event{Kind: v1.EventStatus, Status: fmt.Sprintf("back on %s at %s", branch, short(tip))})
	}
	return nil
}

// take readies a branch the cache has for the worktree at it.dest: refused,
// naming who has it, while another worktree has it checked out, and
// fast-forwarded to origin's when origin's has moved on from it. A branch
// that has diverged from origin's is taken as it stands, and the run told.
// Called with the cache locked.
func (m *Manager) take(ctx context.Context, req Request, it item, cache, branch string) error {
	path, held, err := m.holder(ctx, cache, branch, it.dest)
	if err != nil {
		return err
	}
	if held {
		return m.heldBy(ctx, req, it, path, branch)
	}
	local, remote := "refs/heads/"+branch, "refs/remotes/origin/"+branch
	l, err := m.git(ctx, cache, "rev-parse", "--verify", "--quiet", local)
	gone := exitCode(err) == 1
	if err != nil && !gone {
		return err
	}
	r, err := m.git(ctx, cache, "rev-parse", "--verify", "--quiet", remote)
	switch {
	case exitCode(err) == 1 && gone:
		// Deleted while a released session was away, here and on origin.
		return &Error{Class: ClassSourceFailed, Msg: fmt.Sprintf("branch %s was deleted, on this runner and on origin, while this session was away — start a new session, or push the branch again and send this run again", branch)}
	case exitCode(err) == 1 || err == nil && r == l:
		return nil
	case err != nil:
		return err
	case gone:
		// An all-zero old value: made only if nothing made it meanwhile.
		if _, err := m.git(ctx, cache, "update-ref", "-m", "yad: made again from origin", local, r, strings.Repeat("0", len(r))); err != nil {
			return err
		}
		req.Emit(v1.Event{Kind: v1.EventStatus, Status: fmt.Sprintf("%s was deleted on this runner while this session was away; made again from origin/%s at %s", branch, branch, short(r))})
		return nil
	}
	behind, err := m.ancestor(ctx, cache, l, r)
	if err != nil {
		return err
	}
	if behind {
		// Guarded by the old value: nothing else moves it while the cache
		// is locked, but a harness's own git in a worktree of it does not
		// take that lock.
		if _, err := m.git(ctx, cache, "update-ref", "-m", "yad: fast-forward to origin", local, r, l); err != nil {
			return err
		}
		req.Emit(v1.Event{Kind: v1.EventStatus, Status: fmt.Sprintf("%s fast-forwarded from %s to origin/%s at %s", branch, short(l), branch, short(r))})
		return nil
	}
	ahead, err := m.ancestor(ctx, cache, r, l)
	if err != nil || ahead {
		return err
	}
	counts, err := m.git(ctx, cache, "rev-list", "--left-right", "--count", l+"..."+r)
	if err != nil {
		return err
	}
	here, there, _ := strings.Cut(counts, "\t")
	req.Emit(v1.Event{Kind: v1.EventStatus, Status: fmt.Sprintf("%s and origin/%s have diverged — %s commits only here, %s only there; taken as it stands here, so merge or rebase before pushing",
		branch, branch, here, strings.TrimSpace(there))})
	return nil
}

// holder is the worktree other than dest that has branch checked out, if
// any. One whose directory is gone holds it all the same until it is pruned,
// so it is pruned here.
func (m *Manager) holder(ctx context.Context, cache, branch, dest string) (string, bool, error) {
	out, err := m.git(ctx, cache, "worktree", "list", "--porcelain")
	if err != nil {
		return "", false, err
	}
	pruned := false
	for block := range strings.SplitSeq(out, "\n\n") {
		var path, head string
		prunable := false
		for line := range strings.Lines(block) {
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "worktree "):
				path = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "branch "):
				head = strings.TrimPrefix(line, "branch ")
			case line == "prunable" || strings.HasPrefix(line, "prunable "):
				prunable = true
			}
		}
		if head != "refs/heads/"+branch || path == dest || samePath(path, dest) {
			continue
		}
		if prunable {
			if !pruned {
				if _, err := m.git(ctx, cache, "worktree", "prune"); err != nil {
					return "", false, err
				}
				pruned = true
			}
			continue
		}
		return path, true, nil
	}
	return "", false, nil
}

// heldBy is the run's error for a branch the worktree at path has checked
// out. It names the session only to its own hub: to another, a session id and
// a workdir's path are another hub's business (0064, 0065).
func (m *Manager) heldBy(ctx context.Context, req Request, it item, path, branch string) error {
	conn, session, known := m.sessionOf(ctx, path)
	var msg string
	switch {
	case known && conn == req.Connection:
		msg = fmt.Sprintf("branch %s is checked out by session %s", branch, session)
		if ok, why, err := m.PushedAndClean(ctx, path, branch); err == nil && !ok {
			msg += ", which left " + why
			if strings.HasPrefix(why, "commits") {
				msg += " (a push to a URL rather than to origin is not seen here)"
			}
		} else if err == nil {
			msg += ", which has a run going"
		}
		msg += ". A session lets go of a branch it named when one of its runs ends with the branch committed and pushed to origin — have session " +
			session + "'s next run do that, or close it, then send this run again; or name another branch"
	case known:
		msg = fmt.Sprintf("branch %s is checked out by a session of another hub on this runner; it lets go of the branch when one of its runs ends with it committed and pushed — send this run again then, or name another branch", branch)
	default:
		msg = fmt.Sprintf("branch %s is checked out by another worktree of this runner's copy of %s that belongs to no session — name another branch, or ask the runner's owner to remove that worktree", branch, it.git.name)
	}
	return &Error{Class: ClassSourceFailed, Msg: msg}
}

// sessionOf is the session whose workdir holds the worktree at wt: the
// workdir itself, or the one it lies in when a run had several sources. git
// names a worktree by its resolved path and the store by the one the runner
// built, which differ where the data directory is reached through a link.
func (m *Manager) sessionOf(ctx context.Context, wt string) (connection, session string, ok bool) {
	if m.Holders == nil {
		return "", "", false
	}
	data, _ := filepath.EvalSymlinks(m.Data)
	for _, dir := range []string{wt, filepath.Dir(wt)} {
		tries := []string{dir}
		if rest, ok := strings.CutPrefix(dir, data+string(filepath.Separator)); ok && data != "" && data != m.Data {
			tries = append(tries, filepath.Join(m.Data, rest))
		}
		for _, d := range tries {
			if c, s, ok, err := m.Holders.SessionOf(ctx, d); err == nil && ok {
				return c, s, true
			}
		}
	}
	return "", "", false
}

// switchRefused is the run's error for a worktree that could not go back onto
// its branch: files the branch now changes are in the way — untracked here,
// and added to the branch while the session was away. They are named, not
// git's message, which quotes the workdir's path.
func (m *Manager) switchRefused(ctx context.Context, dir, branch, left, tip string) error {
	changed := map[string]bool{}
	if out, err := m.git(ctx, dir, "diff", "--name-only", "--no-renames", "-z", left, tip); err == nil {
		for name := range strings.SplitSeq(out, "\x00") {
			changed[name] = true
		}
	}
	var hit []string
	if out, err := m.git(ctx, dir, "status", "--porcelain", "-z", "--untracked-files=all"); err == nil {
		for entry := range strings.SplitSeq(out, "\x00") {
			if len(entry) > 3 && changed[entry[3:]] {
				hit = append(hit, entry[3:])
			}
		}
	}
	if len(hit) == 0 {
		return &Error{Class: ClassSourceFailed, Msg: fmt.Sprintf("the session's worktree could not go back onto branch %s; it stays at %s, where its last run left it — close the session and start a new one on the branch", branch, short(left))}
	}
	const shown = 10
	names := strings.Join(hit[:min(len(hit), shown)], ", ")
	if len(hit) > shown {
		names += fmt.Sprintf(" and %d more", len(hit)-shown)
	}
	return &Error{Class: ClassSourceFailed, Msg: fmt.Sprintf("the session's worktree could not go back onto branch %s: %s would be overwritten by the branch's version, which another session committed while this one was away. It stays at %s, where its last run left it, with the files as they are — close the session and start a new one on the branch, or have the runner's owner move the files aside",
		branch, names, short(left))}
}

// markNamed records the branch the hub named for a worktree, when it named
// one. Written on every run, not only the first, so a worktree an earlier
// version made is marked by the first run of it this version prepares.
func markNamed(gitDir string, it item) error {
	if !it.named {
		return nil
	}
	if cur, ok := readMark(gitDir, namedBranch); ok && cur == it.branch {
		return nil
	}
	if err := os.WriteFile(filepath.Join(gitDir, namedBranch), []byte(it.branch+"\n"), 0o600); err != nil {
		return errors.New("the worktree's branch could not be recorded in its git directory — check free disk space on the runner")
	}
	return nil
}

// readMark is a mark's one line, and whether it is there.
func readMark(gitDir, name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(gitDir, name))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// ancestor reports whether a is contained in b.
func (m *Manager) ancestor(ctx context.Context, dir, a, b string) (bool, error) {
	_, err := m.git(ctx, dir, "merge-base", "--is-ancestor", a, b)
	if exitCode(err) == 1 {
		return false, nil
	}
	return err == nil, err
}

// exitCode is the exit status of a git that ran and failed; -1 for any other
// error, and 0 for none.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// short is a commit's name in an event.
func short(sha string) string {
	return sha[:min(len(sha), 12)]
}
