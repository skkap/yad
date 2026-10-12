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
	"time"
)

// DefaultBaseURL is where GitHub serves release pages and assets. Both are
// plain HTTPS downloads for a public repository, so an upgrade needs no login,
// no `gh` and no token of yad's own to hold, log or leak.
const DefaultBaseURL = "https://github.com"

// DefaultStall is how long a request to the release server may go without a
// byte arriving — before the answer's headers, or between two pieces of a
// download — before it is given up. github.com answers a release page in well
// under a second; a minute leaves room for a slow proxy in front of it, and
// still ends a `yad upgrade` at a terminal, or a runner's self-update, that
// would otherwise wait for good on a server that took the request and never
// answered (YAD-181). A download that keeps arriving, however slowly, is
// never cut short by it.
const DefaultStall = time.Minute

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
	// Stall is DefaultStall unless set; tests shorten it.
	Stall time.Duration
}

func (g GitHub) stall() time.Duration {
	if g.Stall <= 0 {
		return DefaultStall
	}
	return g.Stall
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

// errStalled is the cause a request is cancelled with once the server has
// sent nothing for the stall: told apart from the caller's own cancellation,
// it is the one that gets a sentence of its own.
var errStalled = errors.New("the release server stalled")

// maxRedirects is net/http's own limit, kept where do replaces the policy
// that carries it.
const maxRedirects = 10

// do sends one request and gives it up once the server has sent nothing for
// the stall. The clock starts with the request and starts again with each
// redirect, the headers, and every piece of the body read after them, so a slow
// download carries on and only one that has stopped is ended. http.Client's
// Timeout would bound the whole download instead, and a transport's
// ResponseHeaderTimeout ends a request with an error that cannot be told
// apart from a connection that timed out.
func (g GitHub) do(ctx context.Context, client *http.Client, method, u string) (*http.Response, error) {
	stall := g.stall()
	reqCtx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(stall, func() { cancel(errStalled) })
	req, err := http.NewRequestWithContext(reqCtx, method, u, nil)
	if err != nil {
		timer.Stop()
		cancel(nil)
		return nil, err
	}
	// A redirect is an answer: every release asset is one, to another host,
	// which gets the stall afresh rather than what the first one left of it.
	follow := *client
	follow.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		timer.Reset(stall)
		if client.CheckRedirect != nil {
			return client.CheckRedirect(r, via)
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		return nil
	}
	resp, err := follow.Do(req)
	if err != nil {
		timer.Stop()
		stalled := context.Cause(reqCtx) == errStalled
		cancel(nil)
		switch {
		case stalled:
			return nil, fmt.Errorf("%s took the request and sent nothing back in %s — check this machine's network, or its HTTPS_PROXY, and try again", u, stall)
		case ctx.Err() != nil:
			// The caller's own end — a Ctrl-C, or a self-update check
			// that ran out of time — which the caller says in its own words.
			return nil, err
		}
		return nil, fmt.Errorf("could not reach %s: %w — check this machine's network, or its HTTPS_PROXY", u, err)
	}
	timer.Reset(stall)
	resp.Body = &stallingBody{ReadCloser: resp.Body, ctx: reqCtx, cancel: cancel, timer: timer, stall: stall}
	return resp, nil
}

// stallingBody is a response body whose server has the stall to send each
// next piece of it, and which releases the request's timer when closed.
type stallingBody struct {
	io.ReadCloser
	ctx    context.Context
	cancel context.CancelCauseFunc
	timer  *time.Timer
	stall  time.Duration
}

func (b *stallingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(b.stall)
	}
	if err != nil && err != io.EOF && context.Cause(b.ctx) == errStalled {
		err = fmt.Errorf("nothing arrived for %s — check this machine's network, or its HTTPS_PROXY, and try again", b.stall)
	}
	return n, err
}

func (b *stallingBody) Close() error {
	b.timer.Stop()
	b.cancel(nil)
	return b.ReadCloser.Close()
}
