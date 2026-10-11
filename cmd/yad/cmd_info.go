package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
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
	"github.com/skkap/yad/internal/hostool"
	"github.com/skkap/yad/internal/supervise"
)

func cmdVersion(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print the version, commit and protocol majors as JSON, as a runner taking a release over reads them")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(w, buildinfo.Current())
	}
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
	// The login check needs to know which harnesses have accounts. A profile
	// with no config yet has none, which is what Load's default says; one
	// that cannot be read is no reason to refuse the rest of the answer, and
	// is said below, since the daemon will not start with it.
	cfg, cfgErr := config.Load(g.paths)
	if cfgErr == nil {
		// Before the probes, which then see what the daemon's will.
		supervise.SetOwnerEnv(cfg.Env)
	}
	found := capability.Detect(ctx)
	var locked, silent map[string]bool
	var sharedErr error
	if cfgErr == nil {
		capability.DefaultLogins(ctx, found, cfg)
		// Before accountLogins: an account's state is the daemon's own
		// check, made where the daemon runs, and this session's keychain
		// says nothing about it.
		locked = keychainLocked(found, os.Getenv)
		// Read-only, as `yad account list` reads it (decision 0043). States
		// that cannot be read change nothing here: `yad account list` says why.
		accounts, silences, err := account.ReadAll(ctx, g.paths, account.ListsOf(cfg), time.Now())
		if err == nil {
			accountLogins(found, accounts, g.paths)
			// The daemon's record of runs gone silent (decision 0077), read
			// as the capability document reads it, so doctor says not ready
			// exactly when health does.
			silent = capability.SilentHomes(found, cfg, accounts, silences, time.Now(), g.paths.Command)
		}
		// With or without the states: a harness with no accounts runs from
		// its default home, which needs none of them to look in.
		sharedErr = capability.SharedHomes(ctx, found, cfg, accounts, g.paths.Command, true)
	}
	if *asJSON {
		return writeJSON(w, found)
	}
	failing, unanswered := daemonModelsFailures(ctx, g.paths)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "HARNESS\tSTATUS\tVERSION\tPATH")
	ready, broken, needsLogin, noAdapter, unknown, notReady := 0, 0, 0, 0, 0, 0
	for _, d := range found {
		status := "—"
		switch {
		case locked[d.ID]:
			status = "unknown over ssh"
			unknown++
		case d.NeedsLogin:
			status = "needs login"
			needsLogin++
		case d.Present && d.Error != "":
			status = "broken"
			broken++
		case silent[d.ID]:
			// Drivable, and given no run until its cool-down ends: the
			// warning below says until when and why.
			status = "not ready"
			notReady++
		case d.Ready():
			status = "ready"
			ready++
		case d.Present:
			status = "no adapter"
			noAdapter++
		}
		detail := d.Version
		if d.Error != "" && !locked[d.ID] {
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
		switch {
		case locked[d.ID]:
			fmt.Fprintf(w, "\nnote: %s — this doctor runs over SSH, where macOS keeps the login keychain locked, so %s's own login reads as missing here whether or not it is there. "+
				"A runner service in the desktop session reads the keychain unlocked; a daemon started from this SSH session cannot read it either. "+
				"For a true answer, run `%s` in a terminal on the Mac itself, or unlock the keychain in this session with `security unlock-keychain` and run it again\n",
				d.Label, d.ID, g.paths.Command("doctor"))
		case d.Error != "":
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
			fmt.Fprintf(w, "\nwarning: %s — the daemon could not ask it which models it offers (since %s), so hubs are sent the last list it gave, the catalog's, or none: %s\n",
				who, f.Since.Local().Format("2006-01-02 15:04"), f.Reason)
		}
	}
	if sharedErr != nil {
		fmt.Fprintf(w, "\nnote: this machine's processes could not be listed, so whether another Claude runs from the home yad's runs use is not shown: %v\n", sharedErr)
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
	if cfgErr != nil {
		fmt.Fprintf(w, "\nerror: config.toml could not be read, and the daemon will not start until it can — %v\n", cfgErr)
	} else {
		for _, line := range envNotes(cfg, g.paths, os.Getenv) {
			fmt.Fprintf(w, "\n%s\n", line)
		}
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
	case unknown > 0:
		// Before needsLogin: "log it in" is the wrong fix for a login this
		// session cannot see, and logging in again over SSH would not help.
		fmt.Fprintln(w, "No harness confirmed drivable: over SSH this doctor cannot read the login keychain — check it from the Mac itself, as the note above says.")
		return nil
	case needsLogin > 0:
		// Its version probe passed; the error above names the login to run.
		fmt.Fprintln(w, "No drivable harness: an installed one is not logged in — log it in as the error above says, and run this again.")
		return nil
	case notReady > 0:
		fmt.Fprintln(w, "No harness ready now: the runs of an installed one have gone silent, and it is given none until its cool-down ends — the warning above says when, and what to do.")
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

// envNotes is what doctor says of config.toml's [env]: the names it gives
// every child, never a value, and a warning when docker's current context
// names a socket a tool reading no context cannot find and DOCKER_HOST is not
// among them. The context is read as a daemon started from this shell would
// read it — [env] first, then this environment.
func envNotes(cfg config.Config, p config.Paths, getenv func(string) string) []string {
	var out []string
	if len(cfg.Env) > 0 {
		out = append(out, fmt.Sprintf("note: config.toml's [env] gives every child yad starts %s (values not shown)", strings.Join(slices.Sorted(maps.Keys(cfg.Env)), ", ")))
	}
	if _, ok := cfg.Env["DOCKER_HOST"]; ok {
		return out
	}
	lookup := func(k string) string {
		if v, ok := cfg.Env[k]; ok {
			return v
		}
		return getenv(k)
	}
	if ctx, socket, ok := hostool.OffDefaultDockerSocket(lookup); ok {
		out = append(out, fmt.Sprintf("warning: docker's context %q points at %s, not %s, and config.toml's [env] does not declare DOCKER_HOST. "+
			"docker itself follows the context, but a tool that reads no context — Testcontainers among them — finds no daemon in a run. "+
			"Declare it in %s, then restart the daemon:\n\n    [env]\n    DOCKER_HOST = \"unix://%s\"",
			ctx, socket, hostool.DefaultDockerSocket, p.ConfigFile(), socket))
	}
	return out
}

// keychainLogin is account.KeychainLogin, swapped by a test so the macOS
// answer is tested on every platform.
var keychainLogin = account.KeychainLogin

// keychainLocked is each harness whose default login check said "not logged
// in" from an SSH session on a Mac (DEV-170). macOS keeps the login keychain
// locked to a session that did not log in at the console, so Claude cannot
// read its login there and answers as if it had none: a verdict about this
// session, not about the machine or the runner service. Only the default
// login's: each account's state is the daemon's own check.
func keychainLocked(found []harness.Detected, getenv func(string) string) map[string]bool {
	if getenv("SSH_CONNECTION") == "" && getenv("SSH_TTY") == "" {
		return nil
	}
	var out map[string]bool
	for _, d := range found {
		if d.NeedsLogin && keychainLogin(d.ID) {
			if out == nil {
				out = map[string]bool{}
			}
			out[d.ID] = true
		}
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
	// Probed as the daemon probes, or the document printed is not the one
	// a hub receives.
	supervise.SetOwnerEnv(cfg.Env)
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
	accounts, silences, err := account.ReadAll(ctx, g.paths, account.ListsOf(cfg), time.Now())
	if err != nil {
		fmt.Fprintln(errw, "note: account states could not be read, so every configured account is shown free:", err)
		accounts, silences = nil, nil
	}
	return writeJSON(w, capability.Build(ctx, id, cfg, capability.State{Accounts: accounts, Silences: silences, Paths: g.paths}))
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
