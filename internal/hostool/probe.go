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
	// Error is what went wrong with a tool that is installed: a probe that
	// timed out, a binary that would not run, a Docker daemon that does not
	// answer. It is a fact about the machine, never a failure of detection, and
	// it carries the next action wherever there is one.
	Error string `json:"error,omitempty"`
	// LoggedIn is nil for a tool with no notion of a login, and nil too when
	// the tool has one and the probe could not find out — Error says why.
	LoggedIn *bool `json:"logged_in,omitempty"`
	// LoginHost is the host the tool is logged in to: github.com, or a GitHub
	// Enterprise hostname. Who it is logged in *as* is never read, let alone
	// reported — the capability document is a public surface.
	LoginHost string `json:"login_host,omitempty"`
}

// probeTimeout caps one probe. Every one of them waits on something that can
// fail to answer rather than fail: a `--version` that hangs (it happened to
// Claude through a bun regression and stalled every registration in Multica),
// `gh auth status` validating a token against the host over the network, a
// `docker version` waiting on a daemon socket nothing is listening on. Five
// seconds is enough for all three on a working machine, and the probe runs
// again at the next interval. A variable so tests can shorten it.
var probeTimeout = 5 * time.Second

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

// Locate finds the binary for a host tool the way detection does, so whatever
// uses it later is the executable the capability document advertised.
func Locate(id string) (string, bool) {
	t, ok := Lookup(id)
	if !ok {
		return "", false
	}
	return locate(t)
}

func locate(t Tool) (string, bool) {
	if path := os.Getenv(t.EnvPath); path != "" {
		return path, true
	}
	path, err := exec.LookPath(t.Binary)
	return path, err == nil
}

func probeOne(ctx context.Context, t Tool) Detected {
	d := Detected{ID: t.ID}

	path, ok := locate(t)
	if !ok {
		return d // absent, and that is not an error
	}
	d.Path, d.Present = path, true

	out, err := run(ctx, path, t.VersionArgs, false)
	switch {
	case err != nil:
		d.Error = err.Error()
		return d
	case out.TimedOut:
		d.Error = fmt.Sprintf("no answer to %s %s within %s — the tool is installed but not usable until it answers", t.Binary, strings.Join(t.VersionArgs, " "), probeTimeout)
		return d
	case out.Err != nil:
		d.Error = strings.TrimSpace(out.Err.Error() + " " + out.Stderr)
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
func run(ctx context.Context, path string, args []string, mergeStderr bool) (supervise.Capture, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// NoTTY because these are the tools that prompt: git and gh ask for a
	// passphrase or a login through /dev/tty whatever their environment says,
	// and a probe must fail at once rather than wait for a person who is not
	// there.
	return supervise.Run(ctx, supervise.Spec{Path: path, Args: args, NoTTY: true, MergeStderr: mergeStderr}, outputCap)
}
