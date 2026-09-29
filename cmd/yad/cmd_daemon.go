package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter/claude"
	"github.com/skkap/yad/internal/adapter/codex"
	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/logfile"
	"github.com/skkap/yad/internal/runner"
	"github.com/skkap/yad/internal/supervise"
)

// cmdDaemon is the runner process and its lifecycle: start (in the background,
// or --foreground as service units run it), stop, restart, status and logs.
func cmdDaemon(ctx context.Context, g global, args []string, w io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: yad daemon start|stop|restart|status|logs")
	}
	switch args[0] {
	case "start":
		return daemonStart(ctx, g, args[1:], w)
	case "stop":
		return daemonStop(ctx, g, args[1:], w)
	case "restart":
		return daemonRestart(ctx, g, args[1:], w)
	case "status":
		return daemonStatus(ctx, g, args[1:], w)
	case "logs":
		return daemonLogs(ctx, g, args[1:], w)
	default:
		return fmt.Errorf("unknown daemon subcommand %q — use start, stop, restart, status or logs", args[0])
	}
}

// startFlags are start's, shared with restart, which starts the same way.
type startFlags struct {
	foreground bool
	interval   time.Duration
	wait       time.Duration
}

func (s *startFlags) register(fs *flag.FlagSet) {
	fs.DurationVar(&s.interval, "interval", 15*time.Second, "how often to re-probe the machine")
	fs.DurationVar(&s.wait, "wait", 15*time.Second, "how long a background start waits for the daemon to answer")
}

func (s startFlags) check() error {
	if s.interval <= 0 {
		return fmt.Errorf("--interval must be positive, got %s — the default is 15s", s.interval)
	}
	if s.wait <= 0 {
		return fmt.Errorf("--wait must be positive, got %s — the default is 15s", s.wait)
	}
	return nil
}

func daemonStart(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("daemon start", flag.ContinueOnError)
	var s startFlags
	fs.BoolVar(&s.foreground, "foreground", false, "run in this process, as service units do")
	s.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := s.check(); err != nil {
		return err
	}
	if s.foreground {
		return runForeground(ctx, g, s.interval, w)
	}
	return startBackground(ctx, g, s, w)
}

