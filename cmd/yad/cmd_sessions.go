package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/skkap/yad/internal/store"
)

// session is one row of `yad sessions --json`. Its own type rather than the
// store's row, so a column added to the table is not an output change.
type session struct {
	Connection string    `json:"connection"`
	ID         string    `json:"id"`
	Harness    string    `json:"harness"`
	NativeID   string    `json:"native_id,omitempty"`
	State      string    `json:"state"`
	Workdir    string    `json:"workdir"`
	Created    time.Time `json:"created"`
	LastUsed   time.Time `json:"last_used"`
	Runs       int64     `json:"runs"`
	LiveRun    string    `json:"live_run,omitempty"`
}

// cmdSessions lists the sessions this profile's runner holds. It reads the
// state database itself rather than asking the daemon, so it answers with
// the daemon stopped too — which is when an owner is most likely to wonder
// what is on disk.
func cmdSessions(ctx context.Context, g global, args []string, w io.Writer) error {
	if len(args) > 0 && args[0] == "close" {
		return errors.New("`yad sessions close` arrives with Zumino task DEV-18, the rest of epic E4 — until then a hub closes a session (ARCHITECTURE.md §3)")
	}
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the sessions as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected %q — `yad sessions [--json]` lists every session", fs.Arg(0))
	}
	var list []session
	s, err := store.OpenReadOnly(ctx, g.paths.StateDB())
	switch {
	case errors.Is(err, store.ErrNoState):
	case err != nil:
		return err
	default:
		defer s.Close()
		rows, err := s.ListSessions(ctx)
		if err != nil {
			return fmt.Errorf("reading sessions from %s: %w", g.paths.StateDB(), err)
		}
		for _, r := range rows {
			list = append(list, session{
				Connection: r.Connection, ID: r.ID, Harness: r.Harness, NativeID: r.NativeID.String,
				State: r.State, Workdir: r.Workdir, Created: time.UnixMilli(r.CreatedAt).UTC(),
				LastUsed: time.UnixMilli(r.LastUsedAt).UTC(), Runs: r.Runs, LiveRun: r.LiveRun,
			})
		}
	}
	if *asJSON {
		if list == nil {
			list = []session{}
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(list)
	}
	printSessions(w, list, time.Now())
	return nil
}

func printSessions(w io.Writer, list []session, now time.Time) {
	if len(list) == 0 {
		fmt.Fprintln(w, "no sessions — a hub opens one with a run's first claim")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CONNECTION\tSESSION\tHARNESS\tSTATE\tLAST USED\tRUNS\tLIVE RUN\tWORKDIR")
	for _, s := range list {
		live := s.LiveRun
		if live == "" {
			live = "-"
		}
		workdir := s.Workdir
		if workdir == "" {
			// Claimed, but no run of it has reached its workdir yet.
			workdir = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", cleanLine(s.Connection), cleanLine(s.ID), cleanLine(s.Harness), s.State,
			now.Sub(s.LastUsed).Round(time.Second).String()+" ago", s.Runs, cleanLine(live), cleanLine(workdir))
	}
	tw.Flush()
}
