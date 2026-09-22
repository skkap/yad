package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/harness"
	v1 "github.com/skkap/yad/protocol/v1"
)

const accountUsage = "usage: yad account add <harness> <label> | list [--json] | remove <harness> <label> [--yes]"

// Neither command here opens the state database for writing (decision 0043).
// It is the daemon's: they change config.toml, and tell a running daemon over
// the control socket, which re-reads the lists and writes what follows. With
// no daemon running nothing is written, and the daemon catches up at start.

func cmdAccount(ctx context.Context, g global, args []string, w io.Writer) error {
	if len(args) == 0 {
		return errors.New(accountUsage)
	}
	switch args[0] {
	case "add":
		return accountAdd(ctx, g, args[1:], w)
	case "list":
		return accountList(ctx, g, args[1:], w)
	case "remove":
		return accountRemove(ctx, g, args[1:], w)
	case "use":
		return accountUseRefusal(g.paths)
	}
	return fmt.Errorf("unknown account subcommand %q — %s", args[0], accountUsage)
}

// accountAdd makes an account's harness home, runs the harness's own login in
// it with the owner at the terminal (decision 0039), and adds the account to
// config.toml only once the harness's own login check says the home is logged
// in (decision 0043).
//
// YAD reads nothing the login writes. What it learns afterwards is one bit,
// from the check the login probe uses. Anything short of a definite yes — a
// login walked away from, a check that could not answer — leaves config.toml
// as it was and keeps the home, so running the command again picks up where
// this one stopped; an account that cannot take runs is never added.
func accountAdd(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("account add", flag.ContinueOnError)
	pos, err := positional(fs, args, 2, "usage: yad account add <harness> <label>")
	if err != nil {
		return err
	}
	id, label := pos[0], pos[1]
	if err := checkHarness(id); err != nil {
		return err
	}
	if err := config.ValidName(label); err != nil {
		return fmt.Errorf("account label: %w", err)
	}
	again := g.paths.Command("account", "add", id, label)
	// A login is a person at a terminal: it prints a code, opens a browser and
	// waits. Run from a script it would hang or fail silently.
	if !interactive() {
		return fmt.Errorf("`%s` runs the harness's own login and needs you at the terminal — run it from a shell on this machine (finishing a login from elsewhere is DEV-57, backlog)", again)
	}
	bin, ok := harness.Locate(id)
	if !ok {
		return fmt.Errorf("%s is not installed on this machine — `%s` shows where it was looked for", id, g.paths.Command("doctor"))
	}
	// Loaded here only to refuse a config.toml that does not parse before the
	// owner has spent a login on it; it is read again before it is written.
	if _, err := config.Load(g.paths); err != nil {
		return err
	}
	if err := g.paths.Ensure(); err != nil {
		return err
	}
	// Before the home is made or reused: a label removed while a run was on
	// it has a home the daemon deletes when that run ends, which could be in
	// the middle of this login (decision 0043). Nothing to act on if it fails:
	// with no daemon there is nothing pending, and a daemon that cannot
	// answer this will not answer the change either, which says so.
	// Bounded tighter than a change: the daemon reads nothing for this, and
	// the owner is waiting for the login to start.
	kctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, _ = tellDaemon(kctx, g.paths, control.AccountChange{Harness: id, Label: label, Keep: true})
	cancel()
	home, err := account.Ensure(g.paths.Data, id, label)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s account %q: running %s's own login in %s\n", id, label, id, home)
	fmt.Fprintln(w, "yad stores no token of its own; whatever the login writes stays in that directory.")

	loginErr := account.Login(ctx, id, bin, home, stdin, w, os.Stderr)
	in, checkErr := account.LoggedIn(ctx, id, bin, home)
	if checkErr != nil || !in {
		// Not added, and said as a failure: a non-zero exit is how a script
		// finds out. The login's own error, if it had one, goes with it.
		why := "the login did not complete"
		if checkErr != nil {
			why = fmt.Sprintf("whether the login took could not be read (%v)", checkErr)
		}
		return errors.Join(loginErr, fmt.Errorf("%s account %q was not added: %s. config.toml is unchanged, and %s is kept for the next try — run `%s` again when you can finish the login", id, label, why, home, again))
	}

	// Read again, not the copy from before the login: that took minutes, and
	// a `yad connect` in another terminal meanwhile must not be written over.
	cfg, err := config.Load(g.paths)
	if err != nil {
		return err
	}
	if err := addToConfig(g.paths, cfg, id, label); err != nil {
		return err
	}
	res, err := tellDaemon(ctx, g.paths, control.AccountChange{Harness: id, Label: label})
	var refused *daemonRefusal
	switch {
	case errors.Is(err, control.ErrNotRunning):
		fmt.Fprintf(w, "\n%s account %q is added and free; no daemon is running, and `%s` starts one that uses it.\n", id, label, g.paths.Command("daemon", "start"))
		return nil
	case errors.As(err, &refused):
		return fmt.Errorf("%s account %q is added to config.toml, and %w", id, label, err)
	case err != nil:
		return fmt.Errorf("%s account %q is added to config.toml, but the running daemon did not take it up (%v) — `%s` makes it read the file again", id, label, err, g.paths.Command("daemon", "restart"))
	}
	// The daemon's word for the state, not this command's: it is what a run
	// will read.
	switch v1.AccountState(res.State) {
	case v1.AccountFree:
		fmt.Fprintf(w, "\n%s account %q is added, and the running daemon has taken it up: it is free and will take runs.\n", id, label)
		return nil
	case v1.AccountNeedsLogin:
		// The daemon asked the same check a moment later and heard no. It
		// wrote that down, so no run will use the account, and the owner is
		// the one who can settle it.
		return fmt.Errorf("%s account %q is added, but the running daemon's own check finds no login in %s — run `%s` again", id, label, home, again)
	case v1.AccountLimited:
		// Logged in and at a usage limit a run recorded under this label:
		// nothing for the owner to do but wait, which `list` dates.
		fmt.Fprintf(w, "\n%s account %q is added, and the running daemon has taken it up: it is at a usage limit, and `%s` says until when.\n", id, label, g.paths.Command("account", "list"))
		return nil
	}
	return fmt.Errorf("%s account %q is added, but the running daemon could not say what state it is in — `%s` shows it", id, label, g.paths.Command("account", "list"))
}

