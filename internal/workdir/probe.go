//go:build unix

package workdir

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hostool"
	"github.com/skkap/yad/internal/supervise"
)

// ProbeTimeout bounds one probe. A reachable remote answers ls-remote in a
// second or two; one that has not in twenty is one a run's fetch would be
// stuck on too, and the hub is better told so than kept waiting.
const ProbeTimeout = 20 * time.Second

// probeOutputCap bounds what is read of ls-remote's output, which is read
// only to be thrown away: a ref pattern can match many tags.
const probeOutputCap = 64 << 10

// Probe is whether this runner can reach the repository at rawURL and find
// ref there (HEAD when empty), checked as a run's git would fetch it: the
// same git, the same environment — the owner's [env] layer, its credential
// helpers, its ssh agent — and the same checks on the URL (decision 0088).
//
// It answers with a class and never with git's words, which can quote a URL
// carrying a credential an insteadOf rule of the owner's put there. A URL
// carrying a credential of its own — or a query or a fragment, where one may
// sit (queried) — is refused unprobed: a hub sends that
// credential with the run that needs it, so a probe would only say whether
// the hub's token works, and the runner would hold it for nothing.
func (m *Manager) Probe(ctx context.Context, rawURL, ref string) (bool, v1.SourceReachError) {
	m.init()
	r, err := parseRemote(rawURL, m.reach)
	if err != nil || r.cred != nil || !probeRef(ref) || queried(rawURL) {
		return false, v1.ReachRefused
	}
	if ref == "" {
		ref = "HEAD"
	}
	timeout := m.ProbeTimeout
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	bin := m.Git
	if bin == "" {
		var ok bool
		if bin, ok = hostool.Locate("git"); !ok {
			return false, v1.ReachFailed
		}
	}
	p, err := supervise.Start(ctx, supervise.Spec{Path: bin, Args: []string{"ls-remote", "--exit-code", "--", r.url, ref},
		Env: slices.Clip(gitEnv), NoTTY: true})
	if err != nil {
		return false, v1.ReachFailed
	}
	readTail(p, probeOutputCap)
	werr := p.Wait()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return false, v1.ReachTimeout
	case ctx.Err() != nil:
		return false, v1.ReachFailed
	case werr == nil:
		return true, ""
	case exitCode(werr) == 2:
		// --exit-code's own answer: the remote answered, with no ref
		// matching.
		return false, v1.ReachRefNotFound
	}
	return false, reachClass(p.Stderr())
}

// queried is a URL with a query or a fragment: a place config.RedactURL
// treats as holding a token, and that no plain repository URL needs, so a
// probe refuses it as it refuses userinfo.
func queried(raw string) bool {
	_, rest, ok := strings.Cut(raw, "://")
	return ok && strings.ContainsAny(rest, "?#")
}

// probeRef is a ref a hub may ask about: handed to git as a pattern after
// "--", so only a word git could read as something else is refused.
func probeRef(ref string) bool {
	return len(ref) <= 255 && !strings.HasPrefix(ref, "-") &&
		strings.IndexFunc(ref, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

// reachClasses are git's, curl's and ssh's words under LC_ALL=C for each way a
// remote cannot be reached, in the order they are tried: a 404 that
// says so beats the generic "Could not read from remote repository" ssh
// prints after every failure, and an auth refusal is looked for before
// "not found", because GitLab and Bitbucket both say "not found" after a
// refused credential in some versions.
var reachClasses = []struct {
	class v1.SourceReachError
	says  []string
}{
	{v1.ReachAuth, append(slices.Clone(authRefusals),
		"Permission denied", "Host key verification failed", "Invalid username or password",
		"Invalid username or token", "HTTP Basic: Access denied", "access denied", "Access denied")},
	{v1.ReachNotFound, []string{"Repository not found", "repository not found", "returned error: 404",
		"does not appear to be a git repository", "does not exist", "not found"}},
	{v1.ReachNetwork, []string{"Could not resolve host", "Could not resolve hostname", "Connection refused",
		"Connection timed out", "Operation timed out", "Failed to connect", "Network is unreachable",
		"No route to host", "Connection reset", "connection closed", "Connection closed",
		"SSL", "TLS", "certificate", "early EOF", "the remote end hung up unexpectedly"}},
}

// reachClass is the class of a failed ls-remote's stderr, or failed when
// none of the words is there.
func reachClass(stderr string) v1.SourceReachError {
	for _, c := range reachClasses {
		for _, s := range c.says {
			if strings.Contains(stderr, s) {
				return c.class
			}
		}
	}
	return v1.ReachFailed
}
