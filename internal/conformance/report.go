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
	rule:     "POST /runners/{runner}/deregister — the credential dies; the runs that runner held are lost, but for a claim the hub has asked to cancel, which ends cancelled, its offers go back in the queue, and its sessions close with the runs queued in them ended.",
	sections: []section{hubLeases},
	why:      "deregistering retires the runner every other check here is made as; the second runner --second-token registers could carry it, and does not yet. `yad hub` implements it; a hub you write should too.",
}, {
	rule:     "The controls — cancel, interrupt, steer, close_session, drain — their repetition until the runner acts, the single delivery of a steer, a cancelled claim the runner withdraws recorded cancelled rather than lost and the session it bound unbound, and offering nothing to a runner that is draining or has been asked to drain.",
	sections: []section{hubControls},
	why:      "nothing in the protocol asks a hub for a control: only a hub's own API can, and that is outside v1. A conformance runner can only wait for one it cannot cause.",
}, {
	rule:     "start_at, min_version, the feature gates on drain, steer, interrupt, close_session, start_at, effort and fork, and holding back every gated control and gated run while a fingerprint has moved and its document has not arrived.",
	sections: []section{hubControls, hubVersioning},
	why:      "each needs a run or a control the protocol gives a runner no way to ask for. What is checked is the other half of the same rule: that a hub sends no control it should have gated.",
}, {
	rule:     "Hub logins: start_login and login_token repeated until the runner reports the login, login_code while it reports it waiting, cancel_login until it reports it over; a token held only until the runner reports its login and never shown again; a login ended on the hub's own word only while no answer has carried it; a sent login the runner has not reported an end of within thirty minutes ended failed and its token blanked, a later report from the runner still replacing that end; a login the runner reported and then leaves out, not yet over, ended failed.",
	sections: []section{hubControls},
	why:      "only a hub's own API starts a login, which is outside v1, and this runner advertises no login feature to be sent one. What is checked is that none is sent to it, and that its reports are taken.",
}, {
	rule:     "Adding and removing accounts: start_login and login_token carrying add, and remove_account, only while the runner advertises accounts to this hub; remove_account repeated until neither the runner's capability document nor its health lists the account, or the runner stops advertising accounts.",
	sections: []section{hubControls},
	why:      "only a hub's own API adds or removes an account, which is outside v1, and this runner advertises no accounts feature to be sent either. What is checked is that neither is sent to it.",
}, {
	rule:     "Sessions stay put: the first claim in a session binds it to that runner, its later runs are offered to that runner alone, one at a time, and session.new is true for the run that opens a session and false for every later one.",
	sections: []section{hubSessions},
	why:      "it needs two runs in one session, which only a hub's own way of queueing runs can arrange.",
}, {
	rule:     "A close in closed_sessions is believed from the runner holding the session or the one its run was last offered to, whatever features it advertises, a repeat is the same news, and the runs still queued in a closed session end. Nothing is offered in a session the hub has sent close_session for until the close is reported.",
	sections: []section{hubSessions},
	why:      "this runner advertises no feature, so a hub never asks it to close a session, and the only sessions it has are the ones its held runs are in, which no runner closes; and seeing queued runs held back or ended needs a second run queued in the session, which only a hub's own queueing can arrange.",
}, {
	rule:     "Offers only for a harness the runner can drive — first-class, present and without an error — and, among those, preferably one whose health says ready.",
	sections: []section{hubOffers},
	why:      "the runs this suite is offered are the ones queued for the one harness it advertises; seeing a hub offer a harness it should not needs a run queued for that harness, which only a hub's own queueing can arrange.",
}, {
	rule:     "Lapsed leases are found by a timer as well as by a sync, so a hub whose only runner went away still records its runs lost.",
	sections: []section{hubLeases},
	why:      "anything this suite sends to see whether a run was lost is itself a request the hub can settle leases on, so a hub that settles them only when asked cannot be told from one that sweeps.",
}, {
	rule:     "A runner id the hub already knows re-registers only with a token issued for that runner.",
	sections: []section{hubRegister},
	why:      "a token issued for one runner comes from the hub's own API, which is outside v1; and trying the second token on the first runner's id would spend it, on a hub that spends tokens on a refused registration, before the runner it is for.",
}, {
	rule:     "A grant's value is kept only until its run reaches a terminal state.",
	sections: []section{hubGrants},
	why:      "v1 gives a runner no way to read a run back from a hub, so nothing outside the hub can see what it still holds.",
}, {
	rule:     "internal goes only with a 5xx, and never stands in for a refusal of a request the hub will never accept.",
	sections: []section{hubErrors},
	why:      "a fault is not something this suite can cause from outside. What is checked is the refusals it can cause: a body that does not validate, a run the caller does not hold, a body too large.",
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
	rule:     "Whether register refuses a request whose Yad-Protocol header is missing or names another version, whether it refuses a body that does not validate as invalid, and whether it ignores a field this version does not define.",
	sections: []section{hubCalls, hubWire},
	why:      "register is the one call the registration token authenticates. A hub that reads the body before the header would burn the operator's token on a request sent only to check a header, one that spends a token before it validates the body would burn it on a body sent to be refused, and the unknown-field rule needs a registration that succeeds — which this suite has one token for, and spends on the registration it goes on to use. All three are checked on sync, events and result.",
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
