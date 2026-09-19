//go:build unix

package workdir

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/supervise"
)

// hookPath is where a repository keeps its setup hook — the committed one
// only. gpiwt also looks in the main checkout and in the owner's hooks
// directory; a runner has neither, and the hook is the repository's.
const hookPath = ".worktree/setup"

// setupDone marks a worktree whose hook has succeeded. It lives in the
// worktree's own git directory, not in the tree, so the tree the harness sees
// stays clean.
const setupDone = "yad-setup-done"

// hookEnv is the WT_* contract (~/my/gpi-tools/docs/worktrees/README.md).
type hookEnv struct {
	root, main, branch, repo string
	slot                     int64
	// fresh is a worktree added by this run.
	fresh bool
}

func (h hookEnv) vars() []string {
	return []string{
		"WT_ROOT=" + h.root,
		// The bare cache: it holds every branch and the remote, which is what
		// a hook reads WT_MAIN for — except untracked files, which a runner's
		// repository has none of.
		"WT_MAIN=" + h.main,
		"WT_BRANCH=" + h.branch,
		"WT_SLUG=" + strings.ReplaceAll(h.branch, "/", "-"),
		"WT_REPO=" + h.repo,
		"WT_SLOT=" + strconv.FormatInt(h.slot, 10),
	}
}

// setup runs the repository's setup hook in a worktree, unless it has
// succeeded there already. A hook that fails fails the run (decision 0034):
// the harness would otherwise start in a checkout its repository says is not
// ready. The worktree stays, and the session's next run tries the hook again.
func (m *Manager) setup(ctx context.Context, req Request, h hookEnv) error {
	gitDir, err := m.git(ctx, h.root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return &Error{Class: ClassSourceFailed, Msg: err.Error()}
	}
	marker := filepath.Join(gitDir, setupDone)
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	hook, why := m.findHook(h.root)
	if hook == "" {
		if h.fresh {
			req.Emit(v1.Event{Kind: v1.EventStatus, Status: fmt.Sprintf("no setup hook in %s: %s — the worktree is a clean checkout", h.repo, why)})
		}
		return writeMarker(marker)
	}

	ctx, cancel := context.WithTimeout(ctx, m.SetupTimeout)
	defer cancel()
	// Unique per worktree: two repositories of one name in a run are two
	// hooks, and a hub joins a call to its result by the id.
	id := "setup-" + h.repo + "-" + digest(h.root)
	req.Emit(v1.Event{Kind: v1.EventToolCall, Tool: &v1.ToolEvent{
		ID: id, Name: "setup hook", Input: fmt.Sprintf("%s in %s on %s, WT_SLOT=%d", hookPath, h.repo, h.branch, h.slot),
	}})
	env := append(append([]string{}, noPrompt...), h.vars()...)
	p, err := supervise.Start(ctx, supervise.Spec{Path: hook, Dir: h.root, Env: env, NoTTY: true, MergeStderr: true})
	if err != nil {
		return &Error{Class: ClassSetupFailed, Msg: fmt.Sprintf("%s in %s could not be started: %v", hookPath, h.repo, err)}
	}
	out, truncated := readTail(p, v1.MaxToolOutputBytes)
	werr := p.Wait()
	req.Emit(v1.Event{Kind: v1.EventToolResult, Tool: &v1.ToolEvent{ID: id, Output: out, Truncated: truncated}})
	var exit *exec.ExitError
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &Error{Class: ClassSetupFailed, Msg: fmt.Sprintf("%s in %s did not finish within %s and was stopped — the worktree is kept, and the session's next run runs the hook again; the owner raises [workdirs] setup_timeout if it needs longer", hookPath, h.repo, m.SetupTimeout)}
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(werr, &exit):
		msg := fmt.Sprintf("%s in %s exited %d", hookPath, h.repo, exit.ExitCode())
		if last := lastLine(out); last != "" {
			msg += ": " + last
		}
		return &Error{Class: ClassSetupFailed, Msg: msg + " — the worktree is kept, and the session's next run runs the hook again"}
	case werr != nil:
		return &Error{Class: ClassSetupFailed, Msg: fmt.Sprintf("%s in %s: %v", hookPath, h.repo, werr)}
	}
	return writeMarker(marker)
}

// findHook returns the worktree's hook, or "" and why there is none. A hook is
// the repository's own code, run as the runner's user like the harness after
// it; what is refused is a hook that is really a file outside the checkout.
func (m *Manager) findHook(root string) (string, string) {
	path := filepath.Join(root, hookPath)
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", hookPath + " does not exist"
	}
	if err != nil {
		return "", fmt.Sprintf("%s cannot be read: %v", hookPath, err)
	}
	if r, err := filepath.EvalSymlinks(root); err != nil || !within(r, real) {
		return "", hookPath + " links outside the repository, so it is not run"
	}
	fi, err := os.Stat(real)
	switch {
	case err != nil:
		return "", fmt.Sprintf("%s cannot be read: %v", hookPath, err)
	case !fi.Mode().IsRegular():
		return "", hookPath + " is not a file"
	case fi.Mode().Perm()&0o111 == 0:
		return "", hookPath + " is not executable"
	}
	return real, ""
}

func writeMarker(path string) error {
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return &Error{Class: ClassSetupFailed, Msg: "the setup hook's completion could not be recorded: " + err.Error()}
	}
	return nil
}

// readTail reads a child's output to its end and keeps the last n bytes: a
// hook's closing lines say what it did and did not do. A descendant that left
// the hook's process group can hold the pipe open after the hook exits, so
// the read is given up a second after the exit.
func readTail(p *supervise.Process, n int) (string, bool) {
	var (
		mu    sync.Mutex
		buf   []byte
		total int
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		chunk := make([]byte, 32<<10)
		for {
			k, err := p.Stdout().Read(chunk)
			mu.Lock()
			total += k
			buf = append(buf, chunk[:k]...)
			if over := len(buf) - n; over > 0 {
				buf = append(buf[:0], buf[over:]...)
			}
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-p.Done():
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}
	p.Stdout().Close()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return strings.ToValidUTF8(string(buf), "�"), total > len(buf)
}

// lockPath takes a path source for this run, waiting while another run —
// in this runner or another profile's — holds it. The lock is on the
// directory itself, not on a file under the profile's data: every profile on
// the machine, and every OS user, locks the same thing, and nothing is
// written into the owner's directory to do it.
func (m *Manager) lockPath(ctx context.Context, path string, emit func(v1.Event)) (func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &Error{Class: ClassSourceFailed, Msg: "path lock: " + err.Error()}
	}
	waited := false
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, &Error{Class: ClassSourceFailed, Msg: "path lock: " + err.Error()}
		}
		if ok {
			return func() { f.Close() }, nil
		}
		if !waited {
			waited = true
			emit(v1.Event{Kind: v1.EventStatus, Status: "waiting for " + path + ", which another run is using"})
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// lockPoll is how often a run waiting for a path looks again. flock has no
// wait that a context can cancel.
const lockPoll = 200 * time.Millisecond
