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
	"github.com/skkap/yad/internal/service"
	"github.com/skkap/yad/internal/shellword"
)

// startBackground starts the profile's daemon and returns once it answers on
// its control socket. A profile that runs as a service is started by its
// service manager, which then watches it; any other is a detached process of
// its own (startDetached).
func startBackground(ctx context.Context, g global, s startFlags, w io.Writer) error {
	if pid, running, err := control.Holder(g.paths); err != nil {
		return err
	} else if running {
		return &control.RunningError{PID: pid, Socket: g.paths.Socket(), Paths: g.paths}
	}
	u, note, err := serviceOf(ctx, g.paths, 0)
	var unknown *unknownServiceError
	if errors.As(err, &unknown) {
		return fmt.Errorf("start refused: %w — start it through the service manager: `%s`, or run this again once `%s` answers", err, shellword.Command(unknown.m.StartCommand(g.paths.Profile)...), g.paths.Command("service", "status"))
	}
	if err != nil {
		return err
	}
	if u == nil {
		if note != "" {
			fmt.Fprintln(w, note)
		}
		return startDetached(ctx, g, s, w)
	}
	if err := u.refuses(s); err != nil {
		return fmt.Errorf("start refused: %w", err)
	}
	// What the daemon would refuse on its first line is refused here: under
	// a service manager it would only show as a runner restarting every few
	// seconds.
	if err := control.CheckPath(g.paths.Socket()); err != nil {
		return err
	}
	if _, err := config.Load(g.paths); err != nil {
		return err
	}
	return startService(ctx, g, u, s, 0, w)
}

// unit is the service-manager job a profile's runner is.
type unit struct {
	m    service.Manager
	name string
}

// refuses is a start flag the unit cannot carry: it runs the command it was
// installed with, and nothing typed here reaches it.
func (u *unit) refuses(s startFlags) error {
	if s.intervalSet {
		return fmt.Errorf("--interval cannot reach %s, which runs the command it was installed with — drop --interval", u.name)
	}
	return nil
}

// unknownServiceError is a profile whose unit file is there and whose service
// manager could not say whether it holds the job — no user bus, no GUI
// domain. Starting a detached daemon could be DEV-173 again, and refusing
// every start where there is no service would be its mirror; so the owner,
// who can tell, is given the manager's own command.
type unknownServiceError struct {
	m    service.Manager
	name string
	file string
	err  error
}

func (e *unknownServiceError) Error() string {
	return fmt.Sprintf("could not ask the service manager whether this profile runs as %s, whose unit is %s (%v)", e.name, e.file, e.err)
}

func (e *unknownServiceError) Unwrap() error { return e.err }

// serviceOf is the unit that runs this profile's runner, if one does. holder
// is the pid holding the profile's lock, or 0.
//
// A unit is the profile's when the manager's running job is the lock holder,
// or when its file names this profile's service log, which is under the
// profile's data directory. The label alone cannot say: it is only the
// profile's name, and yad.runner.default with the owner's directories is
// not the runner under another YAD_DATA_DIR. A unit the manager has not
// loaded is not started through it, and note says so.
//
// No service manager on this system, or one that cannot be asked and no
// unit file, is no service: absence is data, and the detached daemon is
// the only kind there is.
func serviceOf(ctx context.Context, p config.Paths, holder int) (u *unit, note string, err error) {
	m, _, err := serviceManager()
	if err != nil {
		return nil, "", nil
	}
	name := m.Name(p.Profile)
	st, err := m.Status(ctx, p.Profile)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		file := m.File(p.Profile)
		if _, serr := os.Stat(file); serr != nil {
			return nil, "", nil
		}
		return nil, "", &unknownServiceError{m: m, name: name, file: file, err: err}
	}
	ours := st.Installed && st.LogFile != "" && filepath.Clean(st.LogFile) == filepath.Clean(service.LogFile(p))
	switch {
	case st.Loaded && st.Running && holder > 0 && st.PID == holder, st.Loaded && ours:
		return &unit{m: m, name: name}, "", nil
	case ours:
		return nil, fmt.Sprintf("note: profile %s has a service, %s, that the service manager has not loaded, so this daemon runs outside it and nothing restarts it after a crash — to put it back under the service, `%s`, then `%s`", p.Profile, name, p.Command("daemon", "stop"), p.Command("service", "install")), nil
	}
	return nil, "", nil
}

