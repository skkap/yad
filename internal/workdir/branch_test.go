//go:build unix

package workdir

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// fakeHolders is the runner store's lookup: a workdir at
// <data>/workdirs/<connection>/<session> is that session's.
type fakeHolders struct{ data string }

func (h fakeHolders) SessionOf(_ context.Context, dir string) (string, string, bool, error) {
	rel, err := filepath.Rel(filepath.Join(h.data, "workdirs"), dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", "", false, nil
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 {
		return "", "", false, nil
	}
	return parts[0], parts[1], true, nil
}

// prepareAs is prepare for a session of another connection.
func (f *fixture) prepareAs(connection, session string, sources ...v1.Source) (*Prepared, *events, error) {
	f.t.Helper()
	dir := filepath.Join(f.m.Data, "workdirs", connection, session)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	ev := &events{}
	p, err := f.m.Prepare(context.Background(), Request{Dir: dir, Connection: connection, Session: session, Sources: sources, Emit: ev.emit})
	if err == nil {
		f.t.Cleanup(p.Release)
	}
	return p, ev, err
}

// settle is a run of the session ending.
func (f *fixture) settle(session string) {
	f.t.Helper()
	if err := f.m.Settle(context.Background(), filepath.Join(f.m.Data, "workdirs", "hub", session)); err != nil {
		f.t.Fatal(err)
	}
}

// work is a harness's commit in a worktree.
func work(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sh(t, dir, "git", "add", "--", name)
	sh(t, dir, "git", "commit", "--quiet", "-m", name)
	return sh(t, dir, "git", "rev-parse", "HEAD")
}

// branchOf is the branch a worktree has checked out; empty when detached.
func branchOf(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// The ticket's case: session a pushes branch x and its run ends; session b
// names x, starts, commits and pushes. a comes back while b has it and is
// refused naming b; once b's run ends a is back on x at b's tip, and later
// at a commit pushed to origin while it was away.
func TestASessionLetsGoOfABranchItLeftPushedAndClean(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", map[string]string{"README": "v1\n"})
	src := gitSource(o.bare, "", "x")

	a, _, err := f.prepare("a", src)
	if err != nil {
		t.Fatal(err)
	}
	work(t, a.Dir, "a.txt", "a")
	sh(t, a.Dir, "git", "push", "--quiet", "origin", "x")
	f.settle("a")
	if b := branchOf(t, a.Dir); b != "" {
		t.Fatalf("a is still on %s after a run that left it pushed and clean", b)
	}

	b, _, err := f.prepare("b", src)
	if err != nil {
		t.Fatalf("b could not take the branch a let go of: %v", err)
	}
	if read(t, filepath.Join(b.Dir, "a.txt")) != "a" || branchOf(t, b.Dir) != "x" {
		t.Fatalf("b is on %q without a's commit", branchOf(t, b.Dir))
	}
	work(t, b.Dir, "b.txt", "b")
	sh(t, b.Dir, "git", "push", "--quiet", "origin", "x")

	_, _, err = f.prepare("a", src)
	if class(err) != ClassSourceFailed || !strings.Contains(err.Error(), "checked out by session b, which has a run going") ||
		strings.Contains(err.Error(), "fatal") || strings.Contains(err.Error(), b.Dir) {
		t.Fatalf("a coming back while b has the branch: %v", err)
	}

	f.settle("b")
	a2, ev, err := f.prepare("a", src)
	if err != nil {
		t.Fatalf("a coming back once b let go: %v", err)
	}
	if branchOf(t, a2.Dir) != "x" || read(t, filepath.Join(a2.Dir, "b.txt")) != "b" {
		t.Errorf("a is on %q without b's commit", branchOf(t, a2.Dir))
	}
	if s := ev.statuses(); !strings.Contains(s, "back on x, which moved from") {
		t.Errorf("a's run was not told the branch moved:\n%s", s)
	}

	// A commit pushed to origin by someone else while a is away.
	f.settle("a")
	sh(t, o.work, "git", "fetch", "--quiet")
	sh(t, o.work, "git", "checkout", "--quiet", "x")
	o.commit("upstream", map[string]string{"c.txt": "c"})
	a3, ev, err := f.prepare("a", src)
	if err != nil {
		t.Fatal(err)
	}
	if branchOf(t, a3.Dir) != "x" || read(t, filepath.Join(a3.Dir, "c.txt")) != "c" {
		t.Errorf("a is on %q without origin's commit", branchOf(t, a3.Dir))
	}
	if s := ev.statuses(); !strings.Contains(s, "x fast-forwarded from") {
		t.Errorf("a's run was not told of the fast-forward:\n%s", s)
	}
}

// A branch is let go of only when nothing on it is the session's alone.
// Untracked files do not count: they stay in the worktree whatever becomes
// of the branch.
func TestABranchWithWorkOnlyItsSessionHasIsKept(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(t *testing.T, dir string)
		why  string // empty: let go of
	}{
		{"uncommitted", func(t *testing.T, dir string) {
			sh(t, dir, "git", "push", "--quiet", "origin", "x")
			os.WriteFile(filepath.Join(dir, "README"), []byte("changed\n"), 0o644)
		}, "uncommitted changes on it"},
		{"never pushed", func(t *testing.T, dir string) {
			work(t, dir, "a.txt", "a")
		}, "commits on it never pushed to origin"},
		{"ahead of origin", func(t *testing.T, dir string) {
			sh(t, dir, "git", "push", "--quiet", "origin", "x")
			work(t, dir, "a.txt", "a")
		}, "commits on it that origin/x does not have (a push to a URL rather than to origin is not seen here)"},
		{"untracked only", func(t *testing.T, dir string) {
			sh(t, dir, "git", "push", "--quiet", "origin", "x")
			os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("mine"), 0o644)
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			o := newOrigin(t, f.root, "acme", map[string]string{"README": "v1\n"})
			src := gitSource(o.bare, "", "x")
			a, _, err := f.prepare("a", src)
			if err != nil {
				t.Fatal(err)
			}
			tc.do(t, a.Dir)
			f.settle("a")
			_, _, err = f.prepare("b", src)
			if tc.why == "" {
				if err != nil || branchOf(t, a.Dir) != "" {
					t.Errorf("b could not take the branch (%v); a is on %q", err, branchOf(t, a.Dir))
				}
				return
			}
			if branchOf(t, a.Dir) != "x" {
				t.Errorf("a was taken off its branch")
			}
			if class(err) != ClassSourceFailed || !strings.Contains(err.Error(), "checked out by session a, which left "+tc.why) ||
				strings.Contains(err.Error(), "fatal") || strings.Contains(err.Error(), a.Dir) {
				t.Errorf("b's error: %v", err)
			}
		})
	}
}

