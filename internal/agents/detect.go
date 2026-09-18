package agents

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Detected is one catalog entry as it actually exists on this machine.
type Detected struct {
	Agent
	Path    string `json:"path,omitempty"`
	Version string `json:"version,omitempty"`
	Present bool   `json:"present"`
	Error   string `json:"error,omitempty"`
}

// versionTimeout caps a single `<agent> --version`. An agent CLI that hangs on
// its own version flag is broken, and a runner that waits on it never registers
// — so the detection is bounded and the failure is reported as text.
const versionTimeout = 5 * time.Second

// Detect probes every catalog entry concurrently and returns them in catalog
// order. It never returns an error: a missing or broken agent is a *fact about
// the machine* that belongs in the capability report, not a reason to fail.
func Detect(ctx context.Context) []Detected {
	cat := Catalog()
	out := make([]Detected, len(cat))
	var wg sync.WaitGroup
	for i, a := range cat {
		wg.Add(1)
		go func(i int, a Agent) {
			defer wg.Done()
			out[i] = detectOne(ctx, a)
		}(i, a)
	}
	wg.Wait()
	return out
}

func detectOne(ctx context.Context, a Agent) Detected {
	d := Detected{Agent: a}

	path := os.Getenv(a.EnvPath)
	if path == "" {
		p, err := exec.LookPath(a.Binary)
		if err != nil {
			return d // absent, and that is not an error
		}
		path = p
	}
	d.Path, d.Present = path, true

	ctx, cancel := context.WithTimeout(ctx, versionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, a.VersionArgs...)
	// A version probe must not inherit a half-configured environment's stdin,
	// or a CLI that decides to prompt will block until the timeout.
	cmd.Stdin = nil
	raw, err := cmd.Output()
	if err != nil {
		d.Error = strings.TrimSpace(err.Error())
		return d
	}
	d.Version = ParseVersion(string(raw))
	return d
}

// ParseVersion reduces a CLI's version banner to one line.
//
// The CLIs disagree about what `--version` prints: some emit a bare "2.4.1",
// some "claude 2.4.1 (Claude Code)", some a multi-line banner with an update
// notice underneath. The first non-empty line is the only thing they have in
// common, and it is what a human wants to read in a table.
func ParseVersion(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		if s := strings.TrimSpace(line); s != "" {
			return s
		}
	}
	return ""
}
