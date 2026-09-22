package conformance

import (
	"fmt"
	"io"
	"strings"
)

// unchecked is what this suite does not check, and why. Absence is data: its
// reader is someone implementing a hub from HUB.md, who would otherwise take
// everything the suite is silent about for something the protocol does not
// require.
var unchecked = []struct {
	rule string
	// sections are where the rule is stated: more than one only where the
	// entry is two rules HUB.md states in different places.
	sections []section
	why      string
}{{
	rule:     "POST /runners/{runner}/deregister — the credential dies; the runs that runner held are lost, its offers go back in the queue, and its sessions close with the runs queued in them ended.",
	sections: []section{hubLeases},
	why:      "deregistering retires the runner every other check here is made as; the second runner --second-token registers could carry it, and does not yet. `yad hub` implements it; a hub you write should too.",
}, {
	rule:     "The controls — cancel, interrupt, steer, close_session, drain — their repetition until the runner acts, and the single delivery of a steer.",
	sections: []section{hubControls},
	why:      "nothing in the protocol asks a hub for a control: only a hub's own API can, and that is outside v1. A conformance runner can only wait for one it cannot cause.",
}, {
	rule:     "start_at, min_version, and the feature gates on drain, steer, interrupt, close_session and start_at.",
	sections: []section{hubControls, hubVersioning},
	why:      "each needs a run or a control the protocol gives a runner no way to ask for. What is checked is the other half of the same rule: that a hub sends no control it should have gated.",
}, {
	rule:     "Sessions stay put: the first claim in a session binds it to that runner, and its later runs are offered to that runner alone, one at a time.",
	sections: []section{hubSessions},
	why:      "it needs two runs in one session, which only a hub's own way of queueing runs can arrange.",
}, {
	rule:     "A runner silent for longer than the hub's abandon-after has its sessions closed and the runs queued in them ended, and keeps its credential: when it syncs again it is answered normally, with close_session for each of those sessions until it reports the close.",
	sections: []section{hubLeases},
	why:      "the silence is a day by default and every hub names its own, which v1 gives a runner no way to ask; no suite can wait out a length it cannot learn, and waiting a day would be no check anyone runs.",
}, {
	rule:     "Capacity goes round the hubs: one pool shared between every connection, a unit at a time.",
	sections: []section{hubOffers},
	why:      "it is a rule about one runner across several hubs, not about one hub, so no hub can pass or fail it.",
}, {
	rule:     "The caps on tool output, event text and a result's final text, and the halving of a batch a proxy refused.",
	sections: []section{hubEvents, hubRunnerAnswers},
	why:      "they are what a runner must not exceed, not what a hub must enforce.",
}, {
	rule:     "Whether register refuses a request whose Yad-Protocol header is missing or names another version, and whether it ignores a field this version does not define.",
	sections: []section{hubCalls, hubWire},
	why:      "register is the one call the registration token authenticates. A hub that reads the body before the header would burn the operator's token on a request sent only to check a header, and the unknown-field rule needs a registration that succeeds — which this suite has one token for, and spends on the registration it goes on to use. Both rules are checked on sync, events and result.",
}, {
	rule:     "Whether the copy of a resent event that the hub keeps is the first one.",
	sections: []section{hubEvents},
	why:      "v1 gives a runner no way to read an event back, so nothing outside the hub can see which copy it stored. What is checked is that a resent batch is accepted and acknowledged no further back than before.",
}}

// Print writes the report as the person who ran the suite reads it: every
// check by name, and for each failure the rule, where it is written, and what
// the hub did instead.
func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "yad conformance — protocol v%s against %s\n", "1", r.BaseURL)
	fmt.Fprintf(w, "as a runner advertising harness %q\n\n", r.Harness)
	for _, o := range r.Outcomes {
		fmt.Fprintf(w, "%s  %s\n", label(o.Status), o.ID)
		switch o.Status {
		case Failed:
			writeWrapped(w, o.Rule)
			fmt.Fprintf(w, "      %s\n", o.Section)
			writeWrapped(w, "Observed: "+o.Detail)
		case Skipped:
			writeWrapped(w, "Not checked: "+o.Detail)
		}
	}
	fmt.Fprintf(w, "\n%d passed, %d failed, %d skipped\n", r.count(Passed), r.count(Failed), r.count(Skipped))
	if n := r.count(Skipped); n > 0 {
		wrap(w, "", "", "A skip is not a pass: each one says what this hub gave the suite no way to check, and what would make it possible.")
	}
	if r.Interrupted {
		wrap(w, "", "", "This run was stopped before it finished, so the checks after that point were never made.")
	}
	fmt.Fprint(w, "\nNot checked here, and why — a hub still has to get these right:\n")
	for _, u := range unchecked {
		fmt.Fprintln(w)
		wrap(w, "  · ", "    ", u.rule)
		wrap(w, "    ", "    ", cite(u.sections)+" — "+u.why)
	}
}

func label(s Status) string {
	switch s {
	case Failed:
		return "FAIL"
	case Skipped:
		return "SKIP"
	}
	return "PASS"
}

// wrapAt is where the prose wraps: narrow enough to read in a terminal beside
// the six spaces every continued line is indented by.
const wrapAt = 76

func writeWrapped(w io.Writer, text string) { wrap(w, "      ", "      ", text) }

// wrap prints text under first, and every line after it under rest.
func wrap(w io.Writer, first, rest, text string) {
	indent, line := first, ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len([]rune(line))+1+len([]rune(word)) > wrapAt:
			fmt.Fprintf(w, "%s%s\n", indent, line)
			indent, line = rest, word
		default:
			line += " " + word
		}
	}
	if line != "" {
		fmt.Fprintf(w, "%s%s\n", indent, line)
	}
}

// cite joins the sections an entry is filed under.
func cite(ss []section) string {
	c := make([]string, len(ss))
	for i, s := range ss {
		c[i] = s.String()
	}
	return strings.Join(c, "; ")
}
