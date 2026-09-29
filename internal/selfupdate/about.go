package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/skkap/yad/internal/buildinfo"
)

// legacyProtocols is what a release older than `yad version --json` speaks.
// Every release before the field existed was built against protocol/v1 alone,
// and v1 is the only major there has ever been, so reading such a release as
// v1-only is a fact about the past rather than a guess (decision 0071). The
// literal, not v1.Version: this names what old binaries spoke, which no later
// build's constant can change.
var legacyProtocols = []string{"1"}

// askTimeout bounds a downloaded binary's `yad version --json`. It prints two
// fields; the time is for a machine busy enough, or a first exec scanned
// slowly enough, to start a 20 MB binary late.
const askTimeout = 30 * time.Second

// maxAbout bounds what is read of the answer: an About is a few dozen bytes.
const maxAbout = 64 << 10

// Ask runs a downloaded yad's `version --json` and reads what it says. The
// binary gets HOME and nothing else of this process's environment: it is
// about to be trusted with all of it, but not before it has been asked, and
// the variables a daemon may carry — a harness's credential among them — are
// no business of a `yad version`. HOME is there because every yad command
// resolves its profile's directories before it dispatches.
func Ask(ctx context.Context, path string) (buildinfo.About, error) {
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "version", "--json")
	cmd.Env = []string{"HOME=" + os.Getenv("HOME")}
	cmd.Dir = os.TempDir()
	var out, errOut limited
	out.max, errOut.max = maxAbout, maxAbout
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return buildinfo.About{}, fmt.Errorf("`version --json` did not answer in %s", askTimeout)
		}
		return buildinfo.About{}, fmt.Errorf("`version --json` failed (%v): %s", err, strings.TrimSpace(errOut.String()))
	}
	return ParseAbout(out.Bytes())
}

// ParseAbout reads what `yad version --json` printed. A release older than the
// flag ignores it and prints its one line — "yad v0.4.0 (abc1234)" — which is
// read for its version and as speaking protocol v1 alone (legacyProtocols).
// So is JSON without the protocols field, which no release writes, for the
// same reason.
func ParseAbout(out []byte) (buildinfo.About, error) {
	s := bytes.TrimSpace(out)
	if bytes.HasPrefix(s, []byte("{")) {
		var a buildinfo.About
		if err := json.Unmarshal(s, &a); err != nil {
			return buildinfo.About{}, fmt.Errorf("`version --json` printed something that is not yad's JSON: %w", err)
		}
		if a.Version == "" {
			return buildinfo.About{}, errors.New("`version --json` named no version")
		}
		if a.Protocols == nil {
			a.Protocols = legacyProtocols
		}
		return a, nil
	}
	line, _, _ := strings.Cut(string(s), "\n")
	f := strings.Fields(line)
	if len(f) < 2 || f[0] != "yad" {
		return buildinfo.About{}, fmt.Errorf("`version --json` printed %q, which is not what any yad version prints", truncate(line, 200))
	}
	return buildinfo.About{Version: f[1], Commit: strings.Trim(strings.Join(f[2:], " "), "()"), Protocols: legacyProtocols}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// On a rune boundary, so what the owner reads is never half a character.
	cut := 0
	for i := range s {
		if i > n {
			break
		}
		cut = i
	}
	return s[:cut] + "…"
}

// limited keeps the first max bytes written to it and drops the rest, so a
// binary that prints without end neither fills memory nor blocks on a pipe.
type limited struct {
	bytes.Buffer
	max int
}

func (l *limited) Write(p []byte) (int, error) {
	if room := l.max - l.Len(); room > 0 {
		l.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
