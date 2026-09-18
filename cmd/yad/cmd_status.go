package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/skkap/yad/internal/control"
)

// cmdStatus is what the running daemon is doing, read through its control
// socket.
func cmdStatus(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the status as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	res, err := control.Ask(ctx, g.paths, "status")
	if err != nil {
		return err
	}
	s := res.Status
	if *asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	printStatus(w, *s, time.Now())
	return nil
}

func printStatus(w io.Writer, s control.Status, now time.Time) {
	ago := func(t time.Time) string { return now.Sub(t).Round(time.Second).String() + " ago" }
	state := ""
	if s.Stopping {
		state = " — STOPPING"
	}
	fmt.Fprintf(w, "runner %s (%s) — profile %s, yad %s, pid %d, up %s%s\n",
		s.Name, s.RunnerID, s.Profile, s.Version, s.PID, now.Sub(s.Started).Round(time.Second), state)
	fmt.Fprintf(w, "capacity %d of %d free · %d session(s) open · spool %d · outbox %d\n",
		s.Capacity.Free, s.Capacity.Total, s.Sessions, s.SpoolDepth, s.OutboxDepth)

	fmt.Fprintln(w)
	if len(s.Connections) == 0 {
		fmt.Fprintln(w, "no connections — `yad connect <url> --token …` adds a hub")
	} else {
		fmt.Fprintln(w, "connections")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, c := range s.Connections {
			last := "never synced"
			if c.LastSync != nil {
				last = "synced " + ago(*c.LastSync)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", c.Name, c.State, last, c.URL)
			if c.LastError != "" && c.LastErrorAt != nil {
				fmt.Fprintf(tw, "  \t\tlast error %s: %s\t\n", ago(*c.LastErrorAt), c.LastError)
			}
		}
		tw.Flush()
	}

	fmt.Fprintln(w)
	if len(s.Runs) == 0 {
		fmt.Fprintln(w, "no runs held")
	} else {
		fmt.Fprintf(w, "runs (%d)\n", len(s.Runs))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, r := range s.Runs {
			detail := "since " + ago(r.Since)
			if r.ResumesAt != nil {
				detail = "resumes at " + r.ResumesAt.Local().Format(time.DateTime)
			}
			if r.Reason != "" {
				detail += " — " + r.Reason
			}
			fmt.Fprintf(tw, "  %s/%s\t%s\t%s/%s\tsession %s\t%s\n", r.Connection, r.ID, r.State, r.Harness, r.Model, r.Session, detail)
		}
		tw.Flush()
	}

	if len(s.Errors) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "recent warnings and errors (`yad daemon logs` has everything)")
		for _, e := range s.Errors {
			line := fmt.Sprintf("  %s %-5s %s", e.Time.Local().Format(time.DateTime), e.Level, e.Message)
			if e.Attrs != "" {
				line += "  " + e.Attrs
			}
			fmt.Fprintln(w, line)
		}
	}
}