// A session of another hub is not named, nor is its connection: both are
// that hub's business.
func TestAHolderOfAnotherHubIsNotNamed(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", nil)
	src := gitSource(o.bare, "", "x")
	if _, _, err := f.prepareAs("zeta", "secret-session", src); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.prepare("b", src)
	if class(err) != ClassSourceFailed || !strings.Contains(err.Error(), "a session of another hub on this runner") ||
		strings.Contains(err.Error(), "secret-session") || strings.Contains(err.Error(), "zeta") || strings.Contains(err.Error(), f.m.Data) {
		t.Errorf("err = %v", err)
	}
}

// A branch rewritten on origin while its session was away, so that the
// cache's copy and origin's have each a commit the other lacks, is taken as
// the cache has it, and the run is told.
func TestABranchThatDivergedIsTakenAsItStands(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", nil)
	src := gitSource(o.bare, "", "x")
	a, _, err := f.prepare("a", src)
	if err != nil {
		t.Fatal(err)
	}
	mine := work(t, a.Dir, "a.txt", "a")
	sh(t, a.Dir, "git", "push", "--quiet", "origin", "x")
	f.settle("a")

	sh(t, o.work, "git", "checkout", "--quiet", "-b", "x")
	work(t, o.work, "t.txt", "t")
	sh(t, o.work, "git", "push", "--quiet", "--force", "origin", "x")

	a2, ev, err := f.prepare("a", src)
	if err != nil {
		t.Fatal(err)
	}
	if branchOf(t, a2.Dir) != "x" || sh(t, a2.Dir, "git", "rev-parse", "HEAD") != mine {
		t.Errorf("a is on %q at %s, want x at its own commit", branchOf(t, a2.Dir), sh(t, a2.Dir, "git", "rev-parse", "HEAD"))
	}
	if s := ev.statuses(); !strings.Contains(s, "x and origin/x have diverged — 1 commits only here, 1 only there") {
		t.Errorf("statuses:\n%s", s)
	}
}

