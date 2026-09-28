package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/supervise"
)

func cmdVersion(w io.Writer) error {
	if buildinfo.Commit != "" {
		_, err := fmt.Fprintf(w, "yad %s (%s)\n", buildinfo.Version, buildinfo.Commit)
		return err
	}
	_, err := fmt.Fprintf(w, "yad %s\n", buildinfo.Version)
	return err
}

// cmdDoctor answers "would this machine be any use as a runner?" — the first
// question on every new box, which a bare `yad daemon start` answers far too late.
func cmdDoctor(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the raw detection result")
	if err := fs.Parse(args); err != nil {
		return err
	}
	found := capability.Detect(ctx)
	// The login check needs to know which harnesses have accounts. A profile
	// with no config yet has none, which is what Load's default says; one
	// that cannot be read is no reason to refuse the rest of the answer.
	if cfg, err := config.Load(g.paths); err == nil {
		capability.DefaultLogins(ctx, found, cfg)
		// Read-only, as `yad account list` reads it (decision 0043). States
		// that cannot be read change nothing here: `yad account list` says why.
		if accounts, err := account.Read(ctx, g.paths, account.ListsOf(cfg), time.Now()); err == nil {
			accountLogins(found, accounts, g.paths)
		}
	}
	if *asJSON {
		return writeJSON(w, found)
	}
	failing, unanswered := daemonModelsFailures(ctx, g.paths)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HARNESS\tSTATUS\tVERSION\tPATH")
	ready, broken, needsLogin, noAdapter := 0, 0, 0, 0
	for _, d := range found {
		status := "—"
		switch {
		case d.NeedsLogin:
			status = "needs login"
			needsLogin++
		case d.Present && d.Error != "":
			status = "broken"
			broken++
		case d.Ready():
			status = "ready"
			ready++
		case d.Present:
			status = "no adapter"
			noAdapter++
		}
		detail := d.Version
		if d.Error != "" {
			detail = d.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", d.Label, status, truncate(detail, 40), d.Path)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, d := range found {
		// In full, because the column above is truncated and what a harness
		// error carries now is the next action (DEV-60) — the footer below
		// tells the owner to fix the errors above, so they have to be readable.
		if d.Error != "" {
			fmt.Fprintf(w, "\nerror: %s — %s\n", d.Label, d.Error)
		}
		for _, warn := range d.Warnings {
			fmt.Fprintf(w, "\nwarning: %s — %s\n", d.Label, warn)
		}
		for _, f := range failing {
			if f.Harness != d.ID {
				continue
			}
			who := d.Label
			if f.Account != "" {
				who += ", account " + f.Account
			}
			fmt.Fprintf(w, "\nwarning: %s — the daemon could not ask it which models it offers (since %s), so hubs are sent the last list it gave or the catalog's: %s\n",
				who, f.Since.Local().Format("2006-01-02 15:04"), f.Reason)
		}
	}
	if unanswered {
		fmt.Fprintf(w, "\nnote: the daemon did not answer, so why a harness's models may be the catalog's is not shown — `%s` says what it is doing\n", g.paths.Command("status"))
	}
	// How the machine is set up rather than what is installed on it, so these
	// carry no harness label, and they stay out of --json: that is the
	// detection result a hub's capability document is built from, and a
	// directory's mode is none of a hub's business.
	//
	// They never change the exit code. docs/run-it-safely.md sends an owner
	// here to see their harnesses and their exposures at once, and a doctor
	// that failed on a group-readable directory would stop reporting the
	// harnesses they ran it for.
	for _, warn := range config.Exposures(g.paths) {
		fmt.Fprintf(w, "\nwarning: %s\n", warn)
	}
	for _, warn := range accountVariableWarnings(os.Environ()) {
		fmt.Fprintf(w, "\nwarning: %s\n", warn)
	}
	fmt.Fprintf(w, "\nprofile %s — config %s\n", g.paths.Profile, g.paths.Config)
	// Each way to reach zero has a different next action, and telling someone
	// with both CLIs installed to install them is worse than saying nothing.
	switch {
	case ready > 0:
	case broken > 0:
		// Checked before noAdapter: a broken Claude beside a working Codex needs
		// fixing, not installing.
		fmt.Fprintln(w, "No drivable harness: an installed one failed its version probe — fix the errors above and run this again.")
		return nil
	case needsLogin > 0:
		// Its version probe passed; the error above names the login to run.
		fmt.Fprintln(w, "No drivable harness: an installed one is not logged in — log it in as the error above says, and run this again.")
		return nil
	case noAdapter > 0:
		fmt.Fprintln(w, "No drivable harness: what is installed has no adapter in this yad yet — install Claude Code or Codex, which have one.")
		return nil
	default:
		fmt.Fprintln(w, "No drivable harness found. Install Claude Code or Codex and run this again.")
		return nil
	}
	fmt.Fprintf(w, "%d harness(es) this runner can be given work for.\n", ready)
	return nil
}

// daemonModelsFailures is each login the profile's daemon could not ask for
// its models, and why (DEV-146). The daemon's, not this command's own: its ask
// is the one behind the models_source a hub shows, from the environment and
// the binary the daemon resolved, and asking again here would start every
// login's harness for seconds each to learn something the daemon already
// knows. Asked over the control socket, since the CLI never writes state.db
// and this is not in it (decision 0043). No daemon is no answer and nothing
// to say; unanswered is one that holds the lock and did not answer.
func daemonModelsFailures(ctx context.Context, p config.Paths) (failing []control.ModelsFailure, unanswered bool) {
	res, err := control.Ask(ctx, p, "status")
	if _, wedged := errors.AsType[*control.UnresponsiveError](err); wedged {
		return nil, true
	}
	if err != nil || res.Status == nil {
		return nil, false
	}
	return res.Status.ModelsFailures, false
}

// accountVariableWarnings tells the owner about each variable in env that
// chooses a harness's credential, which supervise.Scrub removes from every
// child (decision 0060). An owner who exported ANTHROPIC_API_KEY expecting
// runs to bill it would otherwise see them use the account and never learn
// why. A warning and never a failure: the runner works as it should either
// way, and this is only what it does with what it found.
//
// The name and never the value, which is a credential. It is this shell's
// environment, the one a `yad daemon start` typed here would inherit; the
// daemon logs the same about its own when it starts, for one a service
// manager started with another.
func accountVariableWarnings(env []string) []string {
	var out []string
	for _, name := range supervise.AccountVariables(env) {
		why, _ := v1.AccountVariable(name)
		out = append(out, fmt.Sprintf("%s is set in this environment, and yad removes it from every harness it starts: %s %s — "+
			"a run uses its account's login instead, or the harness's own login when it has no accounts (decision 0060). "+
			"To bill runs that way, add an account logged in with it; for a project's own use, a hub sends the value as a grant under another name", name, name, why))
	}
	return out
}

// accountLogins reports a harness whose every account needs login as needing
// one, as DefaultLogins does for a harness with none. Its runs never use the
// default home, so only its accounts say whether it can take one, and health
// already calls such a harness not ready; doctor calling it ready is how a
// hub login that ended without taking went unseen on the machine (DEV-138).
// A limited account still counts: it comes back on its own at its reset.
func accountLogins(found []harness.Detected, accounts []account.Account, p config.Paths) {
	for i, d := range found {
		mine := account.For(accounts, d.ID)
		if !d.Ready() || len(mine) == 0 || slices.ContainsFunc(mine, func(a account.Account) bool { return a.State != v1.AccountNeedsLogin }) {
			continue
		}
		found[i].NeedsLogin = true
		// AddArgs: a token account parked on a refused token is logged in
		// again with a new token, never by a login it would outrank (0054).
		found[i].Error = fmt.Sprintf("none of its accounts is logged in — `%s` logs %q in, and `%s` shows each",
			p.Command(account.AddArgs(d.ID, mine[0].Label, mine[0].Home)...), mine[0].Label, p.Command("account", "list"))
	}
}

// cmdHarnesses prints the capability document this runner registers with, so a
// mismatch can be diagnosed on the machine instead of from a hub's logs. It is
// every hub's document but for one feature: accounts, which each hub whose
// connection allows it is sent on top (capability.ForConnection, decision 0057).
func cmdHarnesses(ctx context.Context, g global, args []string, w, errw io.Writer) error {
	fs := flag.NewFlagSet("harnesses", flag.ContinueOnError)
	// --json is accepted for symmetry with doctor; the document is always JSON.
	fs.Bool("json", true, "print the capability document as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(g.paths)
	if err != nil {
		return err
	}
	id, err := g.paths.RunnerID()
	if err != nil {
		return err
	}
	// The document is what a hub receives, and a state database this binary
	// cannot read — one migration behind, because the daemon has not restarted
	// — is no reason to refuse to print it. capability.Build reports the
	// owner's configured labels as free when it is given none.
	//
	// The note goes to stderr: the document goes to w and has to stay
	// parseable, and an owner reading "free" for an account that may need
	// login should be told the states were never read.
	accounts, err := account.Read(ctx, g.paths, account.ListsOf(cfg), time.Now())
	if err != nil {
		fmt.Fprintln(errw, "note: account states could not be read, so every configured account is shown free:", err)
		accounts = nil
	}
	return writeJSON(w, capability.Build(ctx, id, cfg, accounts))
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// truncate shortens s to at most n bytes plus the ellipsis, cutting on a rune
// boundary. The boundary is the point: these strings carry em dashes, and a
// cut mid-rune prints a replacement character in the first diagnostic anyone
// runs on a new machine. The cell it fills has no width of its own — tabwriter
// sizes columns in runes from what they hold — so n is only this table's own
// bound on one line, and the whole text is printed below the table anyway.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n - 1
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
