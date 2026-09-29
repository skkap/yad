//go:build unix

package workdir

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hostool"
	"github.com/skkap/yad/internal/supervise"
)

// noPrompt is the environment every git, and every setup hook, runs with. No
// one is at the runner to answer a prompt, and a prompt nobody answers holds
// the run until its timeout: git's own terminal prompt is off, the askpass
// helpers that would open a dialog are emptied (an empty GIT_ASKPASS also
// stops git falling back to core.askPass), and Git Credential Manager is told
// not to ask. With no controlling terminal as well (supervise.Spec.NoTTY),
// ssh cannot ask for a passphrase or a host key either.
var noPrompt = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GIT_ASKPASS=",
	"SSH_ASKPASS=",
	"SSH_ASKPASS_REQUIRE=never",
	"GCM_INTERACTIVE=never",
}

// gitEnv adds, for YAD's own git commands only, the transports a source may
// use — the same ones parseRemote allows, enforced by git too, so no
// redirect, submodule or helper reaches another. LC_ALL=C keeps the messages
// a run's error carries in one language.
var gitEnv = append([]string{"GIT_ALLOW_PROTOCOL=https:ssh:file", "LC_ALL=C"}, noPrompt...)

// gitOutputCap bounds what YAD reads of a git command's output; the commands
// here print a ref or a path.
const gitOutputCap = 1 << 20

// git runs one git command through the supervisor, under the owner's timeout,
// and returns its trimmed stdout. Every argument is YAD's own or has passed
// source.go's checks; none reaches a shell.
func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	return m.gitEnv(ctx, dir, nil, args...)
}

// gitEnv is git with env added after gitEnv: a source's credential, for the
// commands that talk to its remote and no others.
func (m *Manager) gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, m.GitTimeout)
	defer cancel()
	argv := args
	if dir != "" {
		argv = append([]string{"-C", dir}, args...)
	}
	bin := m.Git
	if bin == "" {
		var ok bool
		if bin, ok = hostool.Locate("git"); !ok {
			return "", errors.New("git could not be started: this runner has no git — install git on the runner's PATH, or point YAD_GIT_PATH at one")
		}
	}
	p, err := supervise.Start(ctx, supervise.Spec{Path: bin, Args: argv, Env: append(slices.Clip(gitEnv), env...), NoTTY: true})
	if err != nil {
		// Not the exec error: it names the binary by its path, which may be
		// under the owner's home, and this reaches the hub in the run's result.
		return "", errors.New("git could not be started — check that YAD_GIT_PATH, if the runner sets it, or else the runner's PATH names a git that runs")
	}
	// Bounded as supervise.Start asks: a descendant that left git's group
	// could otherwise hold stdout open past git's exit and its timeout.
	out, _ := readTail(p, gitOutputCap)
	werr := p.Wait()
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "", fmt.Errorf("git %s did not finish within %s and was stopped — raise [workdirs] git_timeout if the repository is large", args[0], m.GitTimeout)
	case ctx.Err() != nil:
		return "", ctx.Err()
	case werr != nil:
		return "", &gitError{verb: args[0], msg: lastLine(wholeLines(p.Stderr())), err: werr}
	}
	return strings.TrimSpace(out), nil
}

// gitError is a git command that ran and failed, with git's reason as git
// printed it. A reason that can quote a remote — a fetch, a set-head — is
// redacted where the source is known, by worktree's failed: redactSource
// first, then redactURLs, since the pattern cuts a URL where it stops and the
// source would no longer be found whole after it.
type gitError struct {
	verb, msg string
	err       error
}

func (e *gitError) Error() string {
	if e.msg == "" {
		return fmt.Sprintf("git %s: %v", e.verb, e.err)
	}
	return fmt.Sprintf("git %s: %s", e.verb, e.msg)
}

func (e *gitError) Unwrap() error { return e.err }

// urlInText is a URL in a line a program printed, up to the quote, space or
// bracket that ends it.
var urlInText = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s'"<>]+`)

// redactURLs is s with every URL in it passed through config.RedactURL. git
// quotes the remote it failed to reach, and a hub-sent URL may carry a token
// in its query, or as a user where parseRemote did not take it out — an ssh
// URL's, which ssh reads as the login name —
// which git's reason would otherwise carry to the hub in the run's error and
// to the daemon's log (decision 0064). Whether git anonymises the URL itself
// depends on its version and the message, so it is not relied on. It catches
// a URL other than the source too — a redirect's — which redactSource, which
// knows only the source, cannot.
func redactURLs(s string) string {
	return urlInText.ReplaceAllStringFunc(s, config.RedactURL)
}

// wholeLines is a stderr tail without its first line when the tail is full:
// supervise keeps the last StderrTail bytes, so that line may begin partway
// through a URL, past the scheme redactURLs finds it by, and quote the query
// behind it. A tail that is one cut line leaves nothing, and the error names
// git's exit status instead.
func wholeLines(tail string) string {
	if len(tail) < supervise.StderrTail {
		return tail
	}
	_, rest, _ := strings.Cut(tail, "\n")
	return rest
}

// lastLine is the last non-empty line of s: git's reason, after its progress
// and hints.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
