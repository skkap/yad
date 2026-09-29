package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/runner"
	"github.com/skkap/yad/internal/store"
)

const disconnectUsage = "usage: yad disconnect <name> [--now] [--force]"

// cmdDisconnect is `yad disconnect <name>`: in one fixed order, the hub
// retires the runner, the connection leaves config.toml and its credential is
// deleted, and a running daemon is told to let the connection go (decision
// 0069). It never writes the state database (decision 0043): the daemon ends
// what the connection left, now if it is running and at its next start if
// not.
//
// Every way it can stop part-way says what was and was not retired, and
// running the same command again finishes the job: the hub answers that it no
// longer knows the credential, a credential no connection names is deleted,
// and the daemon is told again.
func cmdDisconnect(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("disconnect", flag.ContinueOnError)
	now := fs.Bool("now", false, "stop the runs this hub has in progress here rather than refusing; the hub records them lost")
	force := fs.Bool("force", false, "remove the connection here even when its hub cannot be asked or will not agree; the runner stays registered there, to retire by hand")
	pos, err := positional(fs, args, 1, disconnectUsage)
	if err != nil {
		return err
	}
	name := pos[0]
	if err := config.ValidName(name); err != nil {
		return fmt.Errorf("connection name: %w", err)
	}
	// The command as typed, for every retry this prints: a retry without
	// --now would refuse again, and one without --force would stop at the
	// hub it went past.
	typed := []string{"disconnect", name}
	if *now {
		typed = append(typed, "--now")
	}
	if *force {
		typed = append(typed, "--force")
	}
	again := g.paths.Command(typed...)

	if !*now {
		held, where, err := inProgress(ctx, g.paths, name)
		if err != nil {
			return fmt.Errorf("which runs of %q are in progress could not be read (%w), so nothing was retired or removed — run `%s` again, or `%s` to stop whatever it has in progress",
				name, err, again, g.paths.Command(append(typed, "--now")...))
		}
		if len(held) > 0 {
			return fmt.Errorf("%d run(s) from the hub of %q are in progress here: %s. Nothing was retired or removed — let them finish (`%s` lists them) and run `%s` again, or `%s` stops them now, and the hub records them lost",
				len(held), name, strings.Join(held, ", "), g.paths.Command(where), again, g.paths.Command(append(typed, "--now")...))
		}
	}

	reason := "its owner disconnected it from this hub"
	if *now {
		reason += ", stopping the runs it had in progress"
	}
	d, err := runner.Disconnect(ctx, g.paths, name, *force, reason)
	var nc *runner.NotConnectedError
	if errors.As(err, &nc) {
		return disconnectLeftover(ctx, g, nc, again, w)
	}
	if err != nil {
		return err
	}
	shown := config.RedactURL(d.Connection.URL)
	switch d.Hub {
	case runner.HubRetired:
		fmt.Fprintf(w, "%s has retired this runner: the runs it held there are lost, the runs it had offered are back in its queue, and its sessions there are closed, with the runs queued in them ended\n", shown)
	case runner.HubAlreadyRetired:
		fmt.Fprintf(w, "%s no longer knew this runner's credential — an earlier disconnect retired it, or the hub dropped it — so there was nothing left to retire there\n", shown)
	case runner.HubStillRegistered:
		fmt.Fprintf(w, "%s was not told (%v): it still has this runner (%s) registered, with whatever runs and sessions it holds for it — retire it there\n", shown, d.Refusal, d.RunnerID)
	}
	fmt.Fprintf(w, "%q is removed from %s and its credential deleted\n", name, g.paths.ConfigFile())

	done := fmt.Sprintf("%q is disconnected from %s and removed from %s", name, shown, g.paths.ConfigFile())
	fallback := fmt.Sprintf("until then, its loop for %q stops at its next sync, when the hub refuses the credential, and ends what it left here then", name)
	if d.Hub == runner.HubStillRegistered {
		fallback = fmt.Sprintf("until then, it keeps syncing %q with %s, which still accepts the credential", name, shown)
	}
	return tellRemoved(ctx, g, name, done, fallback, again, d.Remaining, w)
}

