package hostool

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ghLoginTTL is how long gh's answer about its login is reused before gh is
// asked again.
//
// Asking is a network call, not a local read: `gh auth status` validates the
// token against the host, which is how it knows whether an account works at
// all. At the daemon's probe interval that would be some 5,760 authenticated
// requests a day per runner — out of the same hourly budget the runs
// themselves spend — to answer a question that changes when somebody signs in
// or out. It would also make the answer depend on the link: a laptop off Wi-Fi
// would flip the login fields every 15 seconds, move the capability
// fingerprint and make every hub re-read the whole document. Five minutes is
// slow enough for both and fast enough that a fresh `gh auth login` is
// advertised while its owner is still watching. A variable so tests can
// shorten it.
var ghLoginTTL = 5 * time.Minute

// remembered is the last answer gh gave. A login is machine state that outlives
// one probe, so it is kept rather than re-derived every tick.
var remembered struct {
	sync.Mutex
	at    time.Time // when the answer was obtained; zero means never
	in    bool
	hosts []string
}

func recallGHLogin() (in bool, hosts []string, known, fresh bool) {
	remembered.Lock()
	defer remembered.Unlock()
	if remembered.at.IsZero() {
		return false, nil, false, false
	}
	return remembered.in, append([]string(nil), remembered.hosts...), true, time.Since(remembered.at) < ghLoginTTL
}

func rememberGHLogin(in bool, hosts []string) {
	remembered.Lock()
	defer remembered.Unlock()
	remembered.at, remembered.in, remembered.hosts = time.Now(), in, append([]string(nil), hosts...)
}

// forgetGHLogin drops the remembered answer. Tests call it so one test's gh is
// not answered from another's.
func forgetGHLogin() {
	remembered.Lock()
	defer remembered.Unlock()
	remembered.at, remembered.in, remembered.hosts = time.Time{}, false, nil
}

// ghStatus fills in whether gh is signed in and to which hosts.
//
// Two of the three things `gh auth status` prints must never leave this
// machine: the account name and the token, masked or not (CLAUDE.md). So
// nothing from its output is copied anywhere — not into Error, not into a log —
// and only the hosts and the boolean are read out of it. Being signed out is
// not an error; it is the answer to the question a hub asked.
func ghStatus(ctx context.Context, path string, d *Detected) {
	if in, hosts, known, fresh := recallGHLogin(); known && fresh {
		d.LoggedIn, d.LoginHosts = &in, hosts
		return
	}
	in, hosts, failure := askGH(ctx, path)
	if failure != "" {
		// A link that was down for one probe is not news that the login
		// changed. The last answer gh gave stands until gh itself replaces it,
		// which is also what keeps a flaky connection from moving the
		// capability fingerprint all day.
		if in, hosts, known, _ := recallGHLogin(); known {
			d.LoggedIn, d.LoginHosts = &in, hosts
			return
		}
		d.Error = failure
		return
	}
	rememberGHLogin(in, hosts)
	d.LoggedIn, d.LoginHosts = &in, hosts
}

// askGH runs the probe and reduces it to the two things a hub may see. The
// third return is what to report when the answer is not known; it never
// carries a word gh printed.
func askGH(ctx context.Context, path string) (in bool, hosts []string, failure string) {
	// `--json` is gh's machine-readable surface: it names its fields, so
	// nothing is picked out of prose a release may reword. `--active` gives one
	// entry per host — the account gh would use there — which is the question
	// here; the other accounts on a host differ only by a name we must not read.
	out, err := run(ctx, path, []string{"auth", "status", "--active", "--json", "hosts"}, false)
	switch {
	case err != nil:
		return false, nil, wontRun("gh")
	case out.TimedOut:
		return false, nil, noAnswer("gh auth status")
	}
	// The exit status is not consulted: with `--json` gh exits 0 whatever it
	// finds wrong with an account, and non-zero only on a fatal error — which
	// leaves nothing to decode, so the prose path below picks that up.
	if hosts, unsure, ok := ghFromJSON(out.Stdout); ok {
		if unsure {
			return false, nil, "gh could not reach the host it is signed in to, so whether it can open a pull request is unknown — it is asked again at the next probe"
		}
		return len(hosts) > 0, hosts, ""
	}

	// An older gh has no `--json` on auth status and answers only in prose, on
	// stderr when it is signed out. Reporting a working gh as broken would cost
	// its owner every run that needs a pull request, so the prose is read — for
	// the hosts and nothing else.
	out, err = run(ctx, path, []string{"auth", "status"}, true)
	switch {
	case err != nil:
		return false, nil, wontRun("gh")
	case out.TimedOut:
		return false, nil, noAnswer("gh auth status")
	}
	if hosts := ghFromProse(string(out.Stdout)); len(hosts) > 0 {
		return true, hosts, ""
	}
	if signedOut.MatchString(string(out.Stdout)) {
		return false, nil, ""
	}
	// gh said something, but nothing that answers the question. Calling that
	// "signed out" would route pull-request work away from a machine that may
	// well do it.
	return false, nil, "`gh auth status` answered nothing this runner could read — run it on this machine to see why"
}

// ghFromJSON reads the signed-in hosts out of `gh auth status --json hosts`,
// and deliberately nothing else: the account name is in that JSON and is not
// among the fields decoded here.
//
// unsure is gh knowing of a host and not being able to say whether it works.
func ghFromJSON(raw []byte) (hosts []string, unsure, ok bool) {
	var doc struct {
		Hosts map[string][]struct {
			State string `json:"state"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || doc.Hosts == nil {
		return nil, false, false
	}
	unusable := false
	for name, accounts := range doc.Hosts {
		signedIn := false
		for _, account := range accounts {
			// gh's states are success, timeout and error. Only the first is a
			// login this machine can use.
			if account.State == "success" {
				signedIn = true
			}
		}
		switch {
		case signedIn:
			hosts = append(hosts, name)
		case len(accounts) > 0:
			unusable = true
		}
	}
	// Sorted because a map has no order, and an answer that moved between
	// probes would move the capability fingerprint with it.
	sort.Strings(hosts)
	// A host that works answers the question on its own, whatever is wrong with
	// another. Only when none works does the difference matter: gh's error
	// state covers an expired token and a host it could not reach alike, and
	// telling those apart would mean reading the message beside it.
	return hosts, len(hosts) == 0 && unusable, true
}

// loggedInTo matches gh's one stable sentence. The wording around it has
// changed with gh's accounts rework — "as <user> (oauth_token)" became
// "account <user> (keyring)" — and the part after the host is the part that
// must not be read anyway.
var loggedInTo = regexp.MustCompile(`Logged in to (\S+)`)

// signedOut matches gh saying it has no login at all, which is an answer rather
// than a failure.
var signedOut = regexp.MustCompile(`not logged in`)

// ghFromProse reads the hosts out of what an older gh prints, sorted for the
// same reason ghFromJSON sorts.
func ghFromProse(raw string) []string {
	seen := map[string]bool{}
	var hosts []string
	for _, m := range loggedInTo.FindAllStringSubmatch(raw, -1) {
		host := strings.TrimSpace(m[1])
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}