// A branch another session rewrote and pushed takes the returning session
// with it, and the run is told which of its commits the branch no longer
// has.
func TestABranchRewrittenWhileAway(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", nil)
	src := gitSource(o.bare, "", "x")
	a, _, err := f.prepare("a", src)
	if err != nil {
		t.Fatal(err)
	}
	mine := work(t, a.Dir, "a.txt", "a")
	sh(t, a.Dir, "git", "push", "--quiet", "origin", "x")
	f.settle("a")

	b, _, err := f.prepare("b", src)
	if err != nil {
		t.Fatal(err)
	}
	sh(t, b.Dir, "git", "reset", "--quiet", "--hard", "HEAD~1")
	theirs := work(t, b.Dir, "b.txt", "b")
	sh(t, b.Dir, "git", "push", "--quiet", "--force", "origin", "x")
	f.settle("b")

	a2, ev, err := f.prepare("a", src)
	if err != nil {
		t.Fatal(err)
	}
	if head := sh(t, a2.Dir, "git", "rev-parse", "HEAD"); branchOf(t, a2.Dir) != "x" || head != theirs {
		t.Errorf("a is on %q at %s, want x at b's %s", branchOf(t, a2.Dir), head, theirs)
	}
	if s := ev.statuses(); !strings.Contains(s, "was rewritten while this session was away and no longer has "+mine[:12]) {
		t.Errorf("statuses:\n%s", s)
	}
}

// A continuing run naming no sources has no credential for a URL that needed
// one, so a fetch that fails is said and gone past.
func TestAFetchThatFailsDoesNotStopTheReturn(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", nil)
	src := gitSource(o.bare, "", "x")
	a, _, err := f.prepare("a", src)
	if err != nil {
		t.Fatal(err)
	}
	sh(t, a.Dir, "git", "push", "--quiet", "origin", "x")
	f.settle("a")

	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "git")
	script := "#!/bin/sh\nfor a in \"$@\"; do [ \"$a\" = fetch ] && { echo 'fatal: could not read from remote' >&2; exit 128; }; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.m.Git = wrapper
	a2, ev, err := f.prepare("a", src)
	if err != nil {
		t.Fatal(err)
	}
	if branchOf(t, a2.Dir) != "x" {
		t.Errorf("a is on %q", branchOf(t, a2.Dir))
	}
	if s := ev.statuses(); !strings.Contains(s, "could not be fetched, so x is taken as this runner last saw it") || !strings.Contains(s, "back on x") {
		t.Errorf("statuses:\n%s", s)
	}
}

// A new session naming a branch the cache has, behind origin's, starts at
// origin's: checked out as it stood, its first push would be refused.
func TestANewSessionFastForwardsAStaleBranch(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", nil)
	src := gitSource(o.bare, "", "x")
	a, _, err := f.prepare("a", src)
	if err != nil {
		t.Fatal(err)
	}
	sh(t, a.Dir, "git", "push", "--quiet", "origin", "x")
	if err := f.m.Reclaim(context.Background(), "hub", "a", a.Dir); err != nil {
		t.Fatal(err)
	}

	sh(t, o.work, "git", "fetch", "--quiet")
	sh(t, o.work, "git", "checkout", "--quiet", "x")
	o.commit("upstream", map[string]string{"c.txt": "c"})

	b, ev, err := f.prepare("b", src)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(b.Dir, "c.txt")) != "c" {
		t.Error("b started behind origin")
	}
	if s := ev.statuses(); !strings.Contains(s, "x fast-forwarded from") {
		t.Errorf("statuses:\n%s", s)
	}
	work(t, b.Dir, "b.txt", "b")
	sh(t, b.Dir, "git", "push", "--quiet", "origin", "x")
}

