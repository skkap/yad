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
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

// stopSignals are the signals that stop a command. The runner counts them
// (decision 0029); every other command ends at the first.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

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
	// A foreground runner counts stop signals itself (decision 0029); a
	// background start, which installs no handler, ends at the first as
	// every other command does.
	if !(cmd == "daemon" && len(rest) > 0 && rest[0] == "start") {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, stopSignals...)
		defer stop()
	}
	var cmdErr error
	switch cmd {
	case "version":
		cmdErr = cmdVersion(stdout)
	case "doctor":
		cmdErr = cmdDoctor(ctx, g, rest, stdout)
	case "harnesses":
		cmdErr = cmdHarnesses(ctx, g, rest, stdout, stderr)
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
	case "sessions":
		cmdErr = cmdSessions(ctx, g, rest, stdout)
	case "account":
		cmdErr = cmdAccount(ctx, g, rest, stdout)
	case "upgrade":
		cmdErr = cmdUpgrade(ctx, g, rest, stdout)
	case "conformance":
		cmdErr = cmdConformance(ctx, rest, stdout)
	case "disconnect":
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
                      stop it gracefully (it drains: no new runs, the ones it
                      holds finish), restart it, or say whether it is up.
                      Stop signals drain, then cancel runs, then exit at once
  daemon logs [-f] [-n N]
                      its log: the last lines, then (-f) what follows
  status [--json]     what the runner is doing: connections, capacity, runs,
                      sessions and recent errors
  sessions [--json]   the sessions this runner holds: their workdirs, runs and
                      last use — read from disk, so the daemon may be stopped
  sessions close [--connection c] <session>
                      close a session and reclaim its workdir; its hub hears
                      of it. One with a run held closes when the run ends
  account add <harness> <label>
                      run the harness's own login in a home of its own, with
                      you at the terminal; yad keeps no token of its own
  account list [--json]
                      the accounts this runner has, and the state of each
  account remove <harness> <label> [--yes]
                      delete that account's home and its login; the shared
                      transcripts are kept
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
  hub drain <runner>  the runner takes no new runs, finishes those it holds
                      and exits
  hub close-session <session>
                      the session takes no new run, and its runner deletes
                      its workdir
  service install|uninstall|status [--profile name]
                      run this profile's runner as a launchd agent or a
                      systemd user unit, as you, restarted after a crash
  upgrade [--check] [--force] [--tag v]
                      replace this binary with the newest release, verifying
                      its checksum first. Nothing upgrades on its own, and a
                      runner already running keeps the old binary until it is
                      restarted
  conformance <url> --token T
                      check any hub against v1: every rule it breaks, and
                      where that rule is written
  version             version and build

  disconnect · account use
                      exist, and each says which epic brings it

ARCHITECTURE.md §9 has the build order; the plan is in Zumino, yad/dev.
`)
}
