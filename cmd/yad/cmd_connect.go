package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/skkap/yad/internal/runner"
)

// cmdConnect is `yad connect <url> --token T [--name N]`. The logic is in
// runner.Connect; this parses and prints, and never prints a secret.
func cmdConnect(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("connect", flag.ContinueOnError)
	token := fs.String("token", "", "the one-time registration token; - reads it from stdin (see decision 0020)")
	name := fs.String("name", "", "what to call this connection (default: from the hub's host)")
	// The URL comes first in the usage, and flag stops at the first
	// positional argument, so flags are parsed on both sides of it.
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New("usage: yad connect <hub url> --token <token> [--name <name>]")
	}
	url := rest[0]
	if err := fs.Parse(rest[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q — usage: yad connect <hub url> --token <token> [--name <name>]", fs.Arg(0))
	}
	tok := *token
	if tok == "-" {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read the registration token from stdin: %w", err)
		}
		tok = strings.TrimSpace(line)
	}
	conn, res, err := runner.Connect(ctx, g.paths, url, tok, *name)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "connected to %s as %q — credential saved, syncing every %s once the runner starts (`yad daemon start --foreground`)\n",
		conn.URL, conn.Name, time.Duration(res.SyncIntervalMS)*time.Millisecond)
	return nil
}
