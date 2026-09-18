package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
)

func cmdVersion(w io.Writer) error {
	if buildinfo.Commit != "" {
		_, err := fmt.Fprintf(w, "yad %s (%s)\n", buildinfo.Version, buildinfo.Commit)
		return err
	}
	_, err := fmt.Fprintf(w, "yad %s\n", buildinfo.Version)
	return err
}

// cmdDoctor answers "would this machine be any use as a runner?" — the first
// question on every new box, which a bare `yad daemon start` answers far too late.
func cmdDoctor(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the raw detection result")
	if err := fs.Parse(args); err != nil {
		return err
	}
	found := harness.Detect(ctx)
	if *asJSON {
		return writeJSON(w, found)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HARNESS\tSTATUS\tVERSION\tPATH")
	ready := 0
	for _, d := range found {
		status := "—"
		switch {
		case d.Present && d.Error != "":
			status = "broken"
		case d.Ready():
			status = "ready"
			ready++
		case d.Present:
			status = "no adapter"
		}
		detail := d.Version
		if d.Error != "" {
			detail = d.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Label, status, truncate(detail, 40), d.Path)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(w, "\nprofile %s — config %s\n", g.paths.Profile, g.paths.Config)
	if ready == 0 {
		fmt.Fprintln(w, "No drivable harness found. Install Claude Code or Codex and run this again.")
		return nil
	}
	fmt.Fprintf(w, "%d harness(es) this runner can be given work for.\n", ready)
	return nil
}

// cmdHarnesses prints the exact capability document this runner would register
// with, so a mismatch can be diagnosed on the machine instead of from a hub's logs.
func cmdHarnesses(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("harnesses", flag.ContinueOnError)
	// --json is accepted for symmetry with doctor; the document is always JSON.
	fs.Bool("json", true, "print the capability document as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(g.paths)
	if err != nil {
		return err
	}
	id, err := g.paths.RunnerID()
	if err != nil {
		return err
	}
	return writeJSON(w, capability.Build(ctx, id, cfg))
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
