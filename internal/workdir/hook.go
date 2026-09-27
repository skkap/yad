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

	"github.com/skkap/yad/internal/hostool"
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

// hookEnv is the WT_* contract, documented for repository authors in
// docs/setup-hooks.md.
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

	// The hook is a run's child like the harness, and a `docker compose up`
	// in it should reach the docker the capability document advertised. The
	// error is not quoted: it names the profile's data directory.
	path, err := hostool.Links(hostool.LinksDir(m.Data))
	if err != nil {
		return &Error{Class: ClassSetupFailed, Msg: fmt.Sprintf("%s in %s was not started: the runner could not link its git, gh and docker overrides into its data directory — check that the directory is writable and the disk is not full", hookPath, h.repo)}
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
	if path != "" {
		env = append(env, path)
	}
	p, err := supervise.Start(ctx, supervise.Spec{Path: hook, Dir: h.root, Env: env, NoTTY: true, MergeStderr: true})
	if err != nil {
		// Not the exec error: it names the hook by its absolute path, under the
		// owner's home. findHook has proved it an executable file inside the
		// checkout, so what is left is the interpreter its #! line names.
		return &Error{Class: ClassSetupFailed, Msg: fmt.Sprintf("%s in %s could not be started — check that its #! line names an interpreter this runner has", hookPath, h.repo)}
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
		return "", hookPath + " cannot be read — check its permissions and where it links"
	}
	if r, err := filepath.EvalSymlinks(root); err != nil || !within(r, real) {
		return "", hookPath + " links outside the repository, so it is not run"
	}
	fi, err := os.Stat(real)
	switch {
	case err != nil:
		return "", hookPath + " cannot be read — check its permissions and where it links"
	case !fi.Mode().IsRegular():
		return "", hookPath + " is not a file"
	case fi.Mode().Perm()&0o111 == 0:
		return "", hookPath + " is not executable"
	}
	return real, ""
}

// writeMarker's error is not quoted, nor findHook's: both name a path in the
// worktree, under the owner's home, and the run's error and status events go
// to a hub (DEV-67).
func writeMarker(path string) error {
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return &Error{Class: ClassSetupFailed, Msg: "the worktree's setup could not be marked done in its git directory — check free disk space on the runner"}
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
