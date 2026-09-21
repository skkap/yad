//go:build unix

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
)

// startBackground re-executes this binary as a foreground daemon in a session
// of its own, and returns once that daemon answers on its control socket — so
// a start that reports success has a daemon that is really up, and one that
// fails says why here rather than only in a log.
func startBackground(ctx context.Context, g global, s startFlags, w io.Writer) error {
	if pid, running, err := control.Holder(g.paths); err != nil {
		return err
	} else if running {
		return &control.RunningError{PID: pid, Socket: g.paths.Socket(), Paths: g.paths}
	}
	// What the daemon would refuse on its first line is refused here, where
	// the owner is looking.
	if err := control.CheckPath(g.paths.Socket()); err != nil {
		return err
	}
	if _, err := config.Load(g.paths); err != nil {
		return err
	}
	if err := g.paths.Ensure(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find the yad binary to start: %w", err)
	}
	if err := os.MkdirAll(g.paths.Logs(), 0o700); err != nil {
		return err
	}
	// The daemon logs to its own rotated file; stderr catches only what
	// happens before that file is open, and a panic. Each start replaces it.
	stderrPath := filepath.Join(g.paths.Logs(), "stderr.log")
	stderr, err := os.OpenFile(stderrPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer stderr.Close()

	cmd := exec.Command(exe, "--profile", g.paths.Profile, "daemon", "start", "--foreground", "--interval", s.interval.String())
	cmd.Stderr = stderr // stdin and stdout are /dev/null
	// A session of its own: no controlling terminal, so closing this one
	// sends it no SIGHUP, and Ctrl-C here does not reach it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the daemon: %w", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.After(s.wait)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-exited:
			return fmt.Errorf("the daemon exited as it started (%v)%s — `%s` has the rest", err, tailOf(stderrPath), g.paths.Command("daemon", "logs"))
		case <-deadline:
			// Stopped and waited for, so the lock it may hold is free
			// before the owner tries again.
			cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(termGrace):
				cmd.Process.Kill()
				<-exited
			}
			return fmt.Errorf("the daemon (pid %d) did not answer on its control socket within %s, and was stopped%s — `%s` says why, or start with a longer --wait", pid, s.wait, tailOf(stderrPath), g.paths.Command("daemon", "logs"))
		case <-ctx.Done():
			return fmt.Errorf("interrupted while the daemon (pid %d) was starting — `%s` says whether it came up", pid, g.paths.Command("daemon", "status"))
		case <-tick.C:
			actx, cancel := context.WithTimeout(ctx, time.Second)
			res, err := control.Ask(actx, g.paths, "status")
			cancel()
			// Answering is not enough: the runner has to be past the
			// setup that can still make it exit.
			if err == nil && res.PID == pid && res.Status != nil && res.Status.Ready {
				fmt.Fprintf(w, "started — pid %d, profile %s\n", pid, g.paths.Profile)
				fmt.Fprintf(w, "`%s` shows what it is doing, `%s` follows its log, `%s` stops it\n", g.paths.Command("status"), g.paths.Command("daemon", "logs", "-f"), g.paths.Command("daemon", "stop"))
				return nil
			}
		}
	}
}

// tailOf is the last lines of a file, for an error that would otherwise send
// the owner looking for it.
func tailOf(path string) string {
	b, err := os.ReadFile(path)
	if err != nil || len(strings.TrimSpace(string(b))) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return ":\n  " + strings.Join(lines, "\n  ") + "\n"
}

// Timings of the kill fallback. A daemon that has a SIGTERM gets this long to
// act on it before --force escalates. Variables, so tests do not wait them out.
//
// forceStep is the pause between --force's two SIGTERMs: apart, so the
// kernel does not merge them into one, and long enough for a harness that
// answers its interrupt promptly to end its run cancelled.
var (
	termGrace = 10 * time.Second
	killGrace = 5 * time.Second
	forceStep = time.Second
)

type stopFlags struct {
	timeout time.Duration
	force   bool
}

func (s *stopFlags) register(fs *flag.FlagSet) {
	fs.DurationVar(&s.timeout, "timeout", time.Minute, "how long to wait for a graceful stop")
	fs.BoolVar(&s.force, "force", false, "after --timeout, signal the daemon: SIGTERM twice (cancel its runs, then exit now), then SIGKILL")
}

func daemonStop(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("daemon stop", flag.ContinueOnError)
	var s stopFlags
	s.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if s.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive, got %s — the default is 1m", s.timeout)
	}
	return stopDaemon(ctx, g, s, w)
}

