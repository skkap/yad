package upgrade

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// GH fetches releases with the GitHub CLI. The repository is private, so the
// owner's existing `gh auth login` is the whole of the authorisation story:
// there is no URL here to curl without a token, and no token of yad's own to
// hold, log or leak.
type GH struct {
	// Repo is owner/name; empty means DefaultRepo.
	Repo string
}

func (g GH) repo() string {
	if g.Repo == "" {
		return DefaultRepo
	}
	return g.Repo
}

func (g GH) Latest(ctx context.Context) (string, error) {
	// `gh release view` with no tag is the newest release, and --jq keeps the
	// answer a bare tag rather than a JSON document to parse.
	out, err := g.run(ctx, "release", "view", "--repo", g.repo(), "--json", "tagName", "--jq", ".tagName")
	if err != nil {
		// A repository with no releases at all fails this call the same way a
		// missing tag does, and sending that operator to `gh auth status`
		// wastes their time on a login that is working.
		var ge *ghError
		if errors.As(err, &ge) && strings.Contains(ge.stderr, "release not found") {
			return "", g.noReleases()
		}
		return "", err
	}
	if tag := strings.TrimSpace(out); tag != "" {
		return tag, nil
	}
	return "", g.noReleases()
}

func (g GH) noReleases() error {
	return fmt.Errorf("%s has published no release yet — install yad from this checkout with `make install` until it has", g.repo())
}

func (g GH) Download(ctx context.Context, tag string, assets []string, dir string) error {
	args := []string{"release", "download", tag, "--repo", g.repo(), "--dir", dir}
	for _, a := range assets {
		args = append(args, "--pattern", a)
	}
	_, err := g.run(ctx, args...)
	return err
}

// ghError carries gh's own stderr, which is where it says "not logged in",
// "release not found" or "could not resolve to a Repository" — the next action
// is nearly always in there already, and the caller may want to read it.
type ghError struct {
	repo   string
	stderr string
	err    error
}

func (e *ghError) Error() string {
	return fmt.Sprintf("gh could not read %s: %s — the repository is private, so `gh auth status` is the first thing to check", e.repo, e.stderr)
}

func (e *ghError) Unwrap() error { return e.err }

func (g GH) run(ctx context.Context, args ...string) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", fmt.Errorf("the GitHub CLI `gh` is not on PATH, and yad's releases are in a private repository — install gh (https://cli.github.com) and run `gh auth login`")
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return "", &ghError{repo: g.repo(), stderr: msg, err: err}
		}
		return "", fmt.Errorf("could not run gh: %s", msg)
	}
	return stdout.String(), nil
}
