package runner

import (
	"time"
	"unicode/utf8"

	"github.com/skkap/yad/internal/logfile"
)

// The bounds on health's recent_errors. Health rides every sync, so a block
// that grows with the daemon's uptime makes every sync slower for ever.
const (
	// maxHealthErrors is how many distinct problems a hub is told about. Five
	// is what fits on the screen beside a runner's other health, and a sixth
	// reason a runner is idle is not read before the first five are acted on.
	maxHealthErrors = 5
	// healthErrorWindow keeps the block about now. The ring behind it holds
	// the daemon's last twenty records for the life of the process, so without
	// a window one failure at breakfast rides every sync until it exits.
	// An hour is long enough that a problem still causing trouble is still
	// here, since anything ongoing logs again.
	healthErrorWindow = time.Hour
	// maxHealthErrorMessage bounds one message. The ring already cuts at 2 KiB
	// for `yad status`, which is a local read; this is the wire, and five
	// entries at this bound put the whole block under 1.5 KiB.
	maxHealthErrorMessage = 256
)

// healthErrors is recent_errors: the runner's own words about what has gone
// wrong lately, newest first, for a hub asking why a runner is slow or idle.
//
// The message and nothing else. Every record also carries Attrs — the
// key=value pairs of the logging call — and that is where a wrapped error's
// text, a path on this machine and anything a child printed end up. A message
// is a string literal in this repository's source:
// TestLogMessagesReachingHealthAreLiterals fails `make check` for any shipped
// call site that formats a warning or an error — which is the whole of what
// the ring keeps — so what leaves the machine here is text a reviewer has
// read, never text a run produced.
// That is the guarantee DEV-60 had to add to HarnessReport.Error after the
// fact, and the one DEV-67 still owes Warnings.
//
// One ring, and every connection reports from it, so a hub hears about trouble
// that was not its own — "could not reach the hub" reaches hub B when it was
// hub A that would not answer. Deliberate, and bounded by the same rule as the
// rest: a connection's name and a hub's URL are runtime values, so a message
// carrying either would have to be formatted at run time, which the literal
// test makes impossible. What hub B learns is that something was unreachable,
// never which hub or where. That is nothing it could act
// on beyond what it already knows — under decision 0038 the owner trusts the
// hubs it connects, and the capability document has already told each one the
// capacity it is sharing. A struggling runner is news to everyone offering it
// work, so the per-connection alternative would cost information and buy
// nothing.
//
// Repeats collapse to their newest occurrence: a hub that cannot be reached
// logs every sync, and five copies of one sentence tell a hub less than five
// different ones do.
func healthErrors(recs []logfile.Record, now time.Time) []string {
	var out []string
	seen := make(map[string]bool, maxHealthErrors)
	for i := len(recs) - 1; i >= 0 && len(out) < maxHealthErrors; i-- {
		r := recs[i]
		if now.Sub(r.Time) > healthErrorWindow {
			continue
		}
		msg := cutTo(r.Message, maxHealthErrorMessage)
		if msg == "" || seen[msg] {
			continue
		}
		seen[msg] = true
		out = append(out, r.Time.UTC().Format(time.RFC3339)+" "+r.Level.String()+" "+msg)
	}
	return out
}

// cutTo bounds s to n bytes without splitting a rune.
func cutTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}
