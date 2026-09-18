// Command yad is the runner: the process that sits on a machine, says what
// coding agents it has, and runs them when a control plane asks it to.
//
// What exists today is the half that needs no server: detection, the capability
// document, and a heartbeat loop you can watch. See DESIGN.md for the rest.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(2)
	}

	var err error
	switch cmd := os.Args[1]; cmd {
	case "version":
		err = cmdVersion(os.Args[2:])
	case "doctor":
		err = cmdDoctor(ctx, os.Args[2:])
	case "agents":
		err = cmdAgents(ctx, os.Args[2:])
	case "daemon":
		err = cmdDaemon(ctx, os.Args[2:])
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "yad: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "yad:", err)
		os.Exit(1)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `yad — run coding agents on this machine, on someone else's say-so

usage: yad <command> [flags]

  doctor          what is installed here, and what YAD can drive
  agents          the capability document, as a control plane sees it
  daemon start    the runner loop (currently --foreground only)
  version         version and build

Not built yet: run, sessions, config, service, setup. DESIGN.md says in what order.
`)
}
