package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/runner"
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
	// Closing is a close asked for while a run is held: the session closes
	// when it ends.
	Closing     bool       `json:"closing,omitempty"`
	CloseReason string     `json:"close_reason,omitempty"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	// Reclaimed is a closed session whose workdir is gone; HubTold one whose
	// close its hub has acknowledged.
	Reclaimed bool `json:"reclaimed,omitempty"`
	HubTold   bool `json:"hub_told,omitempty"`
}

// cmdSessions lists the sessions this profile's runner holds. It reads the
// state database itself rather than asking the daemon, so it answers with
// the daemon stopped too — which is when an owner is most likely to wonder
// what is on disk.
func cmdSessions(ctx context.Context, g global, args []string, w io.Writer) error {
	if len(args) > 0 && args[0] == "close" {
		return cmdSessionsClose(ctx, g, args[1:], w)
	}
	fs := flag.NewFlagSet("sessions", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the sessions as JSON")
	if _, err := positional(fs, args, 0, "`yad sessions [--json]` lists every session"); err != nil {
		return err
	}
	var list []session
	s, err := store.OpenProfile(ctx, g.paths)
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
			s := session{
				Connection: r.Connection, ID: r.ID, Harness: r.Harness, NativeID: r.NativeID.String,
				State: r.State, Workdir: r.Workdir, Created: time.UnixMilli(r.CreatedAt).UTC(),
				LastUsed: time.UnixMilli(r.LastUsedAt).UTC(), Runs: r.Runs, LiveRun: r.LiveRun,
				Closing: r.State == "open" && r.CloseRequestedAt.Valid, CloseReason: r.CloseReason.String,
				Reclaimed: r.ReclaimedAt.Valid, HubTold: r.ReportedAt.Valid,
			}
			if r.ClosedAt.Valid {
				t := time.UnixMilli(r.ClosedAt.Int64).UTC()
				s.ClosedAt = &t
			}
			list = append(list, s)
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
		switch {
		case s.Reclaimed:
			workdir = "(reclaimed)"
		case workdir == "":
			// Claimed, but no run of it has reached its workdir yet.
			workdir = "-"
		}
		state := s.State
		switch {
		case s.Closing:
			state = "closing"
		case s.CloseReason != "" && s.CloseReason != s.State:
			state += " (" + s.CloseReason + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", cleanLine(s.Connection), cleanLine(s.ID), cleanLine(s.Harness), cleanLine(state),
			now.Sub(s.LastUsed).Round(time.Second).String()+" ago", s.Runs, cleanLine(live), cleanLine(workdir))
	}
	tw.Flush()
}

// cmdSessionsClose is `yad sessions close <id>`: the owner closes a session
// on this machine. The daemon does it — the state database is its — and the
// session's hub hears it was closed by the owner. The connection is found
// from the id when only one holds it.
func cmdSessionsClose(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("sessions close", flag.ContinueOnError)
	conn := fs.String("connection", "", "the connection the session belongs to, when more than one has that id")
	pos, err := positional(fs, args, 1, "usage: yad sessions close [--connection name] <session> — `yad sessions` lists them")
	if err != nil {
		return err
	}
	id := pos[0]
	if *conn == "" {
		found, err := sessionConnections(ctx, g, id)
		if err != nil {
			return err
		}
		switch len(found) {
		case 0:
			return fmt.Errorf("this runner has no session %q — `%s` lists the ones it holds", id, g.paths.Command("sessions"))
		case 1:
			*conn = found[0]
		default:
			return fmt.Errorf("session %q exists on connections %s — name one with --connection", id, strings.Join(found, ", "))
		}
	}
	res, err := control.Send(ctx, g.paths, control.Request{Op: "close_session", Connection: *conn, Session: id})
	if errors.Is(err, control.ErrNotRunning) {
		return fmt.Errorf("the daemon closes sessions, and none is running for this profile — `%s`, then run this again", g.paths.Command("daemon", "start"))
	}
	if err != nil {
		return err
	}
	c := res.Closed
	if c == nil {
		return fmt.Errorf("the daemon answered without saying what it did — `%s` after an upgrade", g.paths.Command("daemon", "restart"))
	}
	switch c.Outcome {
	case runner.CloseDone:
		fmt.Fprintf(w, "session %s closed; its workdir is being removed, and hub %s hears of it at the next sync\n", id, *conn)
	case runner.CloseWaiting:
		fmt.Fprintf(w, "session %s has run %s in it, and closes when that run ends — cancel it from its hub to close sooner\n", id, c.LiveRun)
	case runner.CloseAlready:
		fmt.Fprintf(w, "session %s was already closed (%s)\n", id, c.Reason)
	case runner.CloseUnknown:
		return fmt.Errorf("connection %s has no session %q — `%s` lists the ones this runner holds", *conn, id, g.paths.Command("sessions"))
	default:
		fmt.Fprintf(w, "session %s: %s\n", id, c.Outcome)
	}
	return nil
}

// sessionConnections is every connection holding a session by this id.
func sessionConnections(ctx context.Context, g global, id string) ([]string, error) {
	s, err := store.OpenProfile(ctx, g.paths)
	if errors.Is(err, store.ErrNoState) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer s.Close()
	rows, err := s.ListSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading sessions from %s: %w", g.paths.StateDB(), err)
	}
	var found []string
	for _, r := range rows {
		if r.ID == id {
			found = append(found, r.Connection)
		}
	}
	return found, nil
}
