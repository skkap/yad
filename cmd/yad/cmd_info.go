package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
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
	found := capability.Detect(ctx)
	if *asJSON {
		return writeJSON(w, found)
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HARNESS\tSTATUS\tVERSION\tPATH")
	ready, broken, noAdapter := 0, 0, 0
	for _, d := range found {
		status := "—"
		switch {
		case d.Present && d.Error != "":
			status = "broken"
			broken++
		case d.Ready():
			status = "ready"
			ready++
		case d.Present:
			status = "no adapter"
			noAdapter++
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
	for _, d := range found {
		// In full, because the column above is truncated and what a harness
		// error carries now is the next action (DEV-60) — the footer below
		// tells the owner to fix the errors above, so they have to be readable.
		if d.Error != "" {
			fmt.Fprintf(w, "\nerror: %s — %s\n", d.Label, d.Error)
		}
		for _, warn := range d.Warnings {
			fmt.Fprintf(w, "\nwarning: %s — %s\n", d.Label, warn)
		}
	}
	// How the machine is set up, not what is installed on it, so these carry no
	// harness label — and they are printed here rather than folded into the
	// --json capability document, which is what a hub receives and has no
	// business learning a directory's mode. They never change the exit code:
	// the guide these back (docs/run-it-safely.md) tells an owner to run
	// `yad doctor`, and one that failed on a group-readable directory would
	// stop reporting the harnesses they ran it for.
	for _, warn := range config.Exposures(g.paths) {
		fmt.Fprintf(w, "\nwarning: %s\n", warn)
	}
	fmt.Fprintf(w, "\nprofile %s — config %s\n", g.paths.Profile, g.paths.Config)
	// Each way to reach zero has a different next action, and telling someone
	// with both CLIs installed to install them is worse than saying nothing.
	switch {
	case ready > 0:
	case broken > 0:
		// Checked before noAdapter: a broken Claude beside a working Codex needs
		// fixing, not installing.
		fmt.Fprintln(w, "No drivable harness: an installed one failed its version probe — fix the errors above and run this again.")
		return nil
	case noAdapter > 0:
		fmt.Fprintln(w, "No drivable harness: what is installed has no adapter in this yad yet — install Claude Code or Codex, which have one.")
		return nil
	default:
		fmt.Fprintln(w, "No drivable harness found. Install Claude Code or Codex and run this again.")
		return nil
	}
	fmt.Fprintf(w, "%d harness(es) this runner can be given work for.\n", ready)
	return nil
}

// cmdHarnesses prints the exact capability document this runner would register
// with, so a mismatch can be diagnosed on the machine instead of from a hub's logs.
func cmdHarnesses(ctx context.Context, g global, args []string, w, errw io.Writer) error {
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
	// The document is what a hub receives, and a state database this binary
	// cannot read — one migration behind, because the daemon has not restarted
	// — is no reason to refuse to print it. capability.Build reports the
	// owner's configured labels as free when it is given none.
	//
	// The note goes to stderr: the document goes to w and has to stay
	// parseable, and an owner reading "free" for an account that may need
	// login should be told the states were never read.
	accounts, err := account.Read(ctx, g.paths, cfg)
	if err != nil {
		fmt.Fprintln(errw, "note: account states could not be read, so every configured account is shown free:", err)
		accounts = nil
	}
	return writeJSON(w, capability.Build(ctx, id, cfg, accounts))
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// truncate shortens s to at most n bytes plus the ellipsis, cutting on a rune
// boundary. The boundary is the point: these strings carry em dashes, and a
// cut mid-rune prints a replacement character in the first diagnostic anyone
// runs on a new machine. The cell it fills has no width of its own — tabwriter
// sizes columns in runes from what they hold — so n is only this table's own
// bound on one line, and the whole text is printed below the table anyway.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - 1
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
