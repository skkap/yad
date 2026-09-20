package main

import (
	"context"
	"flag"
	"io"

	"github.com/skkap/yad/internal/conformance"
)

// cmdConformance is `yad conformance <url> --token T`. The checks are in
// internal/conformance, which speaks to the hub over HTTP and knows nothing
// about `yad hub`: this parses, prints, and exits non-zero when a hub broke a
// rule.
func cmdConformance(ctx context.Context, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("conformance", flag.ContinueOnError)
	token := fs.String("token", "", "a registration token the hub has issued for a new runner and nobody has used; one made to re-register an existing runner is refused, and the refusal is the hub's, not a fault of its own; - reads it from stdin (see decision 0020)")
	harness := fs.String("harness", conformance.DefaultHarness, "the harness id to advertise; queue the hub's runs for it to check the rules that need a run")
	wait := fs.Duration("lease-wait", conformance.DefaultLeaseWait, "how long to spend waiting a lease out, which is real time and as long as the hub says; 0 skips that rule")
	pos, err := positional(fs, args, 1, "usage: yad conformance <hub url> --token <token> [--harness id] [--lease-wait d]")
	if err != nil {
		return err
	}
	tok, err := tokenArg(*token)
	if err != nil {
		return err
	}
	rep, err := conformance.Run(ctx, conformance.Options{
		BaseURL: pos[0], Token: tok, Harness: *harness, LeaseWait: *wait,
	})
	if err != nil {
		return err
	}
	rep.Print(w)
	// A run that was stopped part way through exits non-zero too: most of the
	// suite never ran, and a zero from a command that checks things is read as
	// "it checked out".
	if rep.Failed() || rep.Interrupted {
		// The report has already said what broke and where it is written;
		// anything more here would be said twice.
		return exitError{code: 1}
	}
	return nil
}
