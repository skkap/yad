package upgrade

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DefaultBaseURL is where GitHub serves release pages and assets. Both are
// plain HTTPS downloads for a public repository, so an upgrade needs no login,
// no `gh` and no token of yad's own to hold, log or leak.
const DefaultBaseURL = "https://github.com"

// GitHub fetches releases the way a browser does. It reads the newest tag from
// the redirect on /releases/latest rather than from api.github.com, whose
// unauthenticated limit of 60 requests an hour is shared by every machine
// behind one address — a fleet upgraded together would exhaust it.
type GitHub struct {
	// Repo is owner/name; empty means DefaultRepo.
	Repo string
	// BaseURL is DefaultBaseURL except in tests, which serve releases in
	// process because no test here touches the network (ARCHITECTURE.md §7).
	BaseURL string
	// Yad builds the yad commands its errors offer, carrying the caller's
	// profile; nil builds bare ones.
	Yad func(args ...string) string
}

func (g GitHub) repo() string {
	if g.Repo == "" {
		return DefaultRepo
	}
	return g.Repo
}

func (g GitHub) url(parts ...string) string {
	base := g.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	return strings.TrimSuffix(base, "/") + "/" + g.repo() + "/releases/" + strings.Join(parts, "/")
}

func (g GitHub) Latest(ctx context.Context) (string, error) {
	u := g.url("latest")
	// The redirect is the answer, so it is read rather than followed.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := g.do(ctx, client, http.MethodGet, u)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", g.notFound()
	case resp.StatusCode < 300 || resp.StatusCode > 399:
		return "", fmt.Errorf("%s answered %s", u, resp.Status)
	}
	// A repository with releases redirects to /releases/tag/<tag>; one with
	// none redirects to /releases, which is not a failure to report as one.
	loc, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("%s redirected nowhere readable: %w", u, err)
	}
	_, tag, ok := strings.Cut(loc.Path, "/releases/tag/")
	if !ok || tag == "" {
		return "", g.noReleases()
	}
	return tag, nil
}

func (g GitHub) noReleases() error {
	return fmt.Errorf("%s has published no release yet — install yad from a checkout with `make install` until it has", g.repo())
}

// notFound is also what GitHub answers for a private repository, which is the
// case a fork's owner is most likely to be in.
func (g GitHub) notFound() error {
	return fmt.Errorf("github.com/%s does not exist or is private — releases are downloaded without a login, so %s has to name a public repository", g.repo(), RepoEnv)
}

// Download places each named asset of one release into dir. An asset the
// release does not carry is left absent rather than failed on, so Apply can
// say which one is missing; a tag that does not exist fails here, because
// every asset of it being absent would otherwise read as an incomplete release.
func (g GitHub) Download(ctx context.Context, tag string, assets []string, dir string) error {
	tagPath := url.PathEscape(tag)
	resp, err := g.do(ctx, http.DefaultClient, http.MethodHead, g.url("tag", tagPath))
	if err != nil {
		return err
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s has no release %s — `%s` says what the newest one is", g.repo(), tag, Command(g.Repo, g.Yad, "--check"))
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%s answered %s", g.url("tag", tagPath), resp.Status)
	}
	for _, a := range assets {
		if err := g.fetch(ctx, g.url("download", tagPath, url.PathEscape(a)), filepath.Join(dir, a)); err != nil {
			return err
		}
	}
	return nil
}

func (g GitHub) fetch(ctx context.Context, u, dst string) error {
	resp, err := g.do(ctx, http.DefaultClient, http.MethodGet, u)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", u, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return fmt.Errorf("download of %s was cut short: %w", u, err)
	}
	return f.Close()
}

func (g GitHub) do(ctx context.Context, client *http.Client, method, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		return nil, fmt.Errorf("could not reach %s: %w — check this machine's network, or its HTTPS_PROXY", u, err)
	}
	return resp, nil
}
