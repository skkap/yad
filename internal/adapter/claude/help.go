package claude

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/skkap/yad/internal/probe"
	"github.com/skkap/yad/internal/supervise"
)

// Every run passes flags a Claude Code release may not know, and Claude
// refuses an unknown flag before it does anything: a runner with such a Claude
// would claim every run and fail each at its arguments. AGENTS.md's guardrail
// is that a runner never accepts a run it cannot drive, so the capability
// probe asks the installed claude's --help for each flag, and a Claude lacking
// one is reported with an error, which keeps every hub from offering it runs.
// The flags' first releases are not known for certain, so the probe asks
// rather than comparing versions (decision 0050).

// requiredFlags are the flags argv passes on every run that Claude Code has
// not always had.
var requiredFlags = []string{"--system-prompt-snapshot"}

// helpTimeout bounds the probe. --help answers in well under a second; one
// that takes seconds is a claude broken in a way its version probe reports.
const helpTimeout = 10 * time.Second

// helpOutputCap bounds what the probe keeps. Claude's help is some 20 KB.
const helpOutputCap = 1 << 20

// helpRetry is how long a probe that could not run is believed, as for the
// codex schema check: the next may run, and a warning that outlives its cause
// misleads the owner and every hub.
const helpRetry = 10 * time.Minute

type helpAnswer struct {
	err, warning string
	until        time.Time // zero: final for this binary and version
}

var (
	helpMu    sync.Mutex
	helpAsked = map[string]helpAnswer{}
	// helpNow is swapped by a test.
	helpNow = time.Now
)

// FlagsCheck asks the installed claude — the one detection found — whether it
// knows every flag a run passes. It returns an error for the capability
// document when it definitely does not, which keeps the harness from taking
// runs, and a warning when the question could not be asked, which leaves it
// drivable: the adapter still fails a run on such a Claude saying to upgrade
// it. Both are the runner's words, built with found's commands; nothing claude
// printed is quoted. Kept per binary and version, so an upgrade is asked
// again, and a probe that could not run only for helpRetry.
func FlagsCheck(ctx context.Context, found probe.Found, version string) (errMsg, warning string) {
	key := found.Path + "\x00" + version
	helpMu.Lock()
	a, ok := helpAsked[key]
	helpMu.Unlock()
	if ok && (a.until.IsZero() || helpNow().Before(a.until)) {
		return a.err, a.warning
	}
	a, done := askHelp(ctx, found)
	if !done {
		// The caller stopped asking; that says nothing about claude.
		return "", ""
	}
	helpMu.Lock()
	helpAsked[key] = a
	helpMu.Unlock()
	return a.err, a.warning
}

func askHelp(ctx context.Context, found probe.Found) (helpAnswer, bool) {
	pctx, cancel := context.WithTimeout(ctx, helpTimeout)
	defer cancel()
	out, err := supervise.Run(pctx, supervise.Spec{Path: found.Path, Args: []string{"--help"}}, helpOutputCap)
	if ctx.Err() != nil {
		return helpAnswer{}, false
	}
	couldNot := func(why string) (helpAnswer, bool) {
		return helpAnswer{warning: "yad could not check which flags this claude knows, and runs may still work: " + why, until: helpNow().Add(helpRetry)}, true
	}
	switch {
	case err != nil:
		return couldNot(found.WontStart())
	case out.TimedOut:
		return couldNot(found.NoAnswer(helpTimeout, "--help"))
	case out.Err != nil:
		return couldNot(found.WontAnswer("--help"))
	}
	help := string(out.Stdout)
	for _, flag := range requiredFlags {
		if !strings.Contains(help, flag) {
			return helpAnswer{err: "this claude is older than yad needs: it does not know " + flag + ", which every run passes — " +
				found.Do(found.Command("update"), "upgrade it")}, true
		}
	}
	return helpAnswer{}, true
}