// tellDaemon is OpAccountsChanged: the daemon re-reads config.toml's account
// lists and acts on this one account, and has done so when this returns. No
// daemon is control.ErrNotRunning; a daemon that answered with a refusal is a
// *daemonRefusal, which is not the same as one that did not answer.
func tellDaemon(ctx context.Context, p config.Paths, ch control.AccountChange) (control.AccountResult, error) {
	// Past the daemon's own bound, so its answer — a timeout of its own
	// included — arrives rather than this side giving up first.
	ctx, cancel := context.WithTimeout(ctx, control.AccountsDeadline+5*time.Second)
	defer cancel()
	res, err := control.Send(ctx, p, control.Request{Op: control.OpAccountsChanged, Account: &ch})
	switch {
	case err != nil && res.PID != 0:
		return control.AccountResult{}, &daemonRefusal{err: err}
	case err != nil:
		return control.AccountResult{}, err
	case res.Account == nil:
		return control.AccountResult{}, fmt.Errorf("the daemon answered without saying what it did — `%s` after an upgrade", p.Command("daemon", "restart"))
	}
	return *res.Account, nil
}

// daemonRefusal is a daemon that answered, and said no.
type daemonRefusal struct{ err error }

func (e *daemonRefusal) Error() string {
	return "the running daemon refused the change: " + e.err.Error()
}
func (e *daemonRefusal) Unwrap() error { return e.err }

