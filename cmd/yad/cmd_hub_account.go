package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"
)

const hubAccountUsage = "usage: yad hub account remove [--hub url] [--token-file f] <runner> <harness> <account>"

// cmdHubAccount is `yad hub account`: a runner's accounts through the service
// API (decision 0057). Adding one is a login, `yad hub login start --add`.
func cmdHubAccount(ctx context.Context, g global, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "remove" {
		return errors.New(hubAccountUsage)
	}
	fs := flag.NewFlagSet("hub account remove", flag.ContinueOnError)
	hf := addHubFlags(fs, g)
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 3 {
		return errors.New(hubAccountUsage)
	}
	c, err := hf.client()
	if err != nil {
		return err
	}
	runnerID, harness, label := pos[0], pos[1], pos[2]
	a, err := c.RemoveAccount(ctx, runnerID, harness, label)
	if err != nil {
		return err
	}
	what := fmt.Sprintf("%s account %q", cleanLine(a.Harness), cleanLine(a.Account))
	// Waited for, as a login is: the removal is the runner's to make at its
	// next sync, and the one thing that says it did is its health.
	deadline := time.Now().Add(loginWait)
	for a.RemoveRequestedAt != nil {
		if time.Now().After(deadline) {
			return fmt.Errorf("runner %s has not removed %s in %s — is it syncing? The removal stands until it does; `%s` waits on it again",
				cleanLine(a.RunnerID), what, loginWait, hf.command([]string{"hub", "account", "remove"}, runnerID, harness, label))
		}
		select {
		case <-ctx.Done():
			// Nothing to undo: the removal stands, and the runner makes it
			// whether or not anyone waits.
			return fmt.Errorf("stopped waiting; runner %s still removes %s at its next sync: %w", cleanLine(a.RunnerID), what, ctx.Err())
		case <-time.After(loginPoll):
		}
		if a, err = c.Account(ctx, runnerID, harness, label); err != nil {
			return err
		}
	}
	if a.Listed {
		// The request ended without the account leaving: the runner stopped
		// advertising accounts to this hub, or a login added it again.
		return fmt.Errorf("runner %s still has %s and the removal is no longer asked for — a login added it again, or its owner turned manage_accounts off for this hub; `%s` shows its accounts",
			cleanLine(a.RunnerID), what, hf.command([]string{"hub", "runners"}, runnerID))
	}
	fmt.Fprintf(stdout, "%s is removed from runner %s: no new run takes it, a run already on it finishes there, and its login is deleted once the last one has ended\n",
		what, cleanLine(a.RunnerID))
	return nil
}