// inProgress names the runs of a connection that are in progress here, and
// the command that lists them. A running daemon is asked, since it knows what
// it holds. Otherwise the state database is read, read-only: with no daemon,
// a run parked on a usage limit or a start time is in progress — the next
// start resumes it — while a run a crashed process held is not, being lost
// already (decision 0030); a daemon that does not answer may be running any
// of them.
func inProgress(ctx context.Context, p config.Paths, name string) (runs []string, where string, err error) {
	res, err := control.Ask(ctx, p, "status")
	var wedged *control.UnresponsiveError
	switch {
	case err == nil:
		for _, r := range res.Status.Runs {
			if r.Connection == name {
				runs = append(runs, fmt.Sprintf("%s (%s, session %s)", cleanLine(r.ID), cleanLine(r.State), cleanLine(r.Session)))
			}
		}
		return runs, "status", nil
	case errors.Is(err, control.ErrNotRunning), errors.As(err, &wedged):
	default:
		return nil, "", err
	}
	st, err := store.OpenProfile(ctx, p)
	if errors.Is(err, store.ErrNoState) {
		return nil, "sessions", nil
	}
	if err != nil {
		return nil, "", err
	}
	defer st.Close()
	held, err := st.ListHeldRuns(ctx, name)
	if err != nil {
		return nil, "", err
	}
	for _, r := range held {
		if wedged == nil && r.State != string(v1.RunWaiting) {
			continue
		}
		runs = append(runs, fmt.Sprintf("%s (%s, session %s)", cleanLine(r.ID), cleanLine(r.State), cleanLine(r.SessionID)))
	}
	return runs, "sessions", nil
}

// disconnectLeftover is a disconnect of a name config.toml does not list: an
// earlier one that stopped after the hub had answered — its credential file
// left behind, or its daemon not told — or a connection taken out of
// config.toml by hand. It finishes what is left of it.
func disconnectLeftover(ctx context.Context, g global, nc *runner.NotConnectedError, again string, w io.Writer) error {
	name := nc.Name
	has, err := g.paths.HasCredentialFile(name)
	if err != nil {
		return fmt.Errorf("%w; whether it left a credential file at %s could not be read (%v) — fix that and run `%s` again", nc, g.paths.CredentialFile(name), err, again)
	}
	if has {
		if err := g.paths.DeleteCredential(name); err != nil {
			return fmt.Errorf("%w, and the credential file it left at %s could not be deleted (%v) — run `%s` again, or delete the file", nc, g.paths.CredentialFile(name), err, again)
		}
		// The hub is not named: the URL left with the entry. A disconnect
		// removes the entry only once the hub has let the runner go, so this
		// credential is dead — unless the entry was removed by hand.
		fmt.Fprintf(w, "%s no longer lists %q; the credential file it left behind is deleted. If it was taken out of config.toml by hand rather than by `yad disconnect`, its hub was never told: retire this runner there\n", g.paths.ConfigFile(), name)
	}
	res, err := askRemoved(ctx, g.paths, name)
	switch {
	case errors.Is(err, control.ErrNotRunning) && !has:
		return fmt.Errorf("%w — if an earlier `%s` removed it, the next `%s` ends what it left here; otherwise `%s` adds a hub",
			nc, g.paths.Command("disconnect", name), g.paths.Command("daemon", "start"), g.paths.Command("connect", "<hub url>", "--token", "<token>"))
	case errors.Is(err, control.ErrNotRunning):
		fmt.Fprintf(w, "no daemon is running; the next `%s` ends what %q left here — its sessions, and runs parked or left by a crash — and reclaims their workdirs\n", g.paths.Command("daemon", "start"), name)
		return nil
	case err != nil:
		return fmt.Errorf("%s no longer lists %q, but the running daemon was not told (%v) — run `%s` again, or `%s`", g.paths.ConfigFile(), name, err, again, g.paths.Command("daemon", "restart"))
	}
	if !has && !res.Known && !res.Already && len(res.Ended) == 0 && res.Closed+res.Closing == 0 {
		return fmt.Errorf("%w, and the running daemon holds nothing of it", nc)
	}
	if !has {
		fmt.Fprintf(w, "%s no longer lists %q\n", g.paths.ConfigFile(), name)
	}
	printRemoval(w, g.paths, name, res)
	return nil
}

