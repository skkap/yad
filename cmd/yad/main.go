// Command yad runs coding-agent harnesses on this machine for any number of
// hubs, and — as `yad hub` — is a hub itself.
//
// This file only dispatches. Each command group lives in its own file, and
// holds no logic beyond parsing flags and printing: the behaviour is in
// internal/, where it is tested.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/skkap/yad/internal/config"
)

// stdin is where `--token -` reads from; a variable so tests can supply one.
var stdin io.Reader = os.Stdin

// global is what every command can see.
type global struct {
	paths config.Paths
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("yad", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", os.Getenv("YAD_PROFILE"), "which runner on this machine (default: the default profile)")
	fs.Usage = func() { usage(stderr) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		usage(stderr)
		return 2
	}
	paths, err := config.Resolve(*profile)
	if err != nil {
		fmt.Fprintln(stderr, "yad:", err)
		return 2
	}
	g := global{paths: paths}

	cmd, rest := fs.Arg(0), fs.Args()[1:]
	var cmdErr error
	switch cmd {
	case "version":
		cmdErr = cmdVersion(stdout)
	case "doctor":
		cmdErr = cmdDoctor(ctx, g, rest, stdout)
	case "harnesses":
		cmdErr = cmdHarnesses(ctx, g, rest, stdout)
	case "daemon":
		cmdErr = cmdDaemon(ctx, g, rest, stdout)
	case "hub":
		cmdErr = cmdHub(ctx, g, rest, stdout, stderr)
	case "service":
		cmdErr = cmdService(ctx, g, rest, stdout)
	case "connect":
		cmdErr = cmdConnect(ctx, g, rest, stdout)
	case "status":
		cmdErr = cmdStatus(ctx, g, rest, stdout)
	case "disconnect", "sessions", "account", "conformance", "upgrade":
		cmdErr = notYet(cmd, rest)
	case "agents":
		cmdErr = errors.New("`yad agents` is now `yad harnesses` — Claude Code and Codex are harnesses here (DOMAIN.md)")
	case "help", "-h", "--help":
		usage(stdout)
	default:
		fmt.Fprintf(stderr, "yad: unknown command %q\n\n", cmd)
		usage(stderr)
		return 2
	}
	var exit exitError
	if errors.As(cmdErr, &exit) {
		return exit.code
	}
	if cmdErr != nil {
		fmt.Fprintln(stderr, "yad:", cmdErr)
		return 1
	}
	return 0
}

// exitError is a command's answer that is an exit code rather than a failure:
// `yad daemon status` finding no daemon exits 3 having already said so.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit %d", e.code) }

func usage(w io.Writer) {
	fmt.Fprint(w, `yad — run coding-agent harnesses on this machine, for any number of hubs

usage: yad [--profile name] <command> [flags]

  doctor              what is installed here, and what YAD can drive
  harnesses [--json]  the capability document, exactly as a hub receives it
  connect <url> --token T [--name n]
                      register this runner with a hub
  daemon start        the runner, in the background (--foreground in this terminal)
  daemon stop|restart|status
                      stop it gracefully, restart it, or say whether it is up
  daemon logs [-f] [-n N]
                      its log: the last lines, then (-f) what follows
  status [--json]     what the runner is doing: connections, capacity, runs,
                      sessions and recent errors
  hub serve           the standalone hub (headless)
  hub token create    a one-time registration token for yad connect
  hub admin-token create|list|revoke
                      tokens for the hub's service API
  hub submit --harness h --model m <instruction>
                      queue a run on a hub; prints its id
  hub watch <run>     a run's events as they arrive, then its result
  hub cancel <run>    stop a run: at once if unstarted, else down the cancel ladder
  hub interrupt <run> end a run's turn and keep its session
  hub steer <run> <text | ->
                      add input to a running turn
  service install|uninstall|status [--profile name]
                      run this profile's runner as a launchd agent or a
                      systemd user unit, as you, restarted after a crash
  version             version and build

  disconnect · sessions · account · conformance
                      exist, and each says which epic brings it

ARCHITECTURE.md §9 has the build order; the plan is in Zumino, yad/dev.
`)
}
