package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	Error   string `json:"error,omitempty"`
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

func detectOne(ctx context.Context, h Harness) Detected {
	d := Detected{Harness: h}

	path := os.Getenv(h.EnvPath)
	if path == "" {
		p, err := exec.LookPath(h.Binary)
		if err != nil {
			return d // absent, and that is not an error
		}
		path = p
	}
	d.Path, d.Present = path, true

	// Through supervise like every other child: the probe runs every sync, and a
	// wrapper script or node/bun launcher that forks and hangs must take its
	// whole process group with it, not leave one orphan per tick.
	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	p, err := supervise.Start(ctx, supervise.Spec{Path: path, Args: h.VersionArgs})
	if err != nil {
		d.Error = err.Error()
		return d
	}
	raw, _ := io.ReadAll(io.LimitReader(p.Stdout(), versionOutputCap))
	// Past the cap the child may still be writing; closing our end makes it
	// fail with EPIPE rather than block, and the deadline covers the rest.
	p.Stdout().Close()
	werr := p.Wait()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		d.Error = fmt.Sprintf("no answer to %s within %s", strings.Join(h.VersionArgs, " "), versionTimeout)
	case werr != nil:
		d.Error = strings.TrimSpace(werr.Error() + " " + p.Stderr())
	default:
		d.Version = ParseVersion(string(raw))
	}
	return d
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
