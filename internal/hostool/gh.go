package hostool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// ghStatus fills in whether gh is signed in and to which host.
//
// Two of the three things `gh auth status` prints must never leave this
// machine: the account name and the token, masked or not (CLAUDE.md). So
// nothing from its output is copied anywhere — not into Error, not into a log —
// and only the host and the boolean are read out of it. Being logged out is not
// an error; it is the answer to the question a hub asked.
func ghStatus(ctx context.Context, path string, d *Detected) {
	// `--json` is gh's machine-readable surface: it names the fields, so
	// nothing is picked out of prose that a release may reword, and it reports a
	// signed-out host as data rather than as a failure.
	out, err := run(ctx, path, []string{"auth", "status", "--active", "--json", "hosts"}, false)
	switch {
	case err != nil:
		d.Error = err.Error()
		return
	case out.TimedOut:
		d.Error = fmt.Sprintf("no answer to `gh auth status` within %s — gh is installed, but whether it can open a pull request is unknown until it answers", probeTimeout)
		return
	}
	// The exit status is not consulted: gh exits non-zero for an account with
	// any authentication issue, which is exactly the case this has to report.
	if host, in, ok := ghFromJSON(out.Stdout); ok {
		d.LoggedIn, d.LoginHost = &in, host
		return
	}

	// An older gh has no `--json` on auth status and answers only in prose, on
	// stderr when it is signed out. Reporting a working gh as broken would cost
	// its owner every run that needs a pull request, so the prose is read —
	// for the host and nothing else.
	out, err = run(ctx, path, []string{"auth", "status"}, true)
	switch {
	case err != nil:
		d.Error = err.Error()
		return
	case out.TimedOut:
		d.Error = fmt.Sprintf("no answer to `gh auth status` within %s — gh is installed, but whether it can open a pull request is unknown until it answers", probeTimeout)
		return
	}
	if host, in := ghFromProse(string(out.Stdout)); in || len(bytes.TrimSpace(out.Stdout)) > 0 {
		// gh answered — signed in to host, or signed out and saying so.
		d.LoggedIn, d.LoginHost = &in, host
		return
	}
	// It exited without a word, so nothing is known: saying "signed out" here
	// would route pull-request work away from a machine that may well do it.
	d.Error = "`gh auth status` answered nothing — run it on this machine to see why"
}

// ghFromJSON reads the host and the login boolean out of
// `gh auth status --json hosts`, and deliberately nothing else: the account
// name is in that JSON and is not among the fields decoded here.
func ghFromJSON(raw []byte) (host string, loggedIn, ok bool) {
	var doc struct {
		Hosts map[string][]struct {
			State  string `json:"state"`
			Active bool   `json:"active"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Hosts == nil {
		return "", false, false
	}
	// A host may hold several accounts and the map has no order, so the pick
	// has to be deterministic: the active account's host, else the
	// alphabetically first that works. An unstable answer here would move the
	// capability fingerprint on every probe and make a hub re-read the document
	// four times a minute.
	names := make([]string, 0, len(doc.Hosts))
	for name := range doc.Hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, acct := range doc.Hosts[name] {
			if acct.State != "success" {
				continue
			}
			if acct.Active {
				return name, true, true
			}
			if host == "" {
				host = name
			}
		}
	}
	return host, host != "", true
}

// loggedInTo matches gh's one stable sentence. The wording around it has
// changed with gh's accounts rework — "as <user> (oauth_token)" became
// "account <user> (keyring)" — and the part after the host is the part that
// must not be read anyway.
var loggedInTo = regexp.MustCompile(`Logged in to (\S+)`)

func ghFromProse(raw string) (host string, loggedIn bool) {
	// The first match in gh's own order: it prints the active account first
	// under each host.
	m := loggedInTo.FindStringSubmatch(raw)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}
