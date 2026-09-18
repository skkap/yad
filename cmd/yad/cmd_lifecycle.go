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
		return &control.RunningError{PID: pid, Socket: g.paths.Socket()}
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
			return fmt.Errorf("the daemon exited as it started (%v)%s — `yad daemon logs` has the rest", err, tailOf(stderrPath))
		case <-deadline:
			cmd.Process.Signal(syscall.SIGTERM)
			return fmt.Errorf("the daemon (pid %d) did not answer on its control socket within %s, and was stopped%s — `yad daemon logs` says why, or start with a longer --wait", pid, s.wait, tailOf(stderrPath))
		case <-ctx.Done():
			return fmt.Errorf("interrupted while the daemon (pid %d) was starting — `yad daemon status` says whether it came up", pid)
		case <-tick.C:
			actx, cancel := context.WithTimeout(ctx, time.Second)
			res, err := control.Ask(actx, g.paths, "status")
			cancel()
			if err == nil && res.PID == pid {
				fmt.Fprintf(w, "started — pid %d, profile %s\n", pid, g.paths.Profile)
				fmt.Fprintln(w, "`yad status` shows what it is doing, `yad daemon logs -f` follows its log, `yad daemon stop` stops it")
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
var (
	termGrace = 10 * time.Second
	killGrace = 5 * time.Second
)

type stopFlags struct {
	timeout time.Duration
	force   bool
}

func (s *stopFlags) register(fs *flag.FlagSet) {
	fs.DurationVar(&s.timeout, "timeout", time.Minute, "how long to wait for a graceful stop")
	fs.BoolVar(&s.force, "force", false, "after --timeout, signal the daemon: SIGTERM, then SIGKILL")
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
// process to go. A daemon that holds its lock and does not answer is sent
// SIGTERM instead — the kill fallback's first step, which is still a graceful
// stop to a process that is listening for signals. SIGKILL is only ever
// --force's, after the timeout.
func stopDaemon(ctx context.Context, g global, s stopFlags, w io.Writer) error {
	var pid int
	res, err := control.Ask(ctx, g.paths, "stop")
	var wedged *control.UnresponsiveError
	switch {
	case errors.Is(err, control.ErrNotRunning):
		fmt.Fprintf(w, "not running — profile %s has no daemon to stop\n", g.paths.Profile)
		return nil
	case errors.As(err, &wedged):
		pid = wedged.PID
		if err := sendSignal(pid, syscall.SIGTERM); err != nil {
			return err
		}
		fmt.Fprintf(w, "the daemon (pid %d) did not answer on its control socket; sent it SIGTERM\n", pid)
	case err != nil:
		return err
	default:
		pid = res.PID
		fmt.Fprintf(w, "stopping — pid %d\n", pid)
	}
	if gone(ctx, g.paths, pid, s.timeout) {
		fmt.Fprintln(w, "stopped")
		return nil
	}
	if !s.force {
		return fmt.Errorf("pid %d is still stopping after %s — `yad status` shows the runs it is waiting on; wait longer with --timeout, or `yad daemon stop --force` signals it", pid, s.timeout)
	}
	if err := sendSignal(pid, syscall.SIGTERM); err != nil {
		return err
	}
	fmt.Fprintf(w, "sent pid %d SIGTERM\n", pid)
	if gone(ctx, g.paths, pid, termGrace) {
		fmt.Fprintln(w, "stopped")
		return nil
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
