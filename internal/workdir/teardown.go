//go:build unix

package workdir

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// teardownPath is where a repository keeps its teardown hook, beside its
// setup hook and found by the same rules: the executable file in the
// worktree, as the session left it, never one it links to outside.
const teardownPath = ".worktree/teardown"

// teardownDone marks a worktree whose teardown has run, however it ended, so
// a reclaim tried again after a failed removal does not run it twice. It
// lives in the worktree's git directory and goes with it.
const teardownDone = "yad-teardown-done"

// hookEnvFile keeps, in the worktree's git directory, the WT_* values setup
// was given that a teardown cannot read back from the worktree: the harness
// may have switched branch since, and the cache's name holds the repository's
// only as far as a digest-suffixed directory name can.
const hookEnvFile = "yad-hook-env"

type savedHookEnv struct {
	Branch string `json:"branch"`
	Repo   string `json:"repo"`
}

// readHookEnv is the worktree's record, and false when it has none whole.
func readHookEnv(gitDir string) (savedHookEnv, bool) {
	var saved savedHookEnv
	b, err := os.ReadFile(filepath.Join(gitDir, hookEnvFile))
	ok := err == nil && json.Unmarshal(b, &saved) == nil && saved.Branch != "" && saved.Repo != ""
	return saved, ok
}

// keepHookEnv records h's branch and repository for the worktree's teardown,
// once: a continuing run leaves what its first run recorded. The record is
// written aside and renamed in, so a runner killed mid-write leaves none
// rather than a partial one — and one that is not whole anyway is written
// again, rather than kept for a teardown that cannot read it.
func keepHookEnv(h hookEnv) error {
	if _, ok := readHookEnv(h.gitDir); ok {
		return nil
	}
	b, err := json.Marshal(savedHookEnv{Branch: h.branch, Repo: h.repo})
	if err == nil {
		err = writeAside(filepath.Join(h.gitDir, hookEnvFile), b)
	}
	if err != nil {
		// Not quoted: it names a path under the owner's home (DEV-67).
		return &Error{Class: ClassSetupFailed, Msg: "the worktree's hook variables could not be recorded in its git directory — check free disk space on the runner"}
	}
	return nil
}

// writeAside creates or replaces path, 0600, whole or not at all.
func writeAside(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".yad-hook-env-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// teardown runs the repository's .worktree/teardown in a worktree Reclaim is
// about to remove, with the WT_* values its setup was given, so the stacks a
// run started go before the slot their ports came from is freed (decision
// 0080). It runs only in a worktree whose add finished and whose session
// still holds a slot for the repository — setup ran there, or could have —
// and at most once.
//
// Nothing it does fails the reclaim: a teardown that exits non-zero, hangs
// or will not start is logged, and the worktree is removed anyway. The error
// is the reclaim's to return, the worktree and slot kept, only when ctx ended
// — the daemon is stopping, and its next start runs the teardown again — or
// when the slot could not be read, so the retry can still tell the teardown
// which one it was.
func (m *Manager) teardown(ctx context.Context, connection, session, wt, common string) error {
	gitDir, err := m.git(ctx, wt, "rev-parse", "--absolute-git-dir")
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// rev-parse --git-common-dir answered a moment ago; a worktree git
		// can no longer read is removed as it stands.
		m.Log.Warn("a worktree's git directory could not be read, so its teardown hook is not run", "connection", connection, "session", session, "worktree", wt, "err", err)
		return nil
	}
	if _, err := os.Stat(filepath.Join(gitDir, checkedOut)); err != nil {
		return nil // half made: no run worked in it
	}
	if _, err := os.Stat(filepath.Join(gitDir, teardownDone)); err == nil {
		return nil
	}
	slot, held, err := m.Slots.HeldSlot(ctx, filepath.Base(common), connection, session)
	if err != nil {
		return fmt.Errorf("%s: reading the WT_SLOT its teardown hook is given: %w", wt, err)
	}
	if !held {
		return nil
	}
	log := m.Log.With("connection", connection, "session", session, "worktree", wt, "slot", slot)
	hook, why := findHook(wt, teardownPath)
	if hook == "" {
		if !strings.HasSuffix(why, "does not exist") {
			log.Info("a worktree's teardown hook is not run", "why", why)
		}
		return nil
	}
	h := m.teardownEnv(ctx, gitDir, wt, common, slot)
	log = log.With("repo", h.repo)

	// Its output is read and dropped: nobody waits on a reclaim to read it,
	// and the log is not a place for whatever a hook prints, a credential
	// included. Its exit status says what the owner needs to look for.
	_, _, err = m.runHook(ctx, hook, h, m.TeardownTimeout, func() {})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exit *exec.ExitError
	switch {
	case errors.Is(err, errHookLinks):
		// Run against whatever docker PATH finds, it would stop the wrong
		// stacks or none; not marked, so a retried reclaim tries again.
		log.Warn("a worktree's teardown hook was not run: the runner could not link its git, gh and docker overrides into its data directory — check that the directory is writable and the disk is not full; the worktree is removed anyway")
		return nil
	case errors.Is(err, errHookStart):
		log.Warn("a worktree's teardown hook could not be started — check that its #! line names an interpreter this runner has; the worktree is removed anyway")
	case errors.Is(err, errHookTimeout):
		log.Warn("a worktree's teardown hook did not finish within [workdirs] teardown_timeout and was stopped; the worktree is removed anyway — check for containers and volumes it left, and raise the timeout if it needs longer", "timeout", m.TeardownTimeout)
	case errors.As(err, &exit):
		log.Warn("a worktree's teardown hook failed; the worktree is removed anyway — check for containers and volumes it left", "exit", exit.ExitCode())
	case err != nil:
		log.Warn("a worktree's teardown hook failed; the worktree is removed anyway — check for containers and volumes it left", "err", err)
	default:
		log.Info("a worktree's teardown hook ran")
	}
	if err := os.WriteFile(filepath.Join(gitDir, teardownDone), nil, 0o600); err != nil {
		log.Warn("a worktree's teardown could not be marked done; if its removal fails, the retry runs the hook again", "err", err)
	}
	return nil
}

// teardownEnv is the WT_* contract for a worktree's teardown: what setup was
// given, read back. A worktree made before the values were recorded falls
// back to its current branch and the repository's name in the cache's.
func (m *Manager) teardownEnv(ctx context.Context, gitDir, wt, common string, slot int64) hookEnv {
	h := hookEnv{root: wt, main: common, slot: slot, gitDir: gitDir}
	if saved, ok := readHookEnv(gitDir); ok {
		h.branch, h.repo = saved.Branch, saved.Repo
		return h
	}
	h.branch, _ = m.git(ctx, wt, "symbolic-ref", "--quiet", "--short", "HEAD")
	h.repo = repoOfCache(filepath.Base(common))
	return h
}

// repoOfCache undoes cacheName: the repository's name, from its cache's.
func repoOfCache(cache string) string {
	name := strings.TrimSuffix(cache, ".git")
	if i := strings.LastIndexByte(name, '-'); i > 0 && len(name)-i-1 == 16 && isHex(name[i+1:]) {
		return name[:i]
	}
	return name
}
