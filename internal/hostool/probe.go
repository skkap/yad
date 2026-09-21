package hostool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/supervise"
)

// Detected is one host tool as it exists on this machine.
type Detected struct {
	ID      string `json:"id"`
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Present bool   `json:"present"`
	// Error is what went wrong with this tool: a path override that names
	// nothing, a probe that timed out, a binary that would not run, a Docker
	// daemon that does not answer. It is a fact about the machine, never a
	// failure of detection, and it carries the next action wherever there is
	// one. The first of those comes with Present false — the tool is not
	// there, and the owner still needs to hear why.
	Error string `json:"error,omitempty"`
	// LoggedIn is nil for a tool with no notion of a login, and nil too when
	// the tool has one and the probe could not find out — Error says why.
	LoggedIn *bool `json:"logged_in,omitempty"`
	// LoginHosts are the hosts the tool is signed in to: github.com, a GitHub
	// Enterprise hostname, or both — a gh signed in to two reports two, since
	// one of them alone would route work away from a machine that can do it.
	// Who it is signed in *as* is never read, let alone reported — the
	// capability document is a public surface.
	LoginHosts []string `json:"login_hosts,omitempty"`
}

// versionTimeout and statusTimeout cap one probe each: the tool's `--version`,
// and the second question asked of a tool that has a state — gh's login,
// docker's daemon. Every one of them waits on something that can fail to
// answer rather than fail: a `--version` that hangs (it happened to Claude
// through a bun regression and stalled every registration in Multica), `gh auth
// status` validating a token against the host over the network, a `docker
// version` waiting on a daemon socket nothing is listening on. Five seconds is
// enough for all three on a working machine, and the probe runs again at the
// next interval.
//
// They are two values, equal when shipped, so a test can bound them apart.
const (
	versionTimeout = 5 * time.Second
	statusTimeout  = 5 * time.Second
)

// VersionTimeoutForTests and StatusTimeoutForTests replace the two timeouts
// above while they are positive. A test needs budgets the shipped values cannot
// give it: a short one for the probe it means to hang, so the case waits as
// little as it can, and a long one for every probe it means to answer — a
// script spawned beside a whole race-instrumented suite has taken over five
// seconds to print its version (DEV-100), and a probe cut off then reports a
// timeout where the test injected something else. A gh that hangs on its login
// needs both at once: its `--version` must answer, and only its `auth status`
// may hang. The shipped values stay where they are: they are how long a
// broken tool holds up every capability probe. Nothing outside a test may set
// either: TestOnlyTestsReachTheProbeTimeouts fails on any shipped file of the
// module that assigns one, this one included.
var VersionTimeoutForTests, StatusTimeoutForTests time.Duration

func versionWait() time.Duration {
	if VersionTimeoutForTests > 0 {
		return VersionTimeoutForTests
	}
	return versionTimeout
}

func statusWait() time.Duration {
	if StatusTimeoutForTests > 0 {
		return StatusTimeoutForTests
	}
	return statusTimeout
}

// outputCap bounds what one probe may print. A version is one line and
// `gh auth status --json` is a few hundred bytes; a tool that prints megabytes
// is broken, and must not grow the runner's memory.
const outputCap = 64 << 10

// Detect probes every catalog entry concurrently and returns them in catalog
// order. It never returns an error: a missing docker, a logged-out gh and a
// hanging `--version` are facts about the machine that belong in the capability
// document, not reasons to fail registration.
func Detect(ctx context.Context) []Detected {
	cat := Catalog()
	out := make([]Detected, len(cat))
	var wg sync.WaitGroup
	for i, t := range cat {
		wg.Add(1)
		go func(i int, t Tool) {
			defer wg.Done()
			out[i] = probeOne(ctx, t)
		}(i, t)
	}
	wg.Wait()
	return out
}

// locate finds the tool's binary. The second return says whether the override
// names something that is not there, which is the owner's mistake to hear about
// rather than a machine that simply has no docker: falling through to PATH
// would hide a mistyped override behind "no docker here".
func locate(t Tool) (path string, misconfigured bool) {
	if path := os.Getenv(t.EnvPath); path != "" {
		if _, err := os.Stat(path); err != nil {
			return "", true
		}
		return path, false
	}
	// LookPath, unlike the override above, already checks the file is there and
	// can be executed.
	path, err := exec.LookPath(t.Binary)
	if err != nil {
		return "", false
	}
	return path, false
}

func probeOne(ctx context.Context, t Tool) Detected {
	d := Detected{ID: t.ID}

	path, misconfigured := locate(t)
	switch {
	case misconfigured:
		// Not present: `present` means the binary was found, and a path that
		// names nothing did not find one.
		d.Error = t.EnvPath + " names a file that is not there — point it at the " + t.Binary + " binary, or unset it and let PATH decide"
		return d
	case path == "":
		return d // absent, and that is not an error
	}
	d.Path, d.Present = path, true

	out, err := run(ctx, path, t.VersionArgs, false, versionWait())
	switch {
	case err != nil:
		d.Error = wontRun(t.Binary)
		return d
	case out.TimedOut:
		d.Error = noAnswer(t.Binary+" "+strings.Join(t.VersionArgs, " "), versionWait())
		return d
	case out.Err != nil:
		// Neither the wrapped error nor the child's stderr is quoted. The
		// error names the binary's absolute path — under /Users/<name> on a
		// Mac, which is the owner's name DEV-31 says never to send — and a
		// tool's own stderr is unbounded text nobody vetted. What a hub can
		// act on is that the tool does not work; what its owner needs is the
		// next action, and running it by hand tells them more than a tail.
		d.Error = t.Binary + " " + strings.Join(t.VersionArgs, " ") + " exited with an error — run it on this machine to see why"
		return d
	}
	// The same banner-to-one-line rule the harnesses need: these three disagree
	// about `--version` exactly as the CLIs do.
	d.Version = harness.ParseVersion(string(out.Stdout))

	if t.status != nil {
		t.status(ctx, path, &d)
	}
	return d
}

// run is one probe through the supervisor — its own process group, its own
// deadline, and whatever it left behind killed with it. Nothing here calls
// os/exec: a wrapper script that forks and hangs must not leave one orphan per
// probe interval.
func run(ctx context.Context, path string, args []string, mergeStderr bool, timeout time.Duration) (supervise.Capture, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// NoTTY because these are the tools that prompt: git and gh ask for a
	// passphrase or a login through /dev/tty whatever their environment says,
	// and a probe must fail at once rather than wait for a person who is not
	// there.
	return supervise.Run(ctx, supervise.Spec{Path: path, Args: args, NoTTY: true, MergeStderr: mergeStderr}, outputCap)
}

// wontRun and noAnswer are the two things that go wrong with a tool that is
// installed, said without quoting the tool: what a child printed and the path
// it printed it from are this machine's business, and the report is public.
func wontRun(binary string) string {
	return binary + " is installed but will not run — check on this machine that it is an executable file"
}

func noAnswer(what string, waited time.Duration) string {
	return fmt.Sprintf("no answer to `%s` within %s — the tool is installed but not usable until it answers", what, waited)
}