// runForeground is the runner process: it holds the profile's lock and
// control socket, syncs with every connected hub, runs what it claims, and
// keeps its capability document fresh.
func runForeground(ctx context.Context, g global, interval time.Duration, w io.Writer) error {
	cfg, err := config.Load(g.paths)
	if err != nil {
		return err
	}
	if err := g.paths.Ensure(); err != nil {
		return err
	}
	// The lock and the socket come before anything slow, so a second start
	// is refused at once and a background start sees this one answer soon.
	ctl, err := control.Claim(g.paths)
	if err != nil {
		return err
	}
	defer ctl.Close()

	logf, err := logfile.Open(g.paths.Log(), logfile.DefaultMaxBytes, logfile.DefaultBackups)
	if err != nil {
		return fmt.Errorf("open the daemon log: %w", err)
	}
	defer logf.Close()
	// Under a service manager, or started in the background, stdout is a
	// file nothing rotates (service.log, stderr.log): the rotated log is
	// the record, and stdout keeps only what escapes before it — a
	// failure to start, a panic. A person at a terminal still sees it live.
	w = shownOnlyToAPerson(w)
	recent := logfile.NewRecent(recentErrors)
	log := slog.New(recent.Handler(slog.NewMultiHandler(
		slog.NewJSONHandler(logf, nil),
		slog.NewTextHandler(w, nil),
	)))

	id, err := g.paths.RunnerID()
	if err != nil {
		return err
	}
	started := time.Now()
	// The first stop signal drains, the second cancels the runs held, the
	// third ends runCtx: exit now (decision 0029). The caller's ctx ending
	// is the third step too, and `yad daemon stop` is the first.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	drain := runner.NewDrain()
	sigs := make(chan os.Signal, 3)
	signal.Notify(sigs, stopSignals...)
	defer signal.Stop(sigs)
	go runner.OnSignals(runCtx, sigs, drain, stop, log)

	// The one copy of the owner's account lists every part of the runner
	// reads, and the one `yad account add` and `remove` reload through the
	// control socket (decision 0043). The rest of cfg is fixed until restart.
	lists := runner.NewAccounts(g.paths.Data, account.ListsOf(cfg))
	lists.Log = log
	// Account states come from the state database, read-only and closed
	// again: the runner's own store is opened inside runner.Serve, and a
	// document built before it exists must still name the owner's accounts.
	// A read that fails is not a reason not to start — the labels are still
	// reported, free, which is what a runner with no limits would say.
	noted := modelsNoted{}
	build := func() v1.Capabilities {
		now := lists.Lists()
		accounts, err := account.Read(runCtx, g.paths, now, time.Now())
		if err != nil {
			log.Warn("could not read account states; reporting the owner's accounts as free", "err", err)
		}
		doc := capability.Build(runCtx, id, now.Apply(cfg), accounts)
		noted.note(log, capability.ModelsFailures())
		return doc
	}
	doc := build()
	last := capability.Fingerprint(doc)
	fmt.Fprintf(w, "runner %s (%s) — profile %s, %s/%s, yad %s, capacity %d\n", doc.Name, id, g.paths.Profile, doc.OS, doc.Arch, doc.YadVersion, cfg.Capacity)
	if len(cfg.Connections) > 0 {
		fmt.Fprintf(w, "syncing with %d hub(s); runs are claimed for the harnesses `%s` shows as first-class\n", len(cfg.Connections), g.paths.Command("doctor"))
	} else {
		fmt.Fprintf(w, "no hub connected — `%s` to add one; nothing will be claimed\n", g.paths.Command("connect", "<hub url>", "--token", "<token>"))
	}
	fmt.Fprintf(w, "capabilities %s\n", last)
	log.Info("daemon started", "profile", g.paths.Profile, "runner_id", id, "version", buildinfo.Version, "connections", len(cfg.Connections), "capabilities", last)
	// `yad doctor` warns about these from the shell it runs in, which is not
	// always this environment — a service manager starts the daemon with its
	// own. Names only: the values are credentials.
	if names := supervise.AccountVariables(os.Environ()); len(names) > 0 {
		log.Warn("the daemon's environment sets variables that choose a harness's credential; each is removed from every child the runner starts, so runs use their account's login instead (decision 0060)", "variables", names)
	}

	// Syncs read the document every interval; probing harnesses that often
	// would spawn every CLI's --version four times a minute, so they read
	// this copy and the probe keeps its own pace.
	var mu sync.Mutex
	current := func() v1.Capabilities {
		mu.Lock()
		defer mu.Unlock()
		return doc
	}
	monitor := runner.NewMonitor()
	// An account change rebuilds the document now rather than at the next
	// tick, so a hub hears of the new account within one sync.
	rebuild := make(chan struct{}, 1)

	// The socket outlives the runner's context: while a stop is under way,
	// `yad status` is how the owner sees what it is waiting on.
	ctlCtx, ctlStop := context.WithCancel(context.Background())
	ctlDone := make(chan struct{})
	go func() {
		defer close(ctlDone)
		ctl.Serve(ctlCtx, control.Handler{
			Status: func(ctx context.Context) control.Status {
				return statusOf(ctx, g.paths, cfg, current(), started, monitor, recent)
			},
			Stop: gracefulStop(log, drain),
			CloseSession: func(ctx context.Context, conn, id string) (control.SessionClose, error) {
				res, err := monitor.CloseSession(ctx, conn, id)
				return control.SessionClose{Outcome: res.Outcome, Reason: string(res.Reason), LiveRun: res.LiveRun}, err
			},
			AccountsChanged: func(ctx context.Context, ch control.AccountChange) (control.AccountResult, error) {
				if ch.Keep {
					return control.AccountResult{}, lists.Keep(account.Ref{Harness: ch.Harness, Label: ch.Label})
				}
				// config.toml as it reads now, not as it read at start: the
				// CLI wrote the change there before it asked. In turn with a
				// hub's adds and removals, which write it too (decision 0057).
				res, err := lists.Reread(ctx, g.paths, account.Ref{Harness: ch.Harness, Label: ch.Label}, ch.Removed)
				if err != nil {
					return control.AccountResult{}, err
				}
				// An account logged in again at the machine may be on another
				// plan, and the document keeps a login's models for an hour.
				capability.ForgetModels(ch.Harness)
				select {
				case rebuild <- struct{}{}:
				default:
				}
				return control.AccountResult{State: string(res.State), Runs: res.Runs}, nil
			},
		})
	}()
	defer func() {
		ctlStop()
		<-ctlDone
	}()

	served := make(chan error, 1)
	go func() {
		served <- runner.Serve(runCtx, runner.Options{
			Paths: g.paths, Config: cfg, Accounts: lists, RunnerID: id, Capabilities: current,
			// The catalog decides what is advertised; an adapter here with a
			// harness still recognised there is never offered a run.
			Adapters: runner.NewRegistry(claude.Adapter{}, codex.Adapter{}),
			Drain:    drain,
			Log:      log,
			ScrubLog: logf.Scrub,
			Monitor:  monitor,
			// The same ring `yad status` shows. Health reports the messages
			// alone, never the attrs — see runner.healthErrors.
			RecentErrors: recent.Records,
			// A hub login that took, or an account a hub added or removed,
			// is news for the document now, as an account the owner added is.
			AccountsChanged: func() {
				select {
				case rebuild <- struct{}{}:
				default:
				}
			},
		})
	}()

	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		var t time.Time
		select {
		case err := <-served:
			switch {
			case runCtx.Err() != nil:
				fmt.Fprintln(w, "\nshut down — runs cut short are reported lost at the next start")
				log.Info("daemon stopped", "drained", false)
			case drain.IsDraining():
				fmt.Fprintln(w, "\ndrained — every run held has ended")
				log.Info("daemon stopped", "drained", true, "reason", drain.Reason())
			default:
				// Every connection stopped on its own, or the runner could
				// not set up at all — its store would not open, say, which a
				// runner with no hub now meets too. The error says which.
				log.Error("daemon exiting: the runner stopped on its own", "err", err)
			}
			return err
		case t = <-tick.C:
		case <-rebuild:
			t = time.Now()
		}
		next := build()
		if fp := capability.Fingerprint(next); fp != last {
			log.Info("capabilities changed", "from", last, "to", fp)
			fmt.Fprintf(w, "%s capabilities changed %s → %s\n", t.Format(time.TimeOnly), last, fp)
			last = fp
		}
		mu.Lock()
		doc = next
		mu.Unlock()
	}
}

