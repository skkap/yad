package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/upgrade"
)

// cmdUpgrade replaces this binary with the newest tagged release, on the
// owner's command. Nothing else in yad calls it: there is no poll, no hub
// message and no schedule that reaches here (decision 0018), and it restarts
// nothing — a runner already running holds the file it started from until
// someone restarts it, which is said out loud rather than done.
func cmdUpgrade(ctx context.Context, g global, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	check := fs.Bool("check", false, "say what the newest release is and change nothing")
	force := fs.Bool("force", false, "replace the binary even when it is not older than the newest release")
	tag := fs.String("tag", "", "a release to install instead of the newest — naming one installs it whether it is newer or older, which is how a bad release is rolled back")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// flag stops parsing at the first positional argument, so `yad upgrade
	// v0.3.1 --check` would drop the --check and replace the binary for real.
	// Every other command here refuses leftovers for smaller reasons.
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q — a release goes in --tag, as `yad upgrade --tag %s`", fs.Arg(0), fs.Arg(0))
	}
	src := releaseSource()

	want := *tag
	if want == "" {
		var err error
		if want, err = src.Latest(ctx); err != nil {
			return err
		}
	}
	fmt.Fprintf(w, "installed  %s\n", buildinfo.Version)
	fmt.Fprintf(w, "release    %s\n", want)

	state := upgrade.Compare(buildinfo.Version, want)
	if *check {
		fmt.Fprintln(w, checkLine(state, want, *tag != ""))
		return nil
	}
	// Only Behind is what a bare `yad upgrade` asks for. Replacing a binary
	// with one that is not newer — an unstamped `go build` in someone's
	// checkout, a tag that moved backwards — is a thing to say and stop on
	// rather than do quietly; --force and an explicit --tag are the two ways
	// of meaning it.
	if *tag == "" && !*force && state != upgrade.Behind {
		fmt.Fprintln(w, checkLine(state, want, false))
		return nil
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("cannot tell which yad is running: %w", err)
	}
	res, err := upgrade.Apply(ctx, upgrade.Options{
		Source: src,
		Target: exe,
		GOOS:   runtime.GOOS,
		GOARCH: runtime.GOARCH,
		Tag:    want,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "checked its sha256 against the release and replaced %s with %s\n", res.Path, res.Tag)
	// `yad daemon restart` starts a daemon of its own. Under a service manager
	// that is the wrong move: a clean stop is meant to stay stopped, so the
	// unit's crash restart would be replaced by an unsupervised process.
	// Decision 0028 settles it — re-running install replaces the unit and
	// starts it again — and nothing here can tell which kind this is, so both
	// are named rather than one guessed.
	restart := restartAdvice(g.paths.Profile)
	// control.Holder reads one profile's lock, and every profile on this
	// machine shares the binary just replaced — so silence is not "nothing is
	// running", only "nothing is running here".
	pid, running, err := control.Holder(g.paths)
	switch {
	case err != nil:
		fmt.Fprintf(w, "could not tell whether a runner is running under profile %s (%v) — if one is, %s\n", g.paths.Profile, err, restart)
	case running:
		fmt.Fprintf(w, "the runner (pid %d) is still on the old binary — %s\n", pid, restart)
	default:
		fmt.Fprintf(w, "no runner is running under profile %s; any running under another profile is still on the old binary — %s\n", g.paths.Profile, restart)
	}
	return nil
}

// releaseSource is where releases come from. scripts/install.sh takes YAD_REPO
// to install from a fork, and an upgrade that ignored it would replace that
// fork's binary with upstream's — the install source silently lost at the one
// moment the binary changes. Nothing records the repository at install time,
// so the operator sets it the same way both times.
func releaseSource() upgrade.GH {
	return upgrade.GH{Repo: os.Getenv("YAD_REPO")}
}

// restartAdvice is what to type to put a running runner on the new binary.
// Both commands have to carry the profile the message names, and they take it
// in different places: `yad service install` has a --profile flag of its own,
// while `yad daemon restart` has none and reads the global one before the
// subcommand. Printed bare beside "profile work", either would act on default
// — and `service install` would bootstrap a supervised unit for a profile
// nobody meant to run as a service (0028).
func restartAdvice(profile string) string {
	global, sub := "", ""
	if profile != config.DefaultProfile {
		global = " --profile " + profile
		sub = " --profile " + profile
	}
	return fmt.Sprintf("restart it to pick this one up: `yad service install%s` if this profile runs as a service (0028: install replaces the unit and starts it again), otherwise `yad%s daemon restart`", sub, global)
}

// checkLine is the one sentence that says where this build stands, and what
// the owner would type next. named says the tag was given with --tag rather
// than resolved: the sentence must not call it the newest release, and every
// command it offers has to carry the tag, or it names one release and installs
// another.
func checkLine(state upgrade.State, tag string, named bool) string {
	newest := ", the newest release"
	install := "`yad upgrade --force`"
	replace := "`yad upgrade`"
	if named {
		newest = ""
		install = fmt.Sprintf("`yad upgrade --tag %s`", tag)
		replace = install
	}
	switch state {
	case upgrade.Behind:
		return fmt.Sprintf("this build is older than %s — %s replaces it", tag, replace)
	case upgrade.Ahead:
		return fmt.Sprintf("this build is newer than %s%s — %s installs it anyway", tag, newest, install)
	case upgrade.Unstamped:
		return fmt.Sprintf("this build carries no release version (%q), so there is nothing to compare — %s installs %s", buildinfo.Version, install, tag)
	case upgrade.UnreadableTag:
		return fmt.Sprintf("%q is not a version number, so there is nothing to compare it with — %s installs it anyway", tag, install)
	default:
		return fmt.Sprintf("this build is %s%s — %s fetches it again", tag, newest, install)
	}
}
