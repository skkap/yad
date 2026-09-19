package harness

import (
	"context"
	"fmt"
	"os"
	"os/exec"
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
	// never carries a word the harness printed, nor the path it was started
	// from: the capability document reaches every connected hub, and a child's
	// stderr is unbounded text nobody vetted — a proxy URL with a password in
	// it, a loader error naming the owner's home (DEV-60).
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
// failure is reported as text. A variable so tests can shorten it.
var versionTimeout = 5 * time.Second

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
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	out, err := supervise.Run(ctx, supervise.Spec{Path: path, Args: h.VersionArgs}, versionOutputCap)
	switch {
	case err != nil:
		d.Error = wontStart(h, fromEnv)
	case out.TimedOut:
		d.Error = fmt.Sprintf("no answer to %s within %s", strings.Join(h.VersionArgs, " "), versionTimeout)
	case out.Err != nil:
		d.Error = wontAnswer(h)
	default:
		d.Version = ParseVersion(string(out.Stdout))
	}
	return d
}

// wontStart and wontAnswer are the two things that go wrong with a harness that
// is installed, said without quoting it. The wrapped exec error names the
// binary's absolute path — under /Users/<name> on a Mac, which is the owner's
// name — and a harness's own stderr is unbounded text nobody vetted: a dyld
// failure listing libraries under that same home, a proxy URL with a password
// in it. Neither travels. What a hub can act on is that the harness does not
// work; what its owner needs is where to look, and an override's *name* is safe
// where its value is the thing that leaks.
func wontStart(h Harness, fromEnv bool) string {
	where := "the " + h.Binary + " that PATH resolves to"
	if fromEnv {
		where = h.EnvPath
	}
	return fmt.Sprintf("%s is installed but will not run — check that %s names an executable file", h.Binary, where)
}

func wontAnswer(h Harness) string {
	return fmt.Sprintf("`%s %s` exited with an error — run it on this machine to see why", h.Binary, strings.Join(h.VersionArgs, " "))
}

// ParseVersion reduces a CLI's version banner to one line.
//
// The CLIs disagree about what `--version` prints: a bare "2.4.1", "claude 2.4.1
// (Claude Code)", or a banner with an update notice underneath. The first
// non-empty line is the only thing they share, and what a human wants in a table.
func ParseVersion(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}
