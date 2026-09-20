package conformance

import (
	"fmt"
	"io"
	"strings"
)

// unchecked is what this suite does not check, and why. Absence is data: its
// reader is someone implementing a hub from §2, who would otherwise take
// everything the suite is silent about for something the protocol does not
// require.
var unchecked = []struct{ rule, section, why string }{{
	rule:    "POST /runners/{runner}/deregister — the credential dies, and the hub marks the runs that runner held lost.",
	section: sectionCalls,
	why:     "`yad hub`, the implementation this suite is run against, answers 501 there while the behaviour is built (yad DEV-81). A hub you write should implement it.",
}, {
	rule:    "The controls — cancel, interrupt, steer, close_session, drain — their repetition until the runner acts, and the single delivery of a steer.",
	section: sectionSync,
	why:     "nothing in the protocol asks a hub for a control: only a hub's own API can, and that is outside v1. A conformance runner can only wait for one it cannot cause.",
}, {
	rule:    "start_at, min_version, and the feature gates on drain, steer, interrupt, close_session and start_at.",
	section: sectionVersioning,
	why:     "each needs a run or a control the protocol gives a runner no way to ask for. What is checked is the other half of the same rule: that a hub sends no control it should have gated.",
}, {
	rule:    "Sessions stay put: the first claim in a session binds it to that runner, and its later runs are offered to that runner alone, one at a time.",
	section: sectionSync,
	why:     "it needs two runs in one session, which only a hub's own way of queueing runs can arrange.",
}, {
	rule:    "Capacity goes round the hubs: one pool shared between every connection, a unit at a time.",
	section: sectionSync,
	why:     "it is a rule about one runner across several hubs, not about one hub, so no hub can pass or fail it.",
}, {
	rule:    "The caps on tool output, event text and a result's final text, and the halving of a batch a proxy refused.",
	section: sectionEvents,
	why:     "they are what a runner must not exceed, not what a hub must enforce.",
}, {
	rule:    "Whether events and a result are refused from a runner that is not the run's holder while another runner holds it.",
	section: sectionEvents + ", " + sectionResult,
	why:     "it needs two runners at once, and so two registration tokens; this suite holds one. What is checked is the near half: that a run the hub cannot match to the calling runner is refused.",
}, {
	rule:    "Whether the copy of a resent event that the hub keeps is the first one.",
	section: sectionEvents,
	why:     "v1 gives a runner no way to read an event back, so nothing outside the hub can see which copy it stored. What is checked is that a resent batch is accepted and acknowledged no further back than before.",
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
		wrap(w, "    ", "    ", u.section+" — "+u.why)
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
