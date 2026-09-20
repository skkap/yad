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
	pos, err := positional(fs, args, 1, "usage: yad disconnect <name> [--force] — `yad status` lists the connections")
	if err != nil {
		return err
	}
	name := pos[0]

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
	if out.Deregistered {
		// What the hub did with the work it was holding for this runner. A
		// session is resumable only where it ran, so it closes there with it.
		fmt.Fprintln(w, "at the hub: the runs it held are lost, runs it had offered are back in its queue, and its sessions on this runner are closed with the runs that were waiting in them cancelled")
	}
	if out.Forced != nil {
		fmt.Fprintf(w, "note: the hub was not told (%v) — it still holds a registration for this runner, and the runs and sessions it has for it are untouched; retire it at %s\n", out.Forced, out.Connection.URL)
	}
	switch {
	case stopped == nil:
		// Nothing asked the daemon, because none is running: the sessions on
		// this machine are still open and their workdirs still on disk.
		fmt.Fprintf(w, "note: no daemon is running, so that hub's sessions on this machine were not closed — `yad sessions` lists them, and their workdirs stay until a daemon reclaims them under sessions.idle_ttl or the disk floor, which needs a hub still connected\n")
	default:
		if stopped.Runs > 0 {
			fmt.Fprintf(w, "note: %d run(s) of that hub are still running here; the hub has marked them lost, so their results have nowhere to go\n", stopped.Runs)
		}
		if n := stopped.Closed + stopped.Closing; n > 0 {
			fmt.Fprintf(w, "%d session(s) of that hub closed here; their workdirs are reclaimed", n)
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