// modelsNoted is the reason the daemon last logged for each login, by harness
// and account label, whose harness would not say which models it offers. The
// document is rebuilt every interval and a failed ask is retried every few
// minutes, so a harness that stays too old would otherwise say so all day:
// a reason is logged when it appears or changes, and its clearing once.
type modelsNoted map[[2]string]string

func (n modelsNoted) note(log *slog.Logger, failing []capability.ModelsFailure) {
	now := map[[2]string]bool{}
	for _, f := range failing {
		k := [2]string{f.Harness, f.Account}
		now[k] = true
		if n[k] == f.Reason {
			continue
		}
		n[k] = f.Reason
		// The reason is an attribute, so it stays on the machine: a hub's
		// health hears this message and nothing else (healthErrors).
		log.Warn("a harness did not say which models it offers, so hubs are sent the last list it gave, the catalog's, or none", "harness", f.Harness, "account", f.Account, "reason", f.Reason)
	}
	for k := range n {
		if !now[k] {
			delete(n, k)
			log.Info("a harness that did not say which models it offers has answered, or is no longer asked", "harness", k[0], "account", k[1])
		}
	}
}

// shownOnlyToAPerson is w when a person can be reading it, and io.Discard
// when it is a file or pipe a supervisor captures. A writer that is not an
// *os.File is the caller's own, kept as it is.
func shownOnlyToAPerson(w io.Writer) io.Writer {
	f, ok := w.(*os.File)
	if !ok {
		return w
	}
	if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return w
	}
	return io.Discard
}

// recentErrors is how many warnings and errors `yad status` shows: enough to
// see a pattern, few enough to read.
const recentErrors = 20

// gracefulStop is what `yad daemon stop` asks for through the control socket:
// the drain a first stop signal starts (decisions 0027, 0029) — no new runs,
// the ones held finish for up to the drain wait and are then cancelled, and
// the process exits. It is the owner's first stop request, so a SIGTERM after
// it — `yad daemon stop --force` sends one — cancels the runs held.
// It returns at once; the socket answers `yad status` until the process exits.
func gracefulStop(log *slog.Logger, drain *runner.Drain) func() {
	return func() {
		if drain.Stop("`yad daemon stop` asked through the control socket") {
			log.Warn("draining: stop requested through the control socket — no new runs; exiting once the runs held have ended")
		}
	}
}

