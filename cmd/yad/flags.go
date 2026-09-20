package main

import (
	"errors"
	"flag"
	"fmt"
)

// positional parses a command's flags on both sides of its positional
// arguments and returns them.
//
// Go's flag package stops at the first non-flag argument, so a command that
// parses once and then reads fs.Arg refuses its own documented form the moment
// the operator writes the flag last: `yad disconnect home --force` left both
// as positionals and failed the argument count, with `--force` named in the
// usage line, in ARCHITECTURE.md and inside the very error that tells the
// operator to pass it. `yad connect` and `yad account remove` had each solved
// this separately by the time `yad disconnect` got it wrong, which is why the
// handling is in one place now, with a test over every command that takes a
// positional and a flag.
//
// n is how many positional arguments the command requires; usage is returned
// when they are missing or there are extra, and carries the next action.
func positional(fs *flag.FlagSet, args []string, n int, usage string) ([]string, error) {
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	rest := fs.Args()
	if len(rest) < n {
		return nil, errors.New(usage)
	}
	// Whatever follows them is flags again — and anything left after that is
	// an argument the command does not take.
	if err := fs.Parse(rest[n:]); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q — %s", fs.Arg(0), usage)
	}
	return rest[:n], nil
}
