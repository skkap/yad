package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/runner"
	"github.com/skkap/yad/internal/service"
)

// serviceManager is the machine's service manager; a variable so tests drive
// the command against a fake that never reaches launchctl or systemctl.
var serviceManager = func() (service.Manager, service.Host, error) {
	h, err := service.LocalHost()
	if err != nil {
		return nil, h, err
	}
	m, err := service.ForOS(runtime.GOOS, h)
	return m, h, err
}

// executable is yad's own path, as the unit will run it; a variable for tests.
var executable = os.Executable

// cmdService installs, removes and reports the per-user service that runs a
// profile's runner — `yad daemon start --foreground` under launchd or systemd.
func cmdService(ctx context.Context, g global, args []string, w io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: yad service install|uninstall|status [--profile name]")
	}
	sub := args[0]
	fs := flag.NewFlagSet("service "+sub, flag.ContinueOnError)
	profile := fs.String("profile", "", "which runner (default: the profile yad was started with)")
	if _, err := positional(fs, args[1:], 0, fmt.Sprintf("usage: yad service %s [--profile name]", sub)); err != nil {
		return err
	}
	paths := g.paths
	if *profile != "" {
		p, err := config.Resolve(*profile)
		if err != nil {
			return err
		}
		paths = p
	}

	m, h, err := serviceManager()
	if err != nil {
		return err
	}
	if err := service.RefuseRoot(h); err != nil {
		return err
	}
	switch sub {
	case "install":
		return serviceInstall(ctx, m, h, paths, w)
	case "uninstall":
		if err := m.Uninstall(ctx, paths.Profile); err != nil {
			return err
		}
		fmt.Fprintf(w, "removed %s — profile %s has no service now; its config and state are untouched\n", m.Name(paths.Profile), paths.Profile)
		return nil
	case "status":
		st, err := m.Status(ctx, paths.Profile)
		if err != nil {
			return err
		}
		printServiceStatus(w, paths, st)
		return nil
	default:
		return fmt.Errorf("unknown service subcommand %q — use install, uninstall or status", sub)
	}
}

func serviceInstall(ctx context.Context, m service.Manager, h service.Host, paths config.Paths, w io.Writer) error {
	// A config.toml the runner cannot read would only show as a service
	// restarting every few seconds; say so here, where the owner is looking.
	cfg, err := config.Load(paths)
	if err != nil {
		return err
	}
	exe, err := executable()
	if err != nil {
		return fmt.Errorf("cannot find yad's own path to put in the service: %w", err)
	}
	path, note := service.LoginPATH(ctx, h.Run, h.Getenv("SHELL"), h.Getenv("PATH"))
	if path == "" {
		return fmt.Errorf("found no usable PATH for the service — check that your login shell sets PATH, then run this again")
	}
	spec, err := service.NewSpec(paths, exe, path, h)
	if err != nil {
		return err
	}
	// A stop signal drains the runner (decision 0029); the manager must wait
	// out the drain wait and the cancel ladder before it kills anything.
	spec.StopTimeout = runner.StopBudget(cfg.Drain.Wait.Duration)
	if err := paths.Ensure(); err != nil {
		return err
	}
	notes, err := m.Install(ctx, spec)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "installed %s — profile %s runs as %s and restarts after a crash\n", m.Name(paths.Profile), paths.Profile, userName(h))
	fmt.Fprintf(w, "  unit     %s\n", m.File(paths.Profile))
	fmt.Fprintf(w, "  runs     %s\n", strings.Join(spec.Args(), " "))
	fmt.Fprintf(w, "  log      %s\n", spec.LogFile)
	fmt.Fprintf(w, "  stop     drains for up to %s, then cancels its runs; the service manager waits %s — run install again after changing [drain] wait; a reinstall drains the running runner first\n", cfg.Drain.Wait.Duration, spec.StopTimeout)
	if note != "" {
		fmt.Fprintf(w, "  PATH     %s\n", note)
	} else {
		fmt.Fprintln(w, "  PATH     read from your login shell; run install again after changing it")
	}
	for _, n := range notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
	if len(cfg.Connections) == 0 {
		fmt.Fprintf(w, "note: no hub is connected, so the runner claims nothing — after `%s`, run `%s` again to restart it\n", paths.Command("connect", "<hub url>", "--token", "<token>"), paths.Command("service", "install"))
	}
	return nil
}

func printServiceStatus(w io.Writer, paths config.Paths, st service.Status) {
	profile := paths.Profile
	switch {
	case !st.Installed && !st.Loaded:
		fmt.Fprintf(w, "profile %s: no service — `%s` sets one up\n", profile, paths.Command("service", "install"))
		return
	case st.Running:
		fmt.Fprintf(w, "profile %s: %s running, pid %d\n", profile, st.Name, st.PID)
	case st.Loaded:
		fmt.Fprintf(w, "profile %s: %s loaded, not running — %s\n", profile, st.Name, st.Detail)
	default:
		fmt.Fprintf(w, "profile %s: %s installed but not loaded — `%s` loads it again\n", profile, st.Name, paths.Command("service", "install"))
	}
	if st.Installed {
		fmt.Fprintf(w, "  unit     %s\n", st.File)
	} else {
		fmt.Fprintf(w, "  unit     missing (%s) — the service manager still holds the job; `%s` clears it\n", st.File, paths.Command("service", "uninstall"))
	}
}

func userName(h service.Host) string {
	if h.User != "" {
		return h.User
	}
	return fmt.Sprintf("uid %d", h.UID)
}