// tellRemoved asks a running daemon to let the connection go, and says what
// it did. done is what is already true, for a failure's message.
func tellRemoved(ctx context.Context, g global, name, done, fallback, again string, remaining int, w io.Writer) error {
	res, err := askRemoved(ctx, g.paths, name)
	switch {
	case errors.Is(err, control.ErrNotRunning):
		fmt.Fprintf(w, "no daemon is running; the next `%s` ends what %q left here — its sessions, and runs parked or left by a crash — and reclaims their workdirs\n", g.paths.Command("daemon", "start"), name)
		if remaining == 0 {
			fmt.Fprintf(w, "no hub is connected now — `%s` adds one\n", g.paths.Command("connect", "<hub url>", "--token", "<token>"))
		}
		return nil
	case err != nil:
		return fmt.Errorf("%s, but the running daemon was not told (%v) — run `%s` again to tell it, or `%s`; %s", done, err, again, g.paths.Command("daemon", "restart"), fallback)
	}
	printRemoval(w, g.paths, name, res)
	return nil
}

// askRemoved is OpConnectionRemoved, bounded past the daemon's own bound so
// its answer — a timeout of its own included — arrives rather than this side
// giving up first.
func askRemoved(ctx context.Context, p config.Paths, name string) (control.ConnectionRemoval, error) {
	ctx, cancel := context.WithTimeout(ctx, control.RemovalDeadline+5*time.Second)
	defer cancel()
	res, err := control.Send(ctx, p, control.Request{Op: control.OpConnectionRemoved, Connection: name})
	switch {
	case err != nil:
		return control.ConnectionRemoval{}, err
	case res.Removed == nil:
		return control.ConnectionRemoval{}, fmt.Errorf("the daemon answered without saying what it did — `%s` after an upgrade", p.Command("daemon", "restart"))
	}
	return *res.Removed, nil
}

// printRemoval says what the running daemon did.
func printRemoval(w io.Writer, p config.Paths, name string, r control.ConnectionRemoval) {
	switch {
	case !r.Known:
		fmt.Fprintf(w, "the running daemon was not syncing %q — it was connected after the daemon started\n", name)
	case r.Already:
		fmt.Fprintf(w, "the running daemon had already let %q go\n", name)
	default:
		fmt.Fprintf(w, "the running daemon has let %q go: its sync loop has stopped, and the other connections carry on\n", name)
	}
	if len(r.Stopped) > 0 {
		fmt.Fprintf(w, "stopped %d run(s) of it — %s; the hub records them lost\n", len(r.Stopped), cleanList(r.Stopped))
	}
	if len(r.Ended) > 0 {
		fmt.Fprintf(w, "ended %d run(s) of it that no process held — parked, or left by an earlier process — %s\n", len(r.Ended), cleanList(r.Ended))
	}
	if r.Closed+r.Closing > 0 {
		fmt.Fprintf(w, "closed %d session(s) of it here", r.Closed)
		if r.Closing > 0 {
			fmt.Fprintf(w, ", and %d more close once the run stopped in each has ended", r.Closing)
		}
		fmt.Fprintln(w, "; their workdirs are reclaimed")
	}
	if r.Remaining == 0 {
		fmt.Fprintf(w, "no hub is connected now: the daemon stays up, reclaiming workdirs, until `%s` — after `%s`, `%s` makes it sync with the new hub\n",
			p.Command("daemon", "stop"), p.Command("connect", "<hub url>", "--token", "<token>"), p.Command("daemon", "restart"))
	}
}

// cleanList is run ids from a hub, safe to print on one line.
func cleanList(ids []string) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = cleanLine(id)
	}
	return strings.Join(out, ", ")
}
