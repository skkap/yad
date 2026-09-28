package harness

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/skkap/yad/internal/probe"
	"github.com/skkap/yad/internal/supervise"
)

// Detected is one catalog entry as it actually exists on this machine.
type Detected struct {
	Harness
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	// Present is there being a binary every run starts for this harness: Path.
	// Not that it works — Error says whether it does.
	Present bool `json:"present"`
	// Error is what is wrong with this harness and what to do about it. It
	// never carries a word the harness printed,
	// nor the path it was started from: the capability document reaches every
	// connected hub, and a child's stderr is unbounded text nobody vetted — a
	// proxy URL with a password in it, a loader error naming the owner's home
	// (DEV-60).
	Error string `json:"error,omitempty"`
	// Warnings are what is wrong with a harness that can still be driven: an
	// override naming nothing while PATH has the binary, and the readiness
	// checks capability.Detect adds beyond the version probe. The same rule as
	// Error binds them.
	Warnings []string `json:"warnings,omitempty"`
	// NeedsLogin is an Error that is a missing login: in the harness's own
	// default home (capability.DefaultLogins), or, in `yad doctor`, in every
	// account the owner gave it. Installed and working, and unable to take
	// runs until someone logs it in.
	NeedsLogin bool `json:"needs_login,omitempty"`
}

// Ready reports whether a run may target this harness right now: first-class,
// installed, and answering its version probe.
func (d Detected) Ready() bool {
	return d.Present && d.Error == "" && d.Kind == FirstClass
}

// versionTimeout caps one `<harness> --version`. A CLI that hangs on its own
// version flag is broken — it happened to Claude through a bun regression and
// stalled every registration in Multica — so the probe is bounded and the
// failure is reported as text.
const versionTimeout = 5 * time.Second

// VersionTimeoutForTests replaces versionTimeout while it is positive. A test
// needs two budgets the shipped value cannot give it: a short one for a probe
// that is meant to hang, so the case waits as little as it can, and a long one
// for a fake that is meant to answer — a script spawned beside a whole
// race-instrumented suite has taken over five seconds to print its version
// (DEV-100), and a probe cut off then reports a timeout where the test injected
// something else. The shipped value stays where it is: it is how long a CLI
// broken this way holds up every capability probe. Nothing outside
// a test may set it: TestOnlyTestsReachTheProbeTimeouts, in internal/hostool,
// fails on any shipped file of the module that assigns it, this one included.
var VersionTimeoutForTests time.Duration

func versionWait() time.Duration {
	if VersionTimeoutForTests > 0 {
		return VersionTimeoutForTests
	}
	return versionTimeout
}

// versionOutputCap bounds what a probe may print. A version is one line; a CLI
// that prints megabytes is broken, and must not grow the runner's memory.
const versionOutputCap = 64 << 10

// Detect probes every catalog entry concurrently and returns them in catalog
// order. It never returns an error: a missing or broken harness is a fact about
// the machine that belongs in the capability document, not a reason to fail.
func Detect(ctx context.Context) []Detected {
	cat := Catalog()
	out := make([]Detected, len(cat))
	var wg sync.WaitGroup
	for i, h := range cat {
		wg.Add(1)
		go func(i int, h Harness) {
			defer wg.Done()
			out[i] = detectOne(ctx, h)
		}(i, h)
	}
	wg.Wait()
	return out
}

// Locate finds the binary for a harness the way detection does, so a run
// starts exactly the executable its capability document advertised — the one
// on PATH when an override names nothing.
func Locate(id string) (string, bool) {
	h, ok := Lookup(id)
	if !ok {
		return "", false
	}
	f := find(h)
	return f.Path, f.Path != ""
}

func find(h Harness) probe.Found { return probe.Find(h.EnvPath, h.Binary, h.VersionArgs) }

func detectOne(ctx context.Context, h Harness) Detected {
	d := Detected{Harness: h}

	f := find(h)
	d.Error = f.Error
	if f.Warning != "" {
		d.Warnings = append(d.Warnings, f.Warning)
	}
	if f.Path == "" {
		return d // absent, and that is not an error unless an override was set
	}
	d.Path, d.Present = f.Path, true

	// Through supervise like every other child: the probe runs every sync, and a
	// wrapper script or node/bun launcher that forks and hangs must take its
	// whole process group with it, not leave one orphan per tick.
	ctx, cancel := context.WithTimeout(ctx, versionWait())
	defer cancel()
	out, err := supervise.Run(ctx, supervise.Spec{Path: f.Path, Args: h.VersionArgs}, versionOutputCap)
	// A start failure never ran, so it is never a timeout: Run reports
	// TimedOut only for a leader that was running when ctx ended. None of the
	// three quotes the error or the child: see internal/probe.
	switch {
	case err != nil:
		d.Error = f.WontStart()
	case out.TimedOut:
		d.Error = f.NoAnswer(versionWait(), h.VersionArgs...)
	case out.Err != nil:
		d.Error = f.WontAnswer(h.VersionArgs...)
	default:
		d.Version = ParseVersion(string(out.Stdout))
	}
	return d
}

// versionToken is what a version looks like: two to four dotted numbers and
// an optional pre-release or build suffix of letters, digits and dots, standing
// as a word of its own — after a space, a bracket, a quote or a "v", before a
// space, a comma or a closing bracket. The charset is the guarantee: a path
// needs a slash and a URL a colon, and neither can be in a match. The
// delimiters keep a number that is part of one out: the address in a proxy
// URL, a version inside a path.
var versionToken = regexp.MustCompile(`(?:^|[\s("'])v?(\d+(?:\.\d+){1,3}(?:[-+][0-9A-Za-z]+(?:\.[0-9A-Za-z]+)*)?)(?:$|[\s,;)"'])`)

// maxVersionLen bounds the token. The longest real one is a pre-release such
// as 1.0.0-beta.12+build.345, well inside it; the bound keeps a suffix from
// carrying a sentence.
const maxVersionLen = 32

// ParseVersion finds the version in what a CLI printed for `--version`, or
// returns "" when there is none.
//
// The CLIs disagree about what they print: a bare "2.4.1", "claude 2.4.1
// (Claude Code)", "git version 2.51.0", a banner with an update notice under
// it, or a launcher's warning above it. What they share is a dotted number,
// and that is all that is kept. The line around it is the child's own text,
// and it goes into the capability document every connected hub reads: a
// wrapper's proxy URL with a password in it, the path it was installed under
// in the owner's home (DEV-67). The first line holding a version wins, so a
// warning printed first does not hide it; one holding none reports no version
// rather than itself.
func ParseVersion(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		if m := versionToken.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			v := m[1]
			if len(v) > maxVersionLen {
				v = strings.TrimRight(v[:maxVersionLen], ".-+")
			}
			return v
		}
	}
	return ""
}
