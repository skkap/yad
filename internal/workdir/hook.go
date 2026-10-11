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
	// gitDir is the worktree's own git directory, where its marks live.
	gitDir string
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
	marker := filepath.Join(h.gitDir, setupDone)
	if _, err := os.Stat(marker); err == nil {
		return nil
	}
	hook, why := findHook(h.root, hookPath)
	if hook == "" {
		if h.fresh {
			req.Emit(v1.Event{Kind: v1.EventStatus, Status: fmt.Sprintf("no setup hook in %s: %s — the worktree is a clean checkout", h.repo, why)})
		}
		return writeMarker(marker)
	}

	// Unique per worktree: two repositories of one name in a run are two
	// hooks, and a hub joins a call to its result by the id.
	id := "setup-" + h.repo + "-" + digest(h.root)
	out, truncated, err := m.runHook(ctx, hook, h, m.SetupTimeout, func() {
		req.Emit(v1.Event{Kind: v1.EventToolCall, Tool: &v1.ToolEvent{
			ID: id, Name: "setup hook", Input: fmt.Sprintf("%s in %s on %s, WT_SLOT=%d", hookPath, h.repo, h.branch, h.slot),
		}})
	})
	var exit *exec.ExitError
	switch {
	case errors.Is(err, errHookLinks):
		// Not quoted: the links' error names the profile's data directory.
		return &Error{Class: ClassSetupFailed, Msg: fmt.Sprintf("%s in %s was not started: the runner could not link its git, gh and docker overrides into its data directory — check that the directory is writable and the disk is not full", hookPath, h.repo)}
	case errors.Is(err, errHookStart):
		return &Error{Class: ClassSetupFailed, Msg: fmt.Sprintf("%s in %s could not be started — check that its #! line names an interpreter this runner has", hookPath, h.repo)}
	}
	req.Emit(v1.Event{Kind: v1.EventToolResult, Tool: &v1.ToolEvent{ID: id, Output: out, Truncated: truncated}})
	switch {
	case errors.Is(err, errHookTimeout):
		return &Error{Class: ClassSetupFailed, Msg: fmt.Sprintf("%s in %s did not finish within %s and was stopped — the worktree is kept, and the session's next run runs the hook again; the owner raises [workdirs] setup_timeout if it needs longer", hookPath, h.repo, m.SetupTimeout)}
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.As(err, &exit):
		msg := fmt.Sprintf("%s in %s exited %d", hookPath, h.repo, exit.ExitCode())
		if last := lastLine(out); last != "" {
			msg += ": " + last
		}
		return &Error{Class: ClassSetupFailed, Msg: msg + " — the worktree is kept, and the session's next run runs the hook again"}
	case err != nil:
		return &Error{Class: ClassSetupFailed, Msg: fmt.Sprintf("%s in %s: %v", hookPath, h.repo, err)}
	}
	return writeMarker(marker)
}

// What runHook failed at before the hook's own exit status: each caller says
// it in its own words.
var (
	errHookLinks   = errors.New("the runner could not link its git, gh and docker overrides into its data directory")
	errHookStart   = errors.New("the hook could not be started")
	errHookTimeout = errors.New("the hook did not finish within its timeout and was stopped")
)

// runHook runs a hook found by findHook in h.root with the WT_* contract, for
// at most timeout, and returns the last of its output. started is called once
// the hook is about to start, and not at all when it never gets that far.
//
// The error is errHookLinks or errHookStart when the hook never ran, and
// errHookTimeout when it was stopped; ctx's own error when ctx ended; an
// *exec.ExitError when it exited non-zero. The exec error is never returned:
// it names the hook by its absolute path, under the owner's home, and
// findHook has proved the hook an executable file inside the checkout, so
// what is left is the interpreter its #! line names.
func (m *Manager) runHook(ctx context.Context, hook string, h hookEnv, timeout time.Duration, started func()) (string, bool, error) {
	// The hook is a run's child like the harness, and a `docker compose` in it
	// should reach the docker the capability document advertised.
	path, err := hostool.Links(hostool.LinksDir(m.Data))
	if err != nil {
		return "", false, errHookLinks
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started()
	env := append(append([]string{}, noPrompt...), h.vars()...)
	if path != "" {
		env = append(env, path)
	}
	p, err := supervise.Start(hctx, supervise.Spec{Path: hook, Dir: h.root, Env: env, NoTTY: true, MergeStderr: true})
	if err != nil {
		return "", false, errHookStart
	}
	out, truncated := readTail(p, v1.MaxToolOutputBytes)
	werr := p.Wait()
	switch {
	case ctx.Err() != nil:
		return out, truncated, ctx.Err()
	case errors.Is(hctx.Err(), context.DeadlineExceeded):
		return out, truncated, errHookTimeout
	}
	return out, truncated, werr
}

// findHook returns the worktree's hook at rel, or "" and why there is none. A
// hook is the repository's own code, run as the runner's user like the
// harness after it; what is refused is a hook that is really a file outside
// the checkout.
func findHook(root, rel string) (string, string) {
	path := filepath.Join(root, rel)
	real, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", rel + " does not exist"
	}
	if err != nil {
		return "", rel + " cannot be read — check its permissions and where it links"
	}
	if r, err := filepath.EvalSymlinks(root); err != nil || !within(r, real) {
		return "", rel + " links outside the repository, so it is not run"
	}
	fi, err := os.Stat(real)
	switch {
	case err != nil:
		return "", rel + " cannot be read — check its permissions and where it links"
	case !fi.Mode().IsRegular():
		return "", rel + " is not a file"
	case fi.Mode().Perm()&0o111 == 0:
		return "", rel + " is not executable"
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
