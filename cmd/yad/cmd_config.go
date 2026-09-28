package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/skkap/yad/internal/config"
)

const configUsage = "usage: yad config apply <file>"

// cmdConfig is `yad config apply`, which brings this profile's config.toml in
// line with another — a work machine's spec (decision 0059) — and says what
// it changed. It is how `yad-machine up` takes a spec's change to a built
// machine, and it goes through the same lock as every other writer, so a hub
// adding an account meanwhile is neither lost nor overwritten.
func cmdConfig(ctx context.Context, g global, args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New(configUsage)
	}
	if args[0] != "apply" {
		return fmt.Errorf("unknown config subcommand %q — %s", args[0], configUsage)
	}
	fs := flag.NewFlagSet("config apply", flag.ContinueOnError)
	pos, err := positional(fs, args[1:], 1, configUsage)
	if err != nil {
		return err
	}
	from := pos[0]
	spec, err := config.ReadFile(from)
	if err != nil {
		return err
	}
	got, err := config.Apply(ctx, g.paths, spec)
	if err != nil {
		return err
	}
	file := g.paths.ConfigFile()
	switch {
	case got.Created:
		fmt.Fprintf(w, "wrote %s from %s\n", file, from)
	case len(got.Changes) > 0:
		fmt.Fprintf(w, "changed %s to match %s:\n", file, from)
		for _, c := range got.Changes {
			fmt.Fprintf(w, "  %s\n", c)
		}
	default:
		fmt.Fprintf(w, "%s already matches %s\n", file, from)
	}
	if n := len(spec.Connections); n > 0 {
		fmt.Fprintf(w, "note: %s lists %d connection(s), which were not taken — a connection needs the credential `%s` makes on this machine\n",
			from, n, g.paths.Command("connect", "<hub url>", "--token", "-"))
	}
	if got.Changed() {
		// The daemon reads config.toml once, at start, but for the account
		// lists (0043).
		fmt.Fprintf(w, "a running runner reads it when it starts — `%s` restarts it\n", g.paths.Command("service", "install"))
	}
	return nil
}
