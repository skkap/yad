package hostool

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/skkap/yad/internal/probe"
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

// ghLoginStale is how long an answer may go on standing while gh refuses to
// give a new one.
//
// The memory exists to absorb a transient — a closed lid, a VPN reconnecting,
// a GitHub incident — so that one bad probe does not move the fingerprint for
// every hub. It must not outlive the thing it remembers: gh reports a revoked
// token and a host it could not reach with the same state, and the two are
// told apart only by a message this package will not read, so time is the only
// honest bound. Past it the runner stops claiming a login nothing has been able
// to confirm for half an hour, and says so instead.
var ghLoginStale = 30 * time.Minute

// ghMemory is what the last probe of gh left behind.
//
// Two times, not one: when gh was last *asked* decides whether to ask again,
// and when it last *answered* decides how long that answer may stand. Keeping
// only the second would re-ask a gh that is failing on every probe, which is
// the cost the memory exists to avoid.
type ghMemory struct {
	asked    time.Time // when gh was last asked, answered or not
	answered time.Time // when it last gave an answer; zero means never
	in       bool
	hosts    []string
	failure  string // why the last ask produced nothing, for when no answer may stand
}

var remembered struct {
	sync.Mutex
	ghMemory
}

func recallGH() ghMemory {
	remembered.Lock()
	defer remembered.Unlock()
	return remembered.copy()
}

func rememberGH(in bool, hosts []string, failure string) ghMemory {
	remembered.Lock()
	defer remembered.Unlock()
	now := time.Now()
	remembered.asked, remembered.failure = now, failure
	if failure == "" {
		remembered.answered, remembered.in, remembered.hosts = now, in, append([]string(nil), hosts...)
	}
	return remembered.copy()
}

func (m ghMemory) copy() ghMemory {
	m.hosts = append([]string(nil), m.hosts...)
	return m
}

// serve puts an answer gh gave into the report.
func (m ghMemory) serve(d *Detected) { d.LoggedIn, d.LoginHosts = &m.in, m.hosts }

// report puts the best thing known into the report: the answer when there is
// one, an older answer while the failure can still be called a transient, and
// otherwise why nothing is known.
func (m ghMemory) report(d *Detected) {
	switch {
	case m.failure == "":
		m.serve(d)
	case !m.answered.IsZero() && time.Since(m.answered) < ghLoginStale:
		// A link that was down for one probe is not news that the login
		// changed, so the last answer stands — but only while it can still be
		// called a transient. A token revoked this morning must not be
		// advertised all afternoon because nothing has managed to check it
		// since.
		m.serve(d)
	default:
		d.Error = m.failure
	}
}

// ghStatus fills in whether gh is signed in and to which hosts.
//
// Two of the three things `gh auth status` prints must never leave this
// machine: the account name and the token, masked or not (AGENTS.md). So
// nothing from its output is copied anywhere — not into Error, not into a log —
// and only the hosts and the boolean are read out of it. Being signed out is
// not an error; it is the answer to the question a hub asked.
func ghStatus(ctx context.Context, bin probe.Found, d *Detected) {
	m := recallGH()
	switch {
	case !m.answered.IsZero() && time.Since(m.answered) < ghLoginTTL:
		// An answer this fresh is served without asking at all.
		m.serve(d)
		return
	case !m.asked.IsZero() && time.Since(m.asked) < ghLoginTTL:
		// gh was asked within the same interval and could not answer. Asking
		// again now spends another network call on a question it has just
		// declined, and changes nothing in the document either way.
		m.report(d)
		return
	}
	in, hosts, failure := askGH(ctx, bin)
	rememberGH(in, hosts, failure).report(d)
}

// askGH runs the probe and reduces it to the two things a hub may see. The
// third return is what to report when the answer is not known; it never
// carries a word gh printed.
func askGH(ctx context.Context, bin probe.Found) (in bool, hosts []string, failure string) {
	// `--json` is gh's machine-readable surface: it names its fields, so
	// nothing is picked out of prose a release may reword. `--active` gives one
	// entry per host — the account gh would use there — which is the question
	// here; the other accounts on a host differ only by a name we must not read.
	out, err := run(ctx, bin.Path, []string{"auth", "status", "--active", "--json", "hosts"}, false, statusWait())
	switch {
	case err != nil:
		return false, nil, bin.WontStart()
	case out.TimedOut:
		return false, nil, bin.NoAnswer(statusWait(), "auth", "status")
	}
	// The exit status is not consulted: with `--json` gh exits 0 whatever it
	// finds wrong with an account, and non-zero only on a fatal error — which
	// leaves nothing to decode, so the prose path below picks that up.
	if hosts, unsure, ok := ghFromJSON(out.Stdout); ok {
		if unsure {
			// Which of the two it is, this package cannot say: gh gives a
			// revoked token and an unreachable host the same state, and
			// telling them apart means reading the message beside it. So the
			// report says what is known and names the one command that
			// distinguishes them.
			return false, nil, "gh has a login it could not confirm — the token may be expired or the host unreachable; " + bin.Try(bin.Command("auth", "status"), "which")
		}
		return len(hosts) > 0, hosts, ""
	}

	// An older gh has no `--json` on auth status and answers only in prose, on
	// stderr when it is signed out. Reporting a working gh as broken would cost
	// its owner every run that needs a pull request, so the prose is read — for
	// the hosts and nothing else.
	out, err = run(ctx, bin.Path, []string{"auth", "status"}, true, statusWait())
	switch {
	case err != nil:
		return false, nil, bin.WontStart()
	case out.TimedOut:
		return false, nil, bin.NoAnswer(statusWait(), "auth", "status")
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
	return false, nil, "`" + bin.Command("auth", "status") + "` answered nothing this runner could read — " + bin.TryIt("why")
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
