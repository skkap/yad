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
	// daemon's to let go of, not this process's to write. It is told in two
	// stages, either side of the hub call.
	var closed *control.Disconnected
	noDaemon := false
	ask := func(stage string, into **control.Disconnected) func(context.Context) error {
		return func(ctx context.Context) error {
			res, err := control.Send(ctx, g.paths, control.Request{Op: "disconnect", Connection: name, Stage: stage})
			if errors.Is(err, control.ErrNotRunning) {
				// Nothing is syncing it; the next start reads the config
				// without it. Recorded, because what the daemon would have
				// done is then left undone and the operator is told so.
				noDaemon = true
				return nil
			}
			if err != nil {
				return err
			}
			if into != nil {
				*into = res.Disconnected
			}
			return nil
		}
	}

	out, notes, err := runner.Disconnect(ctx, g.paths, name, *force, runner.Daemon{
		Beginning: ask(control.DisconnectBegin, nil),
		Ended:     ask(control.DisconnectEnd, &closed),
	})
	for _, n := range notes {
		fmt.Fprintln(w, "note:", n)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "disconnected %q from %s — credential removed, connection removed from %s\n", out.Connection.Name, out.Connection.URL, g.paths.ConfigFile())
	if out.Outcome == runner.Retired {
		// What the hub did with the work it was holding for this runner. A
		// session is resumable only where it ran, so it closes there with it.
		fmt.Fprintln(w, "at the hub: the runs it held are lost, runs it had offered are back in its queue, and its sessions on this runner are closed with the runs that were waiting in them cancelled")
	}
	if out.Outcome == runner.LeftBehind {
		fmt.Fprintf(w, "note: the hub was not told (%v) — it still holds a registration for this runner, and the runs and sessions it has for it are untouched; retire it at %s\n", out.Refused, out.Connection.URL)
	}
	switch {
	case noDaemon:
		// Nothing asked the daemon, because none is running: the sessions on
		// this machine are still open and their workdirs still on disk.
		fmt.Fprintln(w, "note: no daemon is running, so that hub's sessions on this machine were not closed — `yad sessions` lists them, and their workdirs stay until a daemon reclaims them under sessions.idle_ttl or the disk floor, which needs a hub still connected")
	case closed == nil:
		// A daemon answered the first stage and not the second, or refused
		// one of them; the notes above say which.
		fmt.Fprintln(w, "note: this hub's sessions on this machine may still be open — `yad sessions` lists them, and `yad sessions close` closes one")
	default:
		if closed.Runs > 0 {
			fmt.Fprintf(w, "note: %d run(s) of that hub are still running here; the hub has marked them lost, so their results have nowhere to go\n", closed.Runs)
		}
		if n := closed.Closed + closed.Closing; n > 0 {
			fmt.Fprintf(w, "%d session(s) of that hub closed here; their workdirs are reclaimed", n)
			if closed.Closing > 0 {
				fmt.Fprintf(w, " (%d once the run held in it ends)", closed.Closing)
			}
			fmt.Fprintln(w)
		}
	}
	if out.Remaining == 0 {
		fmt.Fprintln(w, "no connections left — the runner has nothing to sync; a running daemon stays up idle, as one started with no connection does. `yad connect <url> --token …` adds a hub, `yad daemon stop` ends it")
	}
	return nil
}
