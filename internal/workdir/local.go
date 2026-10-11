//go:build unix

package workdir

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxShownChanges is how many uncommitted paths a LocalWork names: enough to
// say what is there, and a refusal stays one readable line.
const maxShownChanges = 5

// LocalWork is one git source of a session's workdir as it stands: the branch
// its worktree is on, and what of it is on this machine alone.
type LocalWork struct {
	// URL is the source's URL as the session keeps it, without a credential
	// (StoredSources); Shown is the same for a message.
	URL, Shown string
	// Branch is the branch the worktree is on, or the one Prepare would put
	// it on when the session has not made it yet.
	Branch string
	// Changes are paths with changes not committed, the first few of them,
	// and Changed how many there are in all.
	Changes []string
	Changed int
	// Unpushed is how many commits the worktree's HEAD has that the remote,
	// as this machine last heard it, has on no branch — and that the branch
	// of the same name there does not hold. Nothing is fetched: what the
	// harness pushed from here moved those refs as it pushed.
	Unpushed int
}

// Clean reports whether nothing of the source is on this machine alone.
func (w LocalWork) Clean() bool { return w.Changed == 0 && w.Unpushed == 0 }

// Local is what a session's workdir holds of each of its git sources that
// the remote does not: the check before a session is released (decision
// 0081), since the runner that takes it next builds the workdir again from
// the remote, and anything only here would be gone without a word. A path
// source has none — it is not carried at all — and is not listed. It reads
// the worktrees and fetches nothing, so it needs neither the network nor a
// credential.
func (m *Manager) Local(ctx context.Context, req Request) ([]LocalWork, error) {
	m.init()
	items, err := m.plan(req)
	if err != nil {
		return nil, err
	}
	var out []LocalWork
	for i, it := range items {
		if it.git == nil {
			continue
		}
		// The URL as the run named it, which is what the next runner's run
		// names too; it.git.url is the one this machine resolved.
		w := LocalWork{URL: withoutCredential(req.Sources[i].Git.URL), Shown: shownURL(it.git.url), Branch: it.branch}
		if fi, err := os.Lstat(filepath.Join(it.dest, ".git")); err != nil || fi.IsDir() {
			// No worktree yet — no run of the session got that far — so
			// there is nothing here to lose.
			out = append(out, w)
			continue
		}
		if b, err := m.git(ctx, it.dest, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil && strings.TrimSpace(b) != "" {
			w.Branch = strings.TrimSpace(b)
		}
		// Version 2: no line starts with a space, which git's output loses
		// to the trim m.git gives it.
		status, err := m.git(ctx, it.dest, "status", "--porcelain=v2", "--untracked-files=all")
		if err != nil {
			return nil, err
		}
		for line := range strings.Lines(status) {
			if path := changedPath(strings.TrimRight(line, "\n")); path != "" {
				w.Changed++
				if len(w.Changes) < maxShownChanges {
					w.Changes = append(w.Changes, path)
				}
			}
		}
		w.Unpushed, err = m.unpushed(ctx, it.dest, w.Branch)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

// changedPath is the path one line of `git status --porcelain=v2` names, or ""
// for a line that names none (a header, or an ignored file).
func changedPath(line string) string {
	fields := func(n int) string {
		f := strings.SplitN(line, " ", n)
		if len(f) < n {
			return ""
		}
		path, _, _ := strings.Cut(f[n-1], "\t")
		return path
	}
	switch {
	case strings.HasPrefix(line, "? "):
		return line[2:]
	case strings.HasPrefix(line, "1 "):
		return fields(9)
	case strings.HasPrefix(line, "2 "):
		return fields(10)
	case strings.HasPrefix(line, "u "):
		return fields(11)
	}
	return ""
}

// unpushed counts the commits HEAD has that origin does not. A branch the
// remote has by the same name and that holds HEAD has none, whatever else is
// true; otherwise every commit of HEAD that no branch of origin holds is one.
// HEAD on no commit of its own — a session that never committed — has none:
// the next runner cuts the same branch from the remote and lands where this
// one started.
func (m *Manager) unpushed(ctx context.Context, dir, branch string) (int, error) {
	if branch != "" {
		ref := "refs/remotes/origin/" + branch
		if _, err := m.git(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}"); err == nil {
			if _, err := m.git(ctx, dir, "merge-base", "--is-ancestor", "HEAD", ref); err == nil {
				return 0, nil
			}
		}
	}
	n, err := m.git(ctx, dir, "rev-list", "--count", "HEAD", "--not", "--remotes=origin")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(n))
}
