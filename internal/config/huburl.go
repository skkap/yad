package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
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
// quotes the URL it dialled with only the password starred, so a token held
// as the username reaches the terminal, the daemon's log and `yad status`
// unless every client that dials a hub passes its errors through here.
func RedactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		ue.URL = RedactURL(ue.URL)
	}
	return err
}

func loopback(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