// stopDaemon asks for a graceful stop through the socket and waits for the
// process to go. A daemon that holds its lock and never acknowledged the ask
// is sent SIGTERM instead — the kill fallback's first step, which is still a
// graceful stop to a process that is listening for signals, and the first
// stop it hears, because an unacknowledged ask is never acted on (see
// control.OpStop). An ask that was acknowledged and confirmed is delivered,
// answered or not: nothing but --force signals after it, and SIGKILL is only
// ever --force's, after the timeout.
func stopDaemon(ctx context.Context, g global, s stopFlags, w io.Writer) error {
	pid, err := control.Stop(ctx, g.paths)
	var wedged *control.UnresponsiveError
	var unanswered *control.UnansweredStopError
	switch {
	case errors.Is(err, control.ErrNotRunning):
		fmt.Fprintf(w, "not running — profile %s has no daemon to stop\n", g.paths.Profile)
		return nil
	case errors.As(err, &unanswered):
		pid = unanswered.PID
		fmt.Fprintf(w, "stopping — pid %d took the request, and its answer did not arrive (%v)\n", pid, unanswered.Err)
	case errors.As(err, &wedged):
		pid = wedged.PID
		// A Ctrl-C during the ask is not a daemon that failed to answer.
		if err := interrupted(ctx, g.paths, pid); err != nil {
			return err
		}
		if err := sendSignal(pid, syscall.SIGTERM); err != nil {
			return err
		}
		fmt.Fprintf(w, "the daemon (pid %d) did not answer on its control socket; sent it SIGTERM\n", pid)
	case err != nil:
		return err
	default:
		fmt.Fprintf(w, "stopping — pid %d\n", pid)
	}
	if gone(ctx, g.paths, pid, s.timeout) {
		fmt.Fprintln(w, "stopped")
		return nil
	}
	if err := interrupted(ctx, g.paths, pid); err != nil {
		return err
	}
	if !s.force {
		return fmt.Errorf("pid %d is still stopping after %s — `%s` shows whether it is stopping and the runs it is waiting on; wait longer with --timeout, or `%s` signals it", pid, s.timeout, g.paths.Command("status"), g.paths.Command("daemon", "stop", "--force"))
	}
	// The runner counts its owner's stop requests (decision 0029), and the
	// stop above was the first: a SIGTERM is the second, which cancels the
	// runs held, and another the third, exit now. Exit now is the runner
	// killing its harnesses' process groups itself — a SIGKILL of the runner
	// would leave them running — so SIGKILL is only for a runner deaf to both.
	for i, step := range []string{"cancel the runs it holds", "exit now"} {
		if err := sendSignal(pid, syscall.SIGTERM); err != nil {
			return err
		}
		fmt.Fprintf(w, "sent pid %d SIGTERM (%s)\n", pid, step)
		wait := forceStep
		if i == 1 {
			wait = termGrace
		}
		if gone(ctx, g.paths, pid, wait) {
			fmt.Fprintln(w, "stopped")
			return nil
		}
		if err := interrupted(ctx, g.paths, pid); err != nil {
			return err
		}
	}
	if err := sendSignal(pid, syscall.SIGKILL); err != nil {
		return err
	}
	if !gone(ctx, g.paths, pid, killGrace) {
		return fmt.Errorf("pid %d survived SIGKILL — check it with ps; it may be stuck in the kernel", pid)
	}
	fmt.Fprintf(w, "killed pid %d — harness processes it started are in process groups of their own and may still be running; the runs it held are given up at the next start\n", pid)
	return nil
}

// sendSignal refuses a pid that cannot be a daemon: 0 and -1 mean "the
// group" and "everything" to kill(2).
func sendSignal(pid int, sig syscall.Signal) error {
	if pid <= 1 || pid == os.Getpid() {
		return fmt.Errorf("the daemon lock names pid %d, which cannot be a daemon — find the process with ps and stop it by hand", pid)
	}
	if err := syscall.Kill(pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	return nil
}

// gone waits for the daemon with this pid to release the profile's lock.
// interrupted is the error for a stop the owner cut short. A wait that ended
// because the command did is not one that elapsed: escalating on it would
// send the next signal at once, and SIGKILL could land before the runner has
// killed its harnesses' process groups.
func interrupted(ctx context.Context, p config.Paths, pid int) error {
	if ctx.Err() == nil {
		return nil
	}
	return fmt.Errorf("stop interrupted — pid %d is left as it is; `%s` shows it: %w", pid, p.Command("daemon", "status"), ctx.Err())
}

func gone(ctx context.Context, p config.Paths, pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		holder, running, err := control.Holder(p)
		if err == nil && (!running || holder != pid) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// daemonRestart checks what it can before it stops anything: a daemon that
// is working is not traded for one that cannot sync (Multica #5165).
func daemonRestart(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("daemon restart", flag.ContinueOnError)
	var start startFlags
	var stop stopFlags
	start.register(fs)
	stop.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := start.check(); err != nil {
		return err
	}
	if stop.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive, got %s — the default is 1m", stop.timeout)
	}
	if err := preflight(g.paths); err != nil {
		return fmt.Errorf("restart refused, and the running daemon left alone: %w", err)
	}
	if _, running, err := control.Holder(g.paths); err != nil {
		return err
	} else if running {
		if err := stopDaemon(ctx, g, stop, w); err != nil {
			return err
		}
	}
	return startBackground(ctx, g, start, w)
}

// preflight is what a new daemon needs and can be checked without it: a
// config that loads, a socket path the kernel takes, and a credential for
// every connection that is present, private and well-formed. It makes no
// protocol call — see config.Paths.CheckCredential.
func preflight(p config.Paths) error {
	cfg, err := config.Load(p)
	if err != nil {
		return err
	}
	errs := []error{control.CheckPath(p.Socket())}
	for _, c := range cfg.Connections {
		errs = append(errs, p.CheckCredential(c.Name))
	}
	return errors.Join(errs...)
}
