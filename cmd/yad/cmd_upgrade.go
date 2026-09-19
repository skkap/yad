package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/skkap/yad/internal/buildinfo"
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
	src := upgrade.GH{}

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
	// control.Holder reads one profile's lock, and every profile on this
	// machine shares the binary just replaced — so silence here is not "no
	// runner is running", and an operator told nothing leaves a work runner
	// serving from the old file for good.
	restart := "restart it to pick this one up: `yad daemon restart`, or your service manager if it runs as one (`yad service status`)"
	if pid, running, err := control.Holder(g.paths); err == nil && running {
		fmt.Fprintf(w, "the runner (pid %d) is still on the old binary — %s\n", pid, restart)
	} else {
		fmt.Fprintf(w, "no runner is running under profile %s; one under another profile still holds the old binary — %s\n", g.paths.Profile, restart)
	}
	return nil
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
