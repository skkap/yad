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
	case "connect":
		cmdErr = cmdConnect(ctx, g, rest, stdout)
	case "disconnect", "status", "sessions", "account", "service", "conformance", "upgrade":
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
	if cmdErr != nil {
		fmt.Fprintln(stderr, "yad:", cmdErr)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprint(w, `yad — run coding-agent harnesses on this machine, for any number of hubs

usage: yad [--profile name] <command> [flags]

  doctor              what is installed here, and what YAD can drive
  harnesses [--json]  the capability document, exactly as a hub receives it
  connect <url> --token T [--name n]
                      register this runner with a hub
  daemon start        the runner (--foreground; background arrives in E3)
  hub serve           the standalone hub (headless)
  hub token create    a one-time registration token for yad connect
  hub admin-token create|list|revoke
                      tokens for the hub's service API
  hub submit --harness h --model m <instruction>
                      queue a run on a hub; prints its id
  hub watch <run>     a run's events as they arrive, then its result
  version             version and build

  disconnect · status · sessions · account · service · conformance
                      exist, and each says which epic brings it

ARCHITECTURE.md §9 has the build order; the plan is in Zumino, yad/dev.
`)
}
