package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
)

// cmdDaemon is the runner process. Today it proves what exists — profile,
// identity, config, detection, fingerprinting and a clean shutdown — against no
// hub at all; syncing with a hub is epic E2.
func cmdDaemon(ctx context.Context, g global, args []string, w io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: yad daemon start --foreground")
	}
	switch args[0] {
	case "start":
	case "stop", "status", "logs":
		return fmt.Errorf("`yad daemon %s` needs the control socket, which arrives in epic E3 (Zumino yad/dev) — stop a foreground runner with Ctrl-C", args[0])
	default:
		return fmt.Errorf("unknown daemon subcommand %q — use start", args[0])
	}

	fs := flag.NewFlagSet("daemon start", flag.ContinueOnError)
	foreground := fs.Bool("foreground", false, "run in this terminal (the only mode until epic E3)")
	interval := fs.Duration("interval", 15*time.Second, "how often to re-probe the machine")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if !*foreground {
		return fmt.Errorf("backgrounding arrives in epic E3 (Zumino yad/dev); run with --foreground for now")
	}

	cfg, err := config.Load(g.paths)
	if err != nil {
		return err
	}
	if err := g.paths.Ensure(); err != nil {
		return err
	}
	if *interval <= 0 {
		return fmt.Errorf("--interval must be positive, got %s — the default is 15s", *interval)
	}
	id, err := g.paths.RunnerID()
	if err != nil {
		return err
	}
	doc := capability.Build(ctx, id, cfg)
	last := capability.Fingerprint(doc)
	fmt.Fprintf(w, "runner %s (%s) — profile %s, %s/%s, yad %s, capacity %d\n", doc.Name, id, g.paths.Profile, doc.OS, doc.Arch, doc.YadVersion, cfg.Capacity)
	if len(cfg.Connections) > 0 {
		fmt.Fprintf(w, "%d connection(s) configured; syncing with hubs arrives in epic E2 — nothing will be claimed\n", len(cfg.Connections))
	} else {
		fmt.Fprintln(w, "no hub connected — nothing will be claimed")
	}
	fmt.Fprintf(w, "capabilities %s\n", last)

	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(w, "\nshutting down — no runs in flight")
			return nil
		case t := <-tick.C:
			fp := capability.Fingerprint(capability.Build(ctx, id, cfg))
			if fp != last {
				fmt.Fprintf(w, "%s capabilities changed %s → %s\n", t.Format(time.TimeOnly), last, fp)
				last = fp
			}
		}
	}
}
