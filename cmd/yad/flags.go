package main

import (
	"errors"
	"flag"
	"fmt"
)

// Go's flag package stops at the first non-flag argument, so a command that
// parses once and then reads fs.Arg refuses its own documented form the moment
// the operator writes the flag last — as `yad hub admin-token revoke <name>
// --db x` did for as long as that command has existed.
//
// Parsing around the positionals is therefore one function, used by every
// command that takes any. It had been solved separately in `yad connect`, in
// `yad account remove` and in parseInterleaved here; a problem solved twice in
// two places is one waiting to reappear in a third.

// parseInterleaved parses flags on both sides of the positional arguments, and
// between them, returning the positionals in order.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
}

// positional is parseInterleaved for a command that takes exactly n of them.
// usage is returned when they are missing or there are extra, and carries the
// next action.
func positional(fs *flag.FlagSet, args []string, n int, usage string) ([]string, error) {
	pos, err := parseInterleaved(fs, args)
	switch {
	case err != nil:
		return nil, err
	case len(pos) < n:
		return nil, errors.New(usage)
	case len(pos) > n:
		return nil, fmt.Errorf("unexpected argument %q — %s", pos[n], usage)
	}
	return pos, nil
}
