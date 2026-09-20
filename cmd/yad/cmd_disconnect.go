package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/runner"
)

// cmdDisconnect is `yad disconnect <name> [--force]`. The logic is in
// runner.Disconnect; this parses, asks the running daemon to let the
// connection go, and prints what happened.
func cmdDisconnect(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("disconnect", flag.ContinueOnError)
	force := fs.Bool("force", false, "remove the credential and the connection even if the hub will not answer; retire this runner at the hub by hand afterwards")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: yad disconnect <name> [--force] — `yad status` lists the connections")
	}
	name := fs.Arg(0)

	// The daemon owns the state database (decision 0035), so what this leaves
	// behind on the runner's side — a syncing loop, open sessions — is the
	// daemon's to let go of, not this process's to write.
	var stopped *control.Disconnected
	stop := func(ctx context.Context) error {
		res, err := control.Send(ctx, g.paths, control.Request{Op: "disconnect", Connection: name})
		if errors.Is(err, control.ErrNotRunning) {
			return nil // nothing is syncing it; the next start reads the config without it
		}
		if err != nil {
			return err
		}
		stopped = res.Disconnected
		return nil
	}

	out, notes, err := runner.Disconnect(ctx, g.paths, name, *force, stop)
	for _, n := range notes {
		fmt.Fprintln(w, "note:", n)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "disconnected %q from %s — credential removed, connection removed from %s\n", out.Connection.Name, out.Connection.URL, g.paths.ConfigFile())
	if out.Forced != nil {
		fmt.Fprintf(w, "note: the hub was not told (%v) — it still holds a registration for this runner; retire it at %s\n", out.Forced, out.Connection.URL)
	}
	if stopped != nil {
		if stopped.Runs > 0 {
			fmt.Fprintf(w, "note: %d run(s) of that hub are still running here; the hub has marked them lost, so their results have nowhere to go\n", stopped.Runs)
		}
		if n := stopped.Closed + stopped.Closing; n > 0 {
			fmt.Fprintf(w, "%d session(s) of that hub closed; their workdirs are reclaimed", n)
			if stopped.Closing > 0 {
				fmt.Fprintf(w, " (%d once the run held in it ends)", stopped.Closing)
			}
			fmt.Fprintln(w)
		}
	}
	if out.Remaining == 0 {
		fmt.Fprintln(w, "no connections left — the runner has nothing to sync, and a running daemon exits once the runs it holds have ended; `yad connect <url> --token …` adds a hub")
	}
	return nil
}
