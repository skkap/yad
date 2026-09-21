package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/skkap/yad/internal/supervise"
)

// Detected is one catalog entry as it actually exists on this machine.
type Detected struct {
	Harness
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Present bool   `json:"present"`
	// Error is what is wrong with this harness and what to do about it. It
	// never carries a word the harness printed,
	// nor the path it was started from: the capability document reaches every
	// connected hub, and a child's stderr is unbounded text nobody vetted — a
	// proxy URL with a password in it, a loader error naming the owner's home
	// (DEV-60).
	Error string `json:"error,omitempty"`
	// Warnings are readiness checks beyond the version probe that failed
	// without making the harness undrivable; capability.Detect fills them.
	Warnings []string `json:"warnings,omitempty"`
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
// starts exactly the executable its capability document advertised.
func Locate(id string) (string, bool) {
	h, ok := Lookup(id)
	if !ok {
		return "", false
	}
	path, _, found := locate(h)
	return path, found
}

// locate says where the binary is and which of the two places it came from.
// Which one is what a broken harness's report sends its owner to: the override
// they set, or whatever PATH resolved.
func locate(h Harness) (path string, fromEnv, found bool) {
	if path := os.Getenv(h.EnvPath); path != "" {
		return path, true, true
	}
	path, err := exec.LookPath(h.Binary)
	return path, false, err == nil
}

func detectOne(ctx context.Context, h Harness) Detected {
	d := Detected{Harness: h}

	path, fromEnv, found := locate(h)
	if !found {
		return d // absent, and that is not an error
	}
	d.Path, d.Present = path, true

	// Through supervise like every other child: the probe runs every sync, and a
	// wrapper script or node/bun launcher that forks and hangs must take its
	// whole process group with it, not leave one orphan per tick.
	ctx, cancel := context.WithTimeout(ctx, versionWait())
	defer cancel()
	out, err := supervise.Run(ctx, supervise.Spec{Path: path, Args: h.VersionArgs}, versionOutputCap)
	// A start failure never ran, so it is never a timeout: Run reports
	// TimedOut only for a leader that was running when ctx ended.
	switch {
	case err != nil:
		d.Error = wontStart(h, fromEnv)
	case out.TimedOut:
		d.Error = noAnswer(h)
	case out.Err != nil:
		d.Error = wontAnswer(h)
	default:
		d.Version = ParseVersion(string(out.Stdout))
	}
	return d
}

// wontStart and wontAnswer are the two things that go wrong with a harness the
// runner found, said without quoting it. The wrapped exec error names the
// binary's absolute path — under /Users/<name> on a Mac, which is the owner's
// name — and a harness's own stderr is unbounded text nobody vetted: a dyld
// failure listing libraries under that same home, a proxy URL with a password
// in it. Neither travels. What a hub can act on is that the harness does not
// work; what its owner needs is where to look, and an override's *name* is safe
// where its value is the thing that leaks.
//
// The two are worded apart because what is still worth checking differs.
// locate does not stat an override, so a YAD_<ID>_PATH naming nothing at all
// reaches here and the override itself is the thing to fix. LookPath has
// already proved the other one exists and is executable, so telling its owner
// to check that would send them to `ls -l` and a dead end: what is left is a
// missing interpreter, a binary for another architecture, or this machine
// failing to fork, and running it by hand is what tells them which.
func wontStart(h Harness, fromEnv bool) string {
	if fromEnv {
		return fmt.Sprintf("%s does not name a %s this runner can start — point it at an executable %s, or unset it and let PATH decide", h.EnvPath, h.Binary, h.Binary)
	}
	return fmt.Sprintf("the %s on PATH will not start — run `%s %s` on this machine to see what stops it", h.Binary, h.Binary, strings.Join(h.VersionArgs, " "))
}

// noAnswer is a probe the harness never came back from. It names the command
// and the wait and nothing else — the same rule as the two above — and gives
// the action because this is the case where it is worth most: a CLI that hangs
// on its own version flag has stopped telling its owner anything at all.
func noAnswer(h Harness) string {
	return fmt.Sprintf("no answer to `%s %s` within %s — run it on this machine to see what it waits on", h.Binary, strings.Join(h.VersionArgs, " "), versionWait())
}

func wontAnswer(h Harness) string {
	return fmt.Sprintf("`%s %s` exited with an error — run it on this machine to see why", h.Binary, strings.Join(h.VersionArgs, " "))
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