// A HEAD the harness detached itself is its business, and a session's own
// branch, named after it, is never let go of: no other session names it.
func TestOnlyTheBranchTheHubNamedIsLetGoOf(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", nil)
	a, _, err := f.prepare("a", gitSource(o.bare, "", "x"))
	if err != nil {
		t.Fatal(err)
	}
	sh(t, a.Dir, "git", "push", "--quiet", "origin", "x")
	sh(t, a.Dir, "git", "switch", "--quiet", "--detach")
	f.settle("a")
	_, ev, err := f.prepare("a", gitSource(o.bare, "", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if branchOf(t, a.Dir) != "" || strings.Contains(ev.statuses(), "back on") {
		t.Errorf("the harness's own detach was undone: on %q\n%s", branchOf(t, a.Dir), ev.statuses())
	}

	c, _, err := f.prepare("c", gitSource(o.bare, "", ""))
	if err != nil {
		t.Fatal(err)
	}
	sh(t, c.Dir, "git", "push", "--quiet", "origin", "yad/hub/c")
	f.settle("c")
	if b := branchOf(t, c.Dir); b != "yad/hub/c" {
		t.Errorf("the session's own branch was let go of: on %q", b)
	}
}

// Each of a run's worktrees lets go of its own branch.
func TestEverySourceLetsGoOfItsBranch(t *testing.T) {
	f := newFixture(t)
	acme := newOrigin(t, f.root, "acme", nil)
	tools := newOrigin(t, f.root, "tools", nil)
	srcs := []v1.Source{gitSource(acme.bare, "", "x"), gitSource(tools.bare, "", "y")}
	a, _, err := f.prepare("a", srcs...)
	if err != nil {
		t.Fatal(err)
	}
	sh(t, filepath.Join(a.Dir, "acme"), "git", "push", "--quiet", "origin", "x")
	sh(t, filepath.Join(a.Dir, "tools"), "git", "push", "--quiet", "origin", "y")
	f.settle("a")
	if _, _, err := f.prepare("b", srcs...); err != nil {
		t.Fatal(err)
	}
	_, _, err = f.prepare("a", srcs...)
	if class(err) != ClassSourceFailed || !strings.Contains(err.Error(), "checked out by session b") {
		t.Errorf("a coming back while b has both: %v", err)
	}
}

// A runner that stopped between writing the release mark and detaching left
// the branch where it was; the next run drops the mark and carries on.
func TestAReleaseCutShortKeepsTheBranch(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", nil)
	src := gitSource(o.bare, "", "x")
	a, _, err := f.prepare("a", src)
	if err != nil {
		t.Fatal(err)
	}
	gitDir := sh(t, a.Dir, "git", "rev-parse", "--absolute-git-dir")
	if err := os.WriteFile(filepath.Join(gitDir, releasedBranch), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, ev, err := f.prepare("a", src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(gitDir, releasedBranch)); !os.IsNotExist(err) {
		t.Errorf("the mark is still there: %v", err)
	}
	if branchOf(t, a.Dir) != "x" || strings.Contains(ev.statuses(), "back on") {
		t.Errorf("on %q\n%s", branchOf(t, a.Dir), ev.statuses())
	}
}

// A file the session left untracked, which another session has since
// committed on the branch, stops the return; the run says which file.
func TestFilesInTheWayOfTheReturnAreNamed(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", nil)
	src := gitSource(o.bare, "", "x")
	a, _, err := f.prepare("a", src)
	if err != nil {
		t.Fatal(err)
	}
	sh(t, a.Dir, "git", "push", "--quiet", "origin", "x")
	os.WriteFile(filepath.Join(a.Dir, "notes.txt"), []byte("a's"), 0o644)
	f.settle("a")

	b, _, err := f.prepare("b", src)
	if err != nil {
		t.Fatal(err)
	}
	work(t, b.Dir, "notes.txt", "b's")
	sh(t, b.Dir, "git", "push", "--quiet", "origin", "x")
	f.settle("b")

	_, _, err = f.prepare("a", src)
	if class(err) != ClassSourceFailed || !strings.Contains(err.Error(), "notes.txt would be overwritten") || strings.Contains(err.Error(), a.Dir) {
		t.Fatalf("err = %v", err)
	}
	if read(t, filepath.Join(a.Dir, "notes.txt")) != "a's" {
		t.Error("a's file was overwritten")
	}
	os.Remove(filepath.Join(a.Dir, "notes.txt"))
	if _, _, err := f.prepare("a", src); err != nil || branchOf(t, a.Dir) != "x" {
		t.Errorf("once the file was moved: %v, on %q", err, branchOf(t, a.Dir))
	}
}

// A branch deleted in the cache while its session was away is made again
// from origin's, and one deleted on origin too stops the return with a
// reason rather than git's.
func TestABranchDeletedWhileAway(t *testing.T) {
	f := newFixture(t)
	o := newOrigin(t, f.root, "acme", nil)
	src := gitSource(o.bare, "", "x")
	a, _, err := f.prepare("a", src)
	if err != nil {
		t.Fatal(err)
	}
	sh(t, a.Dir, "git", "push", "--quiet", "origin", "x")
	f.settle("a")
	caches, _ := filepath.Glob(filepath.Join(f.m.Data, "repos", "*.git"))
	if len(caches) != 1 {
		t.Fatalf("caches %v", caches)
	}
	sh(t, caches[0], "git", "branch", "--quiet", "-D", "x")
	_, ev, err := f.prepare("a", src)
	if err != nil || branchOf(t, a.Dir) != "x" || !strings.Contains(ev.statuses(), "made again from origin/x") {
		t.Fatalf("err %v, on %q\n%s", err, branchOf(t, a.Dir), ev.statuses())
	}

	f.settle("a")
	sh(t, caches[0], "git", "branch", "--quiet", "-D", "x")
	sh(t, o.work, "git", "push", "--quiet", "origin", "--delete", "x")
	_, _, err = f.prepare("a", src)
	if class(err) != ClassSourceFailed || !strings.Contains(err.Error(), "branch x was deleted, on this runner and on origin") {
		t.Errorf("err = %v", err)
	}
}