// startService has the service manager start the unit, and returns once a
// daemon other than old — the one a restart stopped — answers ready on the
// control socket. A daemon that does not is the manager's to retry or give
// up on, and is left to it.
//
// The start is asked for only once the manager has no instance running. A
// daemon releases its lock a moment before it exits, and the manager takes a
// start in that moment for one already running — kickstart without -k leaves
// a running job alone — so a stop straight before would start nothing. An
// instance that is not old and not yet answering is one the manager has just
// started itself (a crash restart): it is waited for, not started again.
func startService(ctx context.Context, g global, u *unit, s startFlags, old int, w io.Writer) error {
	startCmd := shellword.Command(u.m.StartCommand(g.paths.Profile)...)
	deadline := time.After(s.wait)
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	asked, running := false, 0
	for {
		select {
		case <-deadline:
			if !asked {
				return fmt.Errorf("the service manager still has %s running as pid %d after %s, and it does not answer on the control socket — `%s` says what it knows, and `%s` starts the runner once that process has gone", u.name, running, s.wait, g.paths.Command("service", "status"), startCmd)
			}
			return fmt.Errorf("the service manager started %s, and no daemon answered on its control socket within %s%s — `%s` and `%s` say why, or wait longer with --wait", u.name, s.wait, tailOf(service.LogFile(g.paths)), g.paths.Command("service", "status"), g.paths.Command("daemon", "logs"))
		case <-ctx.Done():
			if !asked {
				return fmt.Errorf("interrupted before %s was started — `%s` starts it", u.name, startCmd)
			}
			return fmt.Errorf("interrupted while %s was starting — `%s` says whether it came up", u.name, g.paths.Command("daemon", "status"))
		case <-tick.C:
		}
		actx, cancel := context.WithTimeout(ctx, time.Second)
		res, err := control.Ask(actx, g.paths, "status")
		cancel()
		if err == nil && res.PID != old && res.Status != nil && res.Status.Ready {
			fmt.Fprintf(w, "started by the service manager as %s — pid %d, profile %s\n", u.name, res.PID, g.paths.Profile)
			printStarted(w, g.paths)
			return nil
		}
		if asked {
			continue
		}
		st, err := u.m.Status(ctx, g.paths.Profile)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			return fmt.Errorf("the service manager could not say whether %s is running (%w) — `%s` starts it", u.name, err, startCmd)
		}
		if st.Running {
			running = st.PID
			continue
		}
		if err := u.m.Start(ctx, g.paths.Profile); err != nil {
			return fmt.Errorf("the service manager did not start %s: %w — `%s` says what it knows of it", u.name, err, g.paths.Command("service", "status"))
		}
		asked = true
	}
}

func printStarted(w io.Writer, p config.Paths) {
	fmt.Fprintf(w, "`%s` shows what it is doing, `%s` follows its log, `%s` stops it\n", p.Command("status"), p.Command("daemon", "logs", "-f"), p.Command("daemon", "stop"))
}

// startDetached re-executes this binary as a foreground daemon in a session
// of its own, and returns once that daemon answers on its control socket — so
// a start that reports success has a daemon that is really up, and one that
// fails says why here rather than only in a log.
func startDetached(ctx context.Context, g global, s startFlags, w io.Writer) error {
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
				printStarted(w, g.paths)
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
//
// A profile that runs as a service is stopped the same way — over the socket,
// with --timeout and --force — and started again by its service manager. A
// clean stop is an exit neither unit restarts (KeepAlive.SuccessfulExit is
// false, Restart=on-failure), and a detached daemon in its place would be one
// the manager no longer watches (DEV-173). kickstart -k or systemctl restart
// would do both halves in one, but with the manager's SIGTERM in place of the
// stop decision 0027 settles, and a wait bounded by the unit, not the owner.
func daemonRestart(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("daemon restart", flag.ContinueOnError)
	var start startFlags
	var stop stopFlags
	start.register(fs)
	stop.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := start.check(fs); err != nil {
		return err
	}
	if stop.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive, got %s — the default is 1m", stop.timeout)
	}
	if err := preflight(g.paths); err != nil {
		return fmt.Errorf("restart refused, and the running daemon left alone: %w", err)
	}
	holder, running, err := control.Holder(g.paths)
	if err != nil {
		return err
	}
	if !running {
		holder = 0
	}
	u, note, err := serviceOf(ctx, g.paths, holder)
	var unknown *unknownServiceError
	if errors.As(err, &unknown) {
		return fmt.Errorf("restart refused, and the running daemon left alone: %w — restart it through the service manager: `%s`, or run this again once `%s` answers", err, shellword.Command(unknown.m.RestartCommand(g.paths.Profile)...), g.paths.Command("service", "status"))
	}
	if err != nil {
		return err
	}
	if u != nil {
		if err := u.refuses(start); err != nil {
			return fmt.Errorf("restart refused, and the running daemon left alone: %w", err)
		}
	}
	if running {
		if err := stopDaemon(ctx, g, stop, w); err != nil {
			return err
		}
	}
	if u != nil {
		return startService(ctx, g, u, start, holder, w)
	}
	if note != "" {
		fmt.Fprintln(w, note)
	}
	return startDetached(ctx, g, start, w)
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
