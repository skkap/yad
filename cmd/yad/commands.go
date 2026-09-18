package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/skkap/yad/internal/agents"
	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/capability"
)

func cmdVersion(argv []string) error {
	fs := flag.NewFlagSet("version", flag.ExitOnError)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if buildinfo.Commit != "" {
		fmt.Printf("yad %s (%s)\n", buildinfo.Version, buildinfo.Commit)
		return nil
	}
	fmt.Printf("yad %s\n", buildinfo.Version)
	return nil
}

// cmdDoctor answers "would this machine be any use as a runner?" — which is the
// first question asked on every new box, and the one a bare `yad daemon start`
// answers far too late.
func cmdDoctor(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "print the raw detection result")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	found := agents.Detect(ctx)
	if *asJSON {
		return writeJSON(found)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tSTATUS\tVERSION\tPATH")
	usable := 0
	for _, d := range found {
		status := "—"
		switch {
		case d.Present && d.Error != "":
			status = "broken"
		case d.Present && d.Kind == agents.FirstClass:
			status = "ready"
			usable++
		case d.Present:
			status = "no adapter"
		}
		detail := d.Version
		if d.Error != "" {
			detail = d.Error
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", d.Label, status, truncate(detail, 40), d.Path)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Println()
	if usable == 0 {
		fmt.Println("No drivable agent found. Install Claude Code or Codex and run this again.")
		return nil
	}
	fmt.Printf("%d agent(s) this runner can be given work for.\n", usable)
	return nil
}

// cmdAgents prints the exact document the runner would register with, so a
// capability mismatch can be diagnosed on the machine instead of from the
// control plane's logs.
func cmdAgents(ctx context.Context, argv []string) error {
	fs := flag.NewFlagSet("agents", flag.ExitOnError)
	name := fs.String("name", "", "runner display name (default: hostname)")
	maxRuns := fs.Int("max-concurrent-runs", 4, "how many runs this machine will accept at once")
	labels := fs.String("labels", "", "comma-separated routing labels")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	rep := capability.Build(ctx, runnerID(), *name, *maxRuns, splitLabels(*labels))
	return writeJSON(rep)
}

// cmdDaemon is the runner loop. Today it proves the parts that exist — identity,
// detection, fingerprinting, a clean shutdown — against no server at all, which
// is deliberately the milestone that comes before a protocol.
func cmdDaemon(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("usage: yad daemon start [--foreground]")
	}
	switch argv[0] {
	case "start":
	case "stop", "status", "logs":
		return fmt.Errorf("`yad daemon %s` needs the control socket, which is milestone M1 — see DESIGN.md", argv[0])
	default:
		return fmt.Errorf("unknown daemon subcommand %q", argv[0])
	}

	fs := flag.NewFlagSet("daemon start", flag.ExitOnError)
	foreground := fs.Bool("foreground", false, "run in this terminal (the only mode that exists yet)")
	heartbeat := fs.Duration("heartbeat-interval", 15*time.Second, "how often to re-probe and report")
	name := fs.String("name", "", "runner display name (default: hostname)")
	if err := fs.Parse(argv[1:]); err != nil {
		return err
	}
	if !*foreground {
		return fmt.Errorf("backgrounding is milestone M1; run with --foreground for now")
	}

	id := runnerID()
	rep := capability.Build(ctx, id, *name, 4, nil)
	fmt.Printf("runner %s (%s) — %s/%s, yad %s\n", rep.Name, id, rep.OS, rep.Arch, rep.YadVersion)
	fmt.Printf("capabilities %s — no control plane configured, nothing will be claimed\n", rep.Fingerprint())

	tick := time.NewTicker(*heartbeat)
	defer tick.Stop()
	last := rep.Fingerprint()
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nshutting down — no runs in flight")
			return nil
		case t := <-tick.C:
			now := capability.Build(ctx, id, *name, 4, nil)
			if fp := now.Fingerprint(); fp != last {
				fmt.Printf("%s capabilities changed %s → %s\n", t.Format(time.TimeOnly), last, fp)
				last = fp
				continue
			}
			fmt.Printf("%s heartbeat %s\n", t.Format(time.TimeOnly), last)
		}
	}
}

func writeJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func splitLabels(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