func accountList(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("account list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "as JSON")
	if _, err := positional(fs, args, 0, "usage: yad account list [--json]"); err != nil {
		return err
	}
	cfg, err := config.Load(g.paths)
	if err != nil {
		return err
	}
	accounts, err := account.Read(ctx, g.paths, account.ListsOf(cfg), time.Now())
	if err != nil {
		return err
	}
	if *asJSON {
		// Its own shape, not a slice of half-filled v1.HarnessReports: this
		// is what the CLI prints, and nothing here should look like the
		// capability document a hub receives.
		//
		// The home is shown to the owner, whose machine it is, and never in
		// the JSON a script might send somewhere — what may be known about
		// an account elsewhere is its label and its state.
		type listed struct {
			Harness  string             `json:"harness"`
			Accounts []v1.AccountReport `json:"accounts"`
		}
		out := make([]listed, 0)
		for _, id := range cfg.HarnessIDs() {
			if reps := account.Reports(accounts, id); reps != nil {
				out = append(out, listed{Harness: id, Accounts: reps})
			}
		}
		return writeJSON(w, out)
	}
	if len(accounts) == 0 {
		fmt.Fprintf(w, "no accounts — runs use each harness's own login; `%s` adds one\n", g.paths.Command("account", "add", "<harness>", "<label>"))
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HARNESS\tLABEL\tSTATE\tWINDOWS\tSINCE\tHOME")
	for _, a := range accounts {
		since := "—"
		if !a.UpdatedAt.IsZero() {
			since = a.UpdatedAt.Local().Format(time.RFC3339)
		}
		state := string(a.State)
		if a.State == v1.AccountLimited && a.LimitedUntil != nil {
			state += " until " + a.LimitedUntil.Local().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", a.Harness, a.Label, state, windowsColumn(a.Windows), since, a.Home)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, a := range accounts {
		if a.State == v1.AccountNeedsLogin {
			fmt.Fprintf(w, "\n%s %q needs login: `%s`\n", a.Harness, a.Label, g.paths.Command("account", "add", a.Harness, a.Label))
		}
	}
	return nil
}

// windowsColumn is each usage window as the owner reads it: the harness's own
// name for the window, how much of it is spent and when it refills. An em dash
// for an account no run has been through yet — no window heard is not a window
// at zero.
func windowsColumn(ws []v1.AccountWindow) string {
	if len(ws) == 0 {
		return "—"
	}
	parts := make([]string, 0, len(ws))
	for _, w := range ws {
		p := fmt.Sprintf("%s %.0f%%", w.Name, w.UsedPercent)
		if w.ResetsAt != nil {
			p += " until " + w.ResetsAt.Local().Format(time.RFC3339)
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}

// accountRemove takes an account out of config.toml and deletes its harness
// home.
//
// The home holds the login, so this is the owner logging that account out of
// this machine, and it is not undoable: it asks first unless told not to.
//
// Who deletes the home depends on whether a daemon is running. With none, this
// command does, and nothing else is written — the daemon forgets the account's
// state at its next start (decision 0043). With one, the daemon does: it is the
// only one that knows whether a run is on the account, and a run that is
// finishes there, in a home that must still be on disk; the last run to let go
// of it deletes it.
func accountRemove(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("account remove", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask; the login in that home is deleted")
	pos, err := positional(fs, args, 2, "usage: yad account remove <harness> <label> [--yes]")
	if err != nil {
		return err
	}
	id, label := pos[0], pos[1]
	// Checked before anything is printed or deleted, as `add` does at its own
	// top: this argument becomes a path element under <data>/accounts/, and
	// what waits at the end of that path is os.RemoveAll.
	if err := checkHarness(id); err != nil {
		return err
	}
	if err := config.ValidName(label); err != nil {
		return fmt.Errorf("account label: %w", err)
	}
	home := account.HomeDir(g.paths.Data, id, label)
	if !*yes {
		if !interactive() {
			return fmt.Errorf("removing %s account %q deletes the login in %s — pass --yes to do it without being asked", id, label, home)
		}
		fmt.Fprintf(w, "remove %s account %q? This deletes the login in %s. The sessions in %s are shared and are kept. [y/N] ",
			id, label, home, account.TranscriptDir(g.paths.Data, id))
		if !confirmed() {
			fmt.Fprintln(w, "left alone")
			return nil
		}
	}
	cfg, err := config.Load(g.paths)
	if err != nil {
		return err
	}
	// Out of config.toml first: from here no daemon, running or starting,
	// gives the account a new run.
	if h, ok := cfg.Harness[id]; ok {
		if i := slices.Index(h.Accounts, label); i >= 0 {
			h.Accounts = slices.Delete(slices.Clone(h.Accounts), i, i+1)
			cfg.Harness[id] = h
			if err := config.Save(g.paths, cfg); err != nil {
				return err
			}
		}
	}
	res, err := tellDaemon(ctx, g.paths, control.AccountChange{Harness: id, Label: label, Removed: true})
	again := g.paths.Command("account", "remove", id, label, "--yes")
	var refused *daemonRefusal
	switch {
	case errors.Is(err, control.ErrNotRunning):
		if err := account.Remove(g.paths.Data, id, label); err != nil {
			return err
		}
		fmt.Fprintf(w, "removed %s account %q; %s is gone and the shared transcripts are untouched\n", id, label, home)
		return nil
	case errors.As(err, &refused):
		return fmt.Errorf("%s account %q is out of config.toml, and %w — run `%s` again", id, label, err, again)
	case err != nil:
		// Not deleted: a run may be on it, and only the daemon could say.
		return fmt.Errorf("%s account %q is out of config.toml, but the running daemon did not answer (%v), so its home %s is kept in case a run is using it — once `%s` answers, run `%s` again", id, label, err, home, g.paths.Command("status"), again)
	}
	if len(res.Runs) == 0 {
		fmt.Fprintf(w, "removed %s account %q; the running daemon has let it go, %s is gone, and the shared transcripts are untouched\n", id, label, home)
		return nil
	}
	fmt.Fprintf(w, "removed %s account %q; no new run takes it. Still on it, and finishing there: %s. %s is deleted when the last of them ends; the shared transcripts are untouched\n",
		id, label, strings.Join(res.Runs, ", "), home)
	return nil
}

// checkHarness refuses a harness YAD cannot give a home of its own, naming
// what it does know rather than the fact that this one is unknown.
func checkHarness(id string) error {
	if account.Supported(id) && account.CanLogIn(id) {
		return nil
	}
	var known []string
	for _, h := range harness.Catalog() {
		if account.Supported(h.ID) && account.CanLogIn(h.ID) {
			known = append(known, h.ID)
		}
	}
	return fmt.Errorf("yad has no account home for %q — accounts work for %v, and a harness without one uses its own login", id, known)
}

// addToConfig appends a label to the harness's account order if it is not

// addToConfig appends a label to the harness's account list if it is not
// already there. The list says which accounts take part and breaks ties among
// them; which one a run takes is the soonest refill (decision 0039).
func addToConfig(p config.Paths, cfg config.Config, id, label string) error {
	h := cfg.Harness[id]
	if slices.Contains(h.Accounts, label) {
		return nil
	}
	h.Accounts = append(slices.Clone(h.Accounts), label)
	if cfg.Harness == nil {
		cfg.Harness = map[string]config.HarnessConfig{}
	}
	cfg.Harness[id] = h
	return config.Save(p, cfg)
}

// interactive says a person is at the terminal: both a keyboard to type the
// login into and a screen for it to draw on. A variable so a test can stand
// at the terminal; nothing else assigns it.
var interactive = func() bool {
	return charDevice(stdin) && charDevice(os.Stdout)
}

func charDevice(v any) bool {
	f, ok := v.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func confirmed() bool {
	var answer string
	if _, err := fmt.Fscanln(stdin, &answer); err != nil {
		return false
	}
	return answer == "y" || answer == "Y" || answer == "yes"
}
