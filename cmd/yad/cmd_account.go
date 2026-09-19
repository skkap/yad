package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"text/tabwriter"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
	v1 "github.com/skkap/yad/protocol/v1"
)

const accountUsage = "usage: yad account add <harness> <label> | list [--json] | remove <harness> <label> [--yes]"

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
		return errors.New("`yad account use` arrives with failover (DEV-28, Zumino yad/dev) — until then the order in config.toml's `accounts` is the order runs take, and `yad account add` appends to it")
	}
	return fmt.Errorf("unknown `yad account %s` — %s", args[0], accountUsage)
}

// accountAdd makes an account's harness home and runs the harness's own login
// in it, with the owner at the terminal (decision 0039).
//
// YAD reads nothing the login writes. What it learns afterwards is one bit,
// from the harness's own login check: whether the home has a login. A home
// with none is an account in needs-login — it is kept, it is reported, and it
// is skipped for runs until the owner finishes the login.
func accountAdd(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("account add", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return errors.New("usage: yad account add <harness> <label>")
	}
	id, label := fs.Arg(0), fs.Arg(1)
	if err := checkHarness(id); err != nil {
		return err
	}
	if err := config.ValidName(label); err != nil {
		return fmt.Errorf("account label: %w", err)
	}
	// A login is a person at a terminal: it prints a code, opens a browser and
	// waits. Run from a script it would hang or fail silently and leave a home
	// with no login behind it, which is exactly the half-made account this
	// command must not produce without saying so.
	if !interactive() {
		return errors.New("`yad account add` runs the harness's own login and needs you at the terminal — run it from a shell on this machine (finishing a login from elsewhere is DEV-57, backlog)")
	}
	bin, ok := harness.Locate(id)
	if !ok {
		return fmt.Errorf("%s is not installed on this machine — `yad doctor` shows where it was looked for", id)
	}
	cfg, err := config.Load(g.paths)
	if err != nil {
		return err
	}
	if err := g.paths.Ensure(); err != nil {
		return err
	}
	home, err := account.Ensure(g.paths.Data, id, label)
	if err != nil {
		return err
	}
	// Recorded before the login runs: a login interrupted half way leaves an
	// account the owner can see and finish, not a directory nothing names.
	if err := addToConfig(g.paths, cfg, id, label); err != nil {
		return err
	}
	fmt.Fprintf(w, "%s account %q: running %s's own login in %s\n", id, label, id, home)
	fmt.Fprintln(w, "yad stores no token of its own; whatever the login writes stays in that directory.")

	loginErr := account.Login(ctx, id, bin, home, stdin, w, os.Stderr)
	in, checkErr := account.LoggedIn(ctx, id, bin, home)
	if checkErr != nil {
		// The account is configured and its home exists; what is unknown is
		// whether the login took. It is left free rather than marked, because
		// a check that cannot run is no evidence against the login — a run
		// that fails asks the same question again and settles it.
		return errors.Join(loginErr, fmt.Errorf("%s account %q is configured, but its login state could not be read: %w", id, label, checkErr))
	}
	state := v1.AccountNeedsLogin
	if in {
		state = v1.AccountFree
	}
	if err := recordState(ctx, g.paths, id, label, state); err != nil {
		return err
	}
	if in {
		fmt.Fprintf(w, "\n%s account %q is free and will take runs.\n", id, label)
		return nil
	}
	// Not an error in the state model — the account exists and is reported —
	// but not a success either, and a non-zero exit is how a script finds out.
	fmt.Fprintf(w, "\n%s account %q needs login: the home exists, it is reported to every hub, and no run will use it.\n", id, label)
	return fmt.Errorf("the login did not complete — run `yad account add %s %s` again when you can finish it", id, label)
}

func accountList(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("account list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected %q — usage: yad account list [--json]", fs.Arg(0))
	}
	cfg, err := config.Load(g.paths)
	if err != nil {
		return err
	}
	accounts, err := account.Read(ctx, g.paths, cfg)
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
		fmt.Fprintln(w, "no accounts — runs use each harness's own login; `yad account add <harness> <label>` adds one")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HARNESS\tLABEL\tSTATE\tSINCE\tHOME")
	for _, a := range accounts {
		since := "—"
		if !a.UpdatedAt.IsZero() {
			since = a.UpdatedAt.Local().Format(time.RFC3339)
		}
		state := string(a.State)
		if a.State == v1.AccountLimited && a.LimitedUntil != nil {
			state += " until " + a.LimitedUntil.Local().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", a.Harness, a.Label, state, since, a.Home)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, a := range accounts {
		if a.State == v1.AccountNeedsLogin {
			fmt.Fprintf(w, "\n%s %q needs login: `yad account add %s %s`\n", a.Harness, a.Label, a.Harness, a.Label)
		}
	}
	return nil
}

// accountRemove deletes an account's harness home and forgets the account.
//
// The home holds the login, so this is the owner logging that account out of
// this machine, and it is not undoable: it asks first unless told not to.
func accountRemove(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("account remove", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "do not ask; the login in that home is deleted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// flag stops at the first positional argument, so --yes is parsed on
	// either side of the harness and label.
	rest := fs.Args()
	if len(rest) < 2 {
		return errors.New("usage: yad account remove <harness> <label> [--yes]")
	}
	id, label := rest[0], rest[1]
	if err := fs.Parse(rest[2:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected %q — usage: yad account remove <harness> <label> [--yes]", fs.Arg(0))
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
	if err := account.Remove(g.paths.Data, id, label); err != nil {
		return err
	}
	cfg, err := config.Load(g.paths)
	if err != nil {
		return err
	}
	if h, ok := cfg.Harness[id]; ok {
		if i := slices.Index(h.Accounts, label); i >= 0 {
			h.Accounts = slices.Delete(slices.Clone(h.Accounts), i, i+1)
			cfg.Harness[id] = h
			if err := config.Save(g.paths, cfg); err != nil {
				return err
			}
		}
	}
	if err := forgetState(ctx, g.paths, id, label); err != nil {
		return err
	}
	fmt.Fprintf(w, "removed %s account %q; %s is gone and the shared transcripts are untouched\n", id, label, home)
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
// already there. Appending is the whole of the ordering: the owner's list is
// the order runs take, and rearranging it is editing config.toml.
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

func recordState(ctx context.Context, p config.Paths, id, label string, state v1.AccountState) error {
	st, err := store.Open(ctx, p.StateDB())
	if err != nil {
		return err
	}
	defer st.Close()
	return account.SetState(ctx, st.Queries, id, label, state, time.Now())
}

func forgetState(ctx context.Context, p config.Paths, id, label string) error {
	// No state database means no state to forget, and a remove is no reason
	// to create one.
	if _, err := os.Stat(p.StateDB()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	st, err := store.Open(ctx, p.StateDB())
	if err != nil {
		return err
	}
	defer st.Close()
	return st.DeleteAccount(ctx, db.DeleteAccountParams{Harness: id, Label: label})
}

// interactive says a person is at the terminal: both a keyboard to type the
// login into and a screen for it to draw on.
func interactive() bool {
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
