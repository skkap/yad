package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"strings"
	"time"
)

// CheckHubURL accepts a hub's base URL only where a bearer secret can travel
// safely: https anywhere, or plain http to this machine alone. Every request to
// a hub carries the registration token or the runner credential, and a
// cleartext hop to another host hands either to anyone on the path. Loopback
// stays allowed because `yad hub serve` binds there and prints an http URL.
//
// A refusal names the URL through RedactURL, never as given: it reaches
// stderr, and from there the kept output of whatever script ran `yad connect`.
func CheckHubURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		// url.Parse's own error quotes the URL, and so can its reason — an
		// invalid port is quoted from the text a password sits beside — so
		// neither is repeated.
		return errors.New("the hub URL does not parse as a URL — use an absolute https URL, like https://hub.example/yad/v1 (it is not repeated here, since a URL can carry a credential)")
	}
	if u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("hub URL %q must be an absolute https URL, like https://hub.example/yad/v1", RedactURL(raw))
	}
	// Refused, not ignored: every client sends its token as a bearer, and
	// net/http sends a URL's userinfo only when no Authorization header is
	// set, so a user or password here authenticates nothing. What it would do
	// is put a secret into config.toml, which holds none.
	if u.User != nil {
		return fmt.Errorf("hub URL %q carries a user name or password — drop the user:password@ part; yad authenticates to a hub with a token sent as a bearer (the runner credential `yad connect --token` exchanges for, or an admin token), never with the URL", RedactURL(raw))
	}
	if u.Scheme == "http" && !loopback(u.Hostname()) {
		return fmt.Errorf("hub URL %q is plain http to another host, which would send the runner credential in cleartext — use https (plain http is allowed only for localhost)", RedactURL(raw))
	}
	return nil
}

// UnprintableURL stands in for a URL RedactURL cannot take apart, so cannot
// print any part of safely.
const UnprintableURL = "[a URL that does not parse, not repeated in case it carries a credential]"

// RedactURL is a URL as it may be printed, logged or put in an error: a hub's
// connection URL, or one a hub sent. A URL is a place people are handed
// credentials, and the one printing it cannot tell a username from a token.
//
// It takes out the userinfo whole, not the password alone as url.URL.Redacted
// does — `https://<token>@host/` puts the credential in the username — and the
// query and fragment, where a signed URL keeps its signature. A URL that does
// not parse, or that still holds an @ once its userinfo is gone, is one whose
// credential could be anywhere in it, so none of it is printed. A URL with
// nothing to take out comes back exactly as given, so what is printed matches
// what the owner wrote.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return UnprintableURL
	}
	hadUser := u.User != nil
	u.User = nil
	changed := hadUser
	if u.RawQuery != "" || u.ForceQuery {
		u.RawQuery, u.ForceQuery, changed = "redacted", false, true
	}
	if u.Fragment != "" {
		u.Fragment, u.RawFragment, changed = "redacted", "", true
	}
	// Opaque ("runner:secret@host", read as scheme runner) and a credential
	// in the path of a URL with no host both parse, with no userinfo to find —
	// and either may spell its @ as %40, which String writes back as it was.
	// Path is already decoded; Opaque is not.
	if strings.Contains(u.String(), "@") || strings.Contains(u.Path, "@") ||
		strings.Contains(strings.ToLower(u.Opaque), "%40") {
		return UnprintableURL
	}
	if !changed {
		return raw
	}
	if hadUser {
		// A word in its place rather than nothing, so a reader can see the
		// URL carried a credential and that it was taken out.
		u.User = url.User("redacted")
	}
	return u.String()
}

// RedactURLError is err with the URL in a *url.Error redacted. That is the
// error net/http returns for a request that never got an answer, and it
// quotes the URL it last dialled with at most the password starred. With
// HubHTTPClient that is the hub URL CheckHubURL passed, never a Location, but
// a redirect refused in CheckRedirect would put the Location there as sent.
// Every client that dials a hub passes its errors through here before they
// reach the terminal, the daemon's log or `yad status`.
func RedactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = RedactURL(ue.URL)
	}
	return err
}

// HubHTTPClient is the HTTP client for anything that dials a hub: it never
// follows a redirect. Go keeps the Authorization header across a same-host
// redirect whatever the scheme, so an https hub answering 307 to http:// would
// be handed the bearer in cleartext after CheckHubURL passed; and no call to a
// hub redirects, so a redirect is a hub URL to correct, not a hop to take.
//
// The refusal happens in the transport, where the redirect first arrives, and
// not only in CheckRedirect: net/http parses the Location before it calls
// CheckRedirect, and when that parse fails it builds an error quoting the
// Location whole — userinfo and all — which no rewrite of the url.Error around
// it reaches. Refused here, net/http never parses the Location. A Location
// holding a byte HTTP forbids fails earlier still, in net/textproto, whose
// error quotes the header line; the transport replaces that error too.
func HubHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: refuseRedirects{http.DefaultTransport},
		// Unreachable while the transport refuses first; kept so that a
		// change to Transport can never turn a refusal into a hop that
		// carries the bearer.
		CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			return redirectRefusal(RedactURL(req.URL.String()))
		},
	}
}

type refuseRedirects struct{ next http.RoundTripper }

func (t refuseRedirects) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	var malformed textproto.ProtocolError
	if errors.As(err, &malformed) {
		// net/textproto quotes the offending line whole, and a header line
		// the hub wrote — a Location with a byte it refuses — can carry a
		// credential. Matched by type: every ProtocolError quotes the hub.
		return nil, errors.New("the hub's answer is not valid HTTP — what it sent is not repeated, since a header such as a redirect's Location can carry a credential")
	}
	if err != nil {
		return resp, err
	}
	// Every 3xx carrying a Location, not only the five codes net/http follows
	// today: a hub has no use for any of them, and the wider test cannot miss
	// one a later Go adds.
	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode > 399 || loc == "" {
		return resp, nil
	}
	// A small body is read so the connection stays reusable, by net/http's
	// own rule before following a redirect; a large one is not waited on.
	const maxBodySlurpSize = 2 << 10
	if resp.ContentLength == -1 || resp.ContentLength <= maxBodySlurpSize {
		_, _ = io.CopyN(io.Discard, resp.Body, maxBodySlurpSize)
	}
	resp.Body.Close()
	// Resolved against the request, so a relative Location still says where;
	// one that does not parse is not repeated, not even in part, and neither
	// is url.Parse's error, which quotes it.
	shown := UnprintableURL
	if u, err := req.URL.Parse(loc); err == nil {
		shown = RedactURL(u.String())
	}
	return nil, redirectRefusal(shown)
}

// redirectRefusal takes a target already redacted.
func redirectRefusal(shown string) error {
	return fmt.Errorf("the hub redirected to %s — a hub must not redirect; use the hub's final address as its URL", shown)
}

func loopback(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
