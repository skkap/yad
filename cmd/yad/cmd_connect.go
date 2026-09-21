package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/skkap/yad/internal/runner"
)

// cmdConnect is `yad connect <url> --token T [--name N]`. The logic is in
// runner.Connect; this parses and prints, and never prints a secret.
func cmdConnect(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	token := fs.String("token", "", "the one-time registration token; - reads it from stdin (see decision 0020)")
	name := fs.String("name", "", "what to call this connection (default: from the hub's host)")
	// The URL comes first in the usage, and the flags are parsed on both
	// sides of it.
	pos, err := positional(fs, args, 1, "usage: yad connect <hub url> --token <token> [--name <name>]")
	if err != nil {
		return err
	}
	url := pos[0]
	tok, err := tokenArg(*token)
	if err != nil {
		return err
	}
	conn, res, notes, err := runner.Connect(ctx, g.paths, url, tok, *name)
	// Notes are printed whether or not the registration succeeded: they say
	// what the document was built without.
	for _, n := range notes {
		fmt.Fprintln(w, "note:", n)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "connected to %s as %q — credential saved, syncing every %s once the runner starts (`yad daemon start`, or `yad service install` to start it at login)\n",
		conn.URL, conn.Name, time.Duration(res.SyncIntervalMS)*time.Millisecond)
	return nil
}