// statusOf is `yad status`'s answer, read from the monitor, the store and
// the recent log records.
func statusOf(ctx context.Context, p config.Paths, cfg config.Config, doc v1.Capabilities, started time.Time, m *runner.Monitor, recent *logfile.Recent) control.Status {
	st := control.Status{
		Ready:   m.Ready(),
		Profile: p.Profile, RunnerID: doc.RunnerID, Name: doc.Name, Version: buildinfo.Version, Started: started,
		// No pool exists until the runner has set up, and nothing is
		// claimed before it: all of it is free.
		Capacity:    control.Capacity{Total: doc.Capacity.Total, Free: doc.Capacity.Total},
		PathSources: new(cfg.Workdirs.AllowsPathSources()),
	}
	snap, err := m.Snapshot(ctx)
	if snap.Capacity != nil {
		st.Capacity = control.Capacity{Total: snap.Capacity.Total, Free: snap.Capacity.Free}
	}
	for _, c := range cfg.Connections {
		cs, ok := snap.Connections[c.Name]
		if !ok {
			cs.State = runner.ConnStarting
		}
		// Redacted here rather than where status prints it, so nothing that
		// reads the control socket is handed a credential from the URL.
		conn := control.Connection{Name: c.Name, URL: config.RedactURL(c.URL), State: cs.State, LastError: cs.LastError, Held: cs.Held, Cap: cs.Cap,
			ManageAccounts: new(c.MayManageAccounts())}
		if !cs.LastSync.IsZero() {
			conn.LastSync = &cs.LastSync
		}
		if !cs.LastErrorAt.IsZero() {
			conn.LastErrorAt = &cs.LastErrorAt
		}
		st.Connections = append(st.Connections, conn)
	}
	for _, r := range snap.Held {
		run := control.Run{Connection: r.Connection, ID: r.ID, Session: r.SessionID, Harness: r.Harness, Model: r.Model,
			State: r.State, Reason: r.Reason.String, Since: time.UnixMilli(r.UpdatedAt)}
		if r.ResumesAt.Valid {
			t := time.UnixMilli(r.ResumesAt.Int64)
			run.ResumesAt = &t
		}
		st.Runs = append(st.Runs, run)
	}
	st.Sessions, st.SpoolDepth, st.OutboxDepth = snap.Sessions, snap.Spool, snap.Outbox
	for _, r := range recent.Records() {
		st.Errors = append(st.Errors, control.LogRecord{Time: r.Time, Level: r.Level.String(), Message: r.Message, Attrs: r.Attrs})
	}
	for _, f := range capability.ModelsFailures() {
		st.ModelsFailures = append(st.ModelsFailures, control.ModelsFailure{Harness: f.Harness, Account: f.Account, Reason: f.Reason, Since: f.Since})
	}
	if err != nil {
		st.Errors = append(st.Errors, control.LogRecord{Time: time.Now(), Level: slog.LevelError.String(),
			Message: "status could not read the runner's store", Attrs: err.Error()})
	}
	return st
}

// errNotRunning is `yad daemon status` finding nothing: an answer, not a
// failure, and exit 3 as init scripts have always said it.
var errNotRunning = exitError{code: 3}

// daemonStatus is the short answer: is it up, and where.
func daemonStatus(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("daemon status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	res, err := control.Ask(ctx, g.paths, "status")
	switch {
	case errors.Is(err, control.ErrNotRunning):
		fmt.Fprintf(w, "not running — profile %s; `%s` starts it\n", g.paths.Profile, g.paths.Command("daemon", "start"))
		return errNotRunning
	case err != nil:
		return err
	}
	s := res.Status
	state := "running"
	if s.Stopping {
		state = "stopping"
	}
	fmt.Fprintf(w, "%s — pid %d, up %s, profile %s\n", state, s.PID, time.Since(s.Started).Round(time.Second), s.Profile)
	fmt.Fprintf(w, "runner   %s (%s), yad %s\n", s.Name, s.RunnerID, s.Version)
	fmt.Fprintf(w, "socket   %s\n", g.paths.Socket())
	fmt.Fprintf(w, "log      %s\n", g.paths.Log())
	return nil
}
