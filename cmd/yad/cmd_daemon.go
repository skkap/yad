package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter/claude"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/runner"
)

// cmdDaemon is the runner process: it syncs with every connected hub, runs
// what it claims, and keeps its capability document fresh.
func cmdDaemon(ctx context.Context, g global, args []string, w io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: yad daemon start --foreground")
	}
	switch args[0] {
	case "start":
	case "stop", "status", "logs":
		return fmt.Errorf("`yad daemon %s` needs the control socket, which arrives in epic E3 (Zumino yad/dev) — stop a foreground runner with Ctrl-C or SIGTERM: once drains, twice cancels its runs, three times exits now", args[0])
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
		fmt.Fprintf(w, "syncing with %d hub(s); runs are claimed for the harnesses `yad doctor` shows as first-class\n", len(cfg.Connections))
	} else {
		fmt.Fprintln(w, "no hub connected — `yad connect <url> --token …` to add one; nothing will be claimed")
	}
	fmt.Fprintf(w, "capabilities %s\n", last)

	// Syncs read the document every interval; probing harnesses that often
	// would spawn every CLI's --version four times a minute, so they read
	// this copy and the probe keeps its own pace.
	var mu sync.Mutex
	current := func() v1.Capabilities {
		mu.Lock()
		defer mu.Unlock()
		return doc
	}
	// The first stop signal drains, the second cancels the runs held, the
	// third ends this context: exit now (decision 0029). The caller's ctx
	// ending is the third step too.
	log := slog.New(slog.NewTextHandler(w, nil))
	ctx, exitNow := context.WithCancel(ctx)
	defer exitNow()
	drain := runner.NewDrain()
	sigs := make(chan os.Signal, 3)
	signal.Notify(sigs, stopSignals...)
	defer signal.Stop(sigs)
	go runner.OnSignals(ctx, sigs, drain, exitNow, log)

	served := make(chan error, 1)
	go func() {
		served <- runner.Serve(ctx, runner.Options{
			Paths: g.paths, Config: cfg, RunnerID: id, Capabilities: current,
			// The catalog decides what is advertised; an adapter here with a
			// harness still recognised there is never offered a run.
			Adapters: runner.NewRegistry(claude.Adapter{}),
			Drain:    drain, Log: log,
		})
	}()

	tick := time.NewTicker(*interval)
	defer tick.Stop()
	for {
		select {
		case err := <-served:
			switch {
			case ctx.Err() != nil:
				fmt.Fprintln(w, "\nshut down — runs cut short are reported lost at the next start")
			case drain.IsDraining():
				fmt.Fprintln(w, "\ndrained — every run held has ended")
			}
			// Otherwise every connection stopped on its own: nothing left
			// to do.
			return err
		case t := <-tick.C:
			next := capability.Build(ctx, id, cfg)
			if fp := capability.Fingerprint(next); fp != last {
				fmt.Fprintf(w, "%s capabilities changed %s → %s\n", t.Format(time.TimeOnly), last, fp)
				last = fp
			}
			mu.Lock()
			doc = next
			mu.Unlock()
		}
	}
}
