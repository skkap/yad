package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/skkap/yad/internal/config"
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
	printStatus(w, g.paths, *s, time.Now())
	return nil
}

// cleanLine is clean for one cell of a table: run ids, models, errors and log
// attributes can come from a hub, and a newline or tab in one would draw rows
// of its own.
func cleanLine(s string) string {
	return strings.NewReplacer("\n", " ", "\t", " ").Replace(clean(s))
}

// printUpdate is the self-update, when config.toml has it on (decision 0071):
// where the schedule stands, then any release refused or waiting to take
// over. The version running is the status's first line; a pending release is
// the one installed on disk.
func printUpdate(w io.Writer, u control.Update, now time.Time) {
	ago := func(t time.Time) string { return now.Sub(t).Round(time.Second).String() + " ago" }
	if u.Off != "" {
		fmt.Fprintf(w, "self-update on, and does nothing on this build: %s\n", cleanLine(u.Off))
		return
	}
	line := "self-update on — "
	switch {
	case u.LastCheck == nil:
		line += "not checked yet"
	case u.LastError != "":
		line += fmt.Sprintf("the last check, %s, failed: %s", ago(*u.LastCheck), cleanLine(u.LastError))
	default:
		line += fmt.Sprintf("checked %s, newest release %s", ago(*u.LastCheck), cleanLine(u.Latest))
	}
	if u.NextCheck != nil {
		line += fmt.Sprintf("; next check in %s", u.NextCheck.Sub(now).Round(time.Second))
	}
	fmt.Fprintln(w, line)
	if r := u.Refused; r != nil {
		fmt.Fprintf(w, "  refused %s %s: %s\n", cleanLine(r.Tag), ago(r.At), cleanLine(r.Reason))
	}
	if p := u.Pending; p != nil {
		if p.Swapping {
			fmt.Fprintf(w, "  %s installed %s; taking over — %s. No new runs; the runner re-executes as %s once the runs held have ended\n",
				cleanLine(p.Tag), ago(p.Since), cleanLine(p.Reason), cleanLine(p.Tag))
		} else {
			fmt.Fprintf(w, "  %s installed %s in place of this binary; the runner becomes it at its first idle moment, or drains for it at %s\n",
				cleanLine(p.Tag), ago(p.Since), p.By.Local().Format(time.DateTime))
		}
	}
}

func printStatus(w io.Writer, p config.Paths, s control.Status, now time.Time) {
	ago := func(t time.Time) string { return now.Sub(t).Round(time.Second).String() + " ago" }
	state := ""
	if s.Stopping {
		state = " — STOPPING"
	}
	fmt.Fprintf(w, "runner %s (%s) — profile %s, yad %s, pid %d, up %s%s\n",
		s.Name, s.RunnerID, s.Profile, s.Version, s.PID, now.Sub(s.Started).Round(time.Second), state)
	fmt.Fprintf(w, "capacity %d of %d free · %d session(s) open · spool %d · outbox %d\n",
		s.Capacity.Free, s.Capacity.Total, s.Sessions, s.SpoolDepth, s.OutboxDepth)
	// Only when off: it is the one workdir setting that refuses a run a hub
	// may expect to be taken, and an owner who set it long ago otherwise
	// learns of it only from that run's error. Named as config.toml names it,
	// like manage_accounts below, since undoing it is an edit there and a
	// restart.
	if s.PathSources != nil && !*s.PathSources {
		fmt.Fprintln(w, "sources on the machine switched off (path_sources = false under [workdirs] in config.toml) — a run with a path source or a local git URL is refused")
	}
	if s.Update != nil {
		printUpdate(w, *s.Update, now)
	}

	fmt.Fprintln(w)
	if len(s.Connections) == 0 {
		fmt.Fprintf(w, "no connections — `%s` adds a hub\n", p.Command("connect", "<hub url>", "--token", "<token>"))
	} else {
		fmt.Fprintln(w, "connections")
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, c := range s.Connections {
			last := "never synced"
			if c.LastSync != nil {
				last = "synced " + ago(*c.LastSync)
			}
			held := fmt.Sprintf("%d run(s)", c.Held)
			if c.Cap > 0 {
				held = fmt.Sprintf("%d of %d run(s)", c.Held, c.Cap)
			}
			// The setting's own name, since changing it is an edit to
			// config.toml and a restart.
			var accounts string
			switch {
			case c.ManageAccounts == nil:
			case *c.ManageAccounts:
				accounts = "manage_accounts on"
			default:
				accounts = "manage_accounts off"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\t%s\n", cleanLine(c.Name), c.State, held, last, accounts, cleanLine(c.URL))
			if c.LastError != "" && c.LastErrorAt != nil {
				fmt.Fprintf(tw, "  \t\tlast error %s: %s\t\n", ago(*c.LastErrorAt), cleanLine(c.LastError))
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
				detail += " — " + cleanLine(r.Reason)
			}
			fmt.Fprintf(tw, "  %s/%s\t%s\t%s/%s\tsession %s\t%s\n", cleanLine(r.Connection), cleanLine(r.ID), cleanLine(r.State), cleanLine(r.Harness), cleanLine(r.Model), cleanLine(r.Session), detail)
		}
		tw.Flush()
	}

	if len(s.Errors) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintf(w, "recent warnings and errors (`%s` has everything)\n", p.Command("daemon", "logs"))
		for _, e := range s.Errors {
			line := fmt.Sprintf("  %s %-5s %s", e.Time.Local().Format(time.DateTime), e.Level, cleanLine(e.Message))
			if e.Attrs != "" {
				line += "  " + cleanLine(e.Attrs)
			}
			fmt.Fprintln(w, line)
		}
	}
}
