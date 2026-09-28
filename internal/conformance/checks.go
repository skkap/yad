package conformance

import (
	"encoding/json"
	"fmt"
	"slices"

	v1 "github.com/skkap/yad/protocol/v1"
)

// Where each rule is written down. A failure names one of these because its
// reader is someone implementing a hub, who has HUB.md — the hub contract — and
// not this repository, so each is the HUB.md section that states the rule and
// not the part of ARCHITECTURE.md that says why.
//
// hubSections are the headings of HUB.md's numbered sections, spelled exactly
// as HUB.md spells them, and a section's sub is one of its ### headings the
// same way: a test finds every one cited here in HUB.md, so renaming a heading
// there fails a test rather than leaving failures pointing at nothing.
var hubSections = map[int]string{
	2:  "Wire basics",
	3:  "The calls",
	4:  "Runs",
	5:  "Leases, timings, and runners that go away",
	6:  "Events and results",
	7:  "Controls and features",
	8:  "Sessions",
	9:  "Grants",
	10: "Errors and `next_action`",
	11: "Versioning",
}

type section struct {
	n   int
	sub string
}

func (s section) String() string {
	c := fmt.Sprintf("HUB.md §%d %s", s.n, hubSections[s.n])
	if s.sub != "" {
		c += " › " + s.sub
	}
	return c
}

var (
	hubWire          = section{n: 2}
	hubCalls         = section{n: 3}
	hubRegister      = section{3, "`POST /runners/register`"}
	hubSync          = section{3, "`POST /runners/{runner}/sync`"}
	hubEventsCall    = section{3, "`POST /runs/{run}/events`"}
	hubResultCall    = section{3, "`POST /runs/{run}/result`"}
	hubRunRules      = section{4, "Rules the schema cannot state"}
	hubOffers        = section{4, "Who may be offered what"}
	hubLeases        = section{n: 5}
	hubEvents        = section{6, "Events"}
	hubResults       = section{6, "Results"}
	hubRunnerAnswers = section{6, "How a runner treats your answers to events and results"}
	hubControls      = section{n: 7}
	hubSessions      = section{n: 8}
	hubErrors        = section{n: 10}
	hubVersioning    = section{n: 11}
	hubGrants        = section{n: 9}
)

// checks is the suite, in the order it runs. The order is part of it: a check
// registers the runner every later one authenticates as, and the run the event
// and result rules are made against is a run an earlier check claimed.
func checks() []check {
	return []check{{
		id:      "errors/unknown-path",
		rule:    "Every response under the connection's base URL carries the error envelope: a path that is no operation answers {\"error\": {code, message, next_action}}, never a plain-text 404.",
		section: hubErrors,
		needs:   nothing,
		run:     checkUnknownPath,
	}, {
		id:      "errors/wrong-method",
		rule:    "Every response under the connection's base URL carries the error envelope: a method an operation does not have answers with one too, never a plain-text 405.",
		section: hubErrors,
		needs:   nothing,
		run:     checkWrongMethod,
	}, {
		id:      "register/token-required",
		rule:    "register is authenticated by a registration token the hub itself issued: a hub registers no runner without a bearer, and none with a bearer it never issued.",
		section: hubRegister,
		needs:   nothing,
		run:     checkRegisterNeedsToken,
	}, {
		id:      "register/exchange",
		rule:    "register exchanges a registration token for a runner credential, which authenticates every later call.",
		section: hubRegister,
		needs:   nothing,
		run:     checkRegister,
	}, {
		id:      "register/token-is-one-time",
		rule:    "A registration token registers one runner once: presented a second time, for any runner, it is refused.",
		section: hubRegister,
		needs:   credential,
		run:     checkTokenIsOneTime,
	}, {
		id:      "sync/credential-required",
		rule:    "Every call but register is authenticated by the runner credential: a sync is refused when its bearer is one the hub did not issue, and refused when it carries none at all.",
		section: hubSync,
		needs:   credential,
		run:     checkCredentialRequired,
	}, {
		id:      "sync/runner-id-matches-the-path",
		rule:    "The body's runner_id equals the runner the path names, or the sync is refused: a hub reading one of the two has two answers to which runner is syncing.",
		section: hubSync,
		needs:   credential,
		run:     checkRunnerIDMatchesThePath,
	}, {
		id:      "sync/document-runner-id-matches-the-path",
		rule:    "A capability document in a sync names the runner the path names, or the sync is refused as invalid: stored, it would describe this runner by a document written for another.",
		section: hubSync,
		needs:   credential,
		run:     checkDocumentRunnerIDMatchesThePath,
	}, {
		id:      "sync/another-runners-credential",
		rule:    "The credential on a sync belongs to the runner the path names: another runner's credential is refused, or any runner a hub knows can claim, renew and be offered another's runs.",
		section: hubSync,
		needs:   credential,
		second:  true,
		run:     checkAnotherRunnersCredential,
	}, {
		id:      "protocol-header/missing",
		rule:    "A request whose Yad-Protocol header is missing is refused with 426 unsupported_protocol, before its body is read — on every call, not only on sync.",
		section: hubCalls,
		needs:   credential,
		run:     checkProtocolHeaderMissing,
	}, {
		id:      "protocol-header/another-version",
		rule:    "A request whose Yad-Protocol header names another version is refused with 426 unsupported_protocol, before its body is read — on every call, not only on sync.",
		section: hubCalls,
		needs:   credential,
		run:     checkProtocolHeaderOtherVersion,
	}, {
		id:      "sync/empty-lists-omitted",
		rule:    "An absent list is an empty list: a sync response with no runs and no controls omits both fields rather than sending [] or null.",
		section: hubWire,
		needs:   credential,
		run:     checkEmptyListsOmitted,
	}, {
		id:      "sync/unknown-fields-ignored",
		rule:    "Unknown fields are ignored: a sync carrying a field this version of the protocol does not define is accepted, so a field added within v1 never breaks an older hub or runner.",
		section: hubWire,
		needs:   credential,
		run:     checkUnknownFieldsIgnored,
	}, {
		id:      "sync/dashboard-health-optional",
		rule:    "A sync is refused only over what routing reads: one whose health leaves out load, disk_free_bytes, spool_depth and outbox_depth — the dashboard fields — is accepted, since a refused sync renews no lease.",
		section: hubSync,
		needs:   credential,
		run:     checkDashboardHealthOptional,
	}, {
		id:      "sync/logins-accepted",
		rule:    "A sync reporting hub logins in logins is accepted, one reporting a login the hub never started included: a runner repeats each until a sync carrying its end is answered, and a refused sync renews no lease.",
		section: hubControls,
		needs:   credential,
		run:     checkLoginsAccepted,
	}, {
		id:      "sync/report-capabilities",
		rule:    "A sync whose fingerprint differs from the one that came with the document the hub holds, and that carries no document, is answered with a report_capabilities control.",
		section: hubSync,
		needs:   credential,
		run:     checkReportCapabilities,
	}, {
		id:      "sync/free-capacity",
		rule:    "A hub never offers a sync more runs than the free capacity that sync declared: not more than the total, and not more than the figure the sync gave for that harness, which is the owner's cap and is never a run to be claimed and then found to be over it.",
		section: hubOffers,
		needs:   credential,
		run:     checkFreeCapacity,
	}, {
		id:      "sync/cancel-for-a-run-not-held",
		rule:    "A run a sync lists that the runner does not hold — never offered to it, offered to another, or already finished — is answered with a cancel control for that run.",
		section: hubSync,
		needs:   credential,
		run:     checkCancelForRunNotHeld,
	}, {
		id:      "sync/unlisted-offer-taken-back",
		rule:    "An offered run the next sync does not list was never received, and that sync takes it back into the queue: a claim of it listed after that is answered with a cancel, because the offer is no longer open.",
		section: hubSync,
		needs:   credential,
		run:     checkUnlistedOfferTakenBack,
	}, {
		id:      "sync/claim-by-listing",
		rule:    "A run offered in a sync response is claimed when the runner lists it in its next sync: the hub neither offers it again nor answers it with a cancel.",
		section: hubSync,
		needs:   heldRun,
		run:     checkClaimByListing,
	}, {
		id:      "run/offer-validates",
		rule:    "A run breaking a rule the schema cannot state — no model, two grants by one name, two file grants whose names differ only by case — is refused whole, and a hub offers no run that does not validate.",
		section: hubRunRules,
		needs:   heldRun,
		run:     checkOfferedRunIsValid,
	}, {
		id:      "run/gated-features",
		rule:    "Neither side uses what the other did not advertise: a run in a live session goes only to a runner advertising live_sessions, a run whose start_at is still ahead only to one advertising start_at, since any other runner starts it on arrival, and a run carrying an effort only to one advertising effort, since any other runs the harness at its default.",
		section: hubControls,
		needs:   heldRun,
		run:     checkGatedRunsAreNotOffered,
	}, {
		id:      "errors/invalid-body",
		rule:    "A body that does not validate is refused with the code invalid under a 4xx other than 413, on sync, events and result alike: invalid is what a runner drops a report on, where a 5xx is retried for ever and a 413 halves a batch that was never too large.",
		section: hubErrors,
		needs:   heldRun,
		run:     checkInvalidBody,
	}, {
		id:      "events/credential-required",
		rule:    "Every call but register is authenticated by the runner credential, on every call and not only on sync: a batch of events for a run this runner holds is refused when the bearer is one the hub never issued, or absent.",
		section: hubEventsCall,
		needs:   heldRun,
		run:     checkEventsCredentialRequired,
	}, {
		id:      "events/seq-from-one",
		rule:    "Events are numbered from 1: an event with seq 0 is refused as invalid, not stored.",
		section: hubEvents,
		needs:   heldRun,
		run:     checkEventsSeqFromOne,
	}, {
		id:      "events/acked-through",
		rule:    "acked_through is the highest seq up to which the hub holds every event: a batch that leaves a gap does not move it past the gap, and the batch that fills the gap moves it over everything already held.",
		section: hubEvents,
		needs:   heldRun,
		run:     checkAckedThrough,
	}, {
		id:      "events/idempotent",
		rule:    "Events are idempotent by (run, seq): a batch the runner sends again is accepted, and acknowledged no further back than the first time.",
		section: hubEvents,
		needs:   heldRun,
		run:     checkEventsIdempotent,
	}, {
		id:      "events/unknown-fields-ignored",
		rule:    "Unknown fields are ignored on every call, not only on sync: a batch of events carrying a field this version of the protocol does not define is accepted.",
		section: hubWire,
		needs:   heldRun,
		run:     checkEventsUnknownFields,
	}, {
		id:      "events/not-held",
		rule:    "Only the runner a run was claimed by may append to it: a hub takes no events for a run it cannot match to the calling runner — 403 not_holder, or a not_found for a run it has never heard of.",
		section: hubEvents,
		needs:   credential,
		run:     checkEventsNotHeld,
	}, {
		id:      "result/not-held",
		rule:    "A hub applies a result only from the runner the run was offered to or claimed by: a result for a run it cannot match to the calling runner is refused — 403 not_holder, or a not_found for a run it has never heard of — and never applied.",
		section: hubResults,
		needs:   credential,
		run:     checkResultNotHeld,
	}, {
		id:      "events/held-by-another",
		rule:    "Only the runner a run was claimed by may append to it: a batch of events another runner sends for it is refused — 403 not_holder, or a not_found from a hub that will not name the run to that runner — and never written into the run, whose stream would otherwise show output its harness never produced.",
		section: hubEvents,
		needs:   heldRun,
		second:  true,
		run:     checkEventsHeldByAnother,
	}, {
		id:      "result/held-by-another",
		rule:    "A hub applies a result only from the runner the run was offered to or claimed by: a terminal state another runner reports for it is refused — 403 not_holder, or a not_found from a hub that will not name the run to that runner — and never applied.",
		section: hubResults,
		needs:   heldRun,
		second:  true,
		run:     checkResultHeldByAnother,
	}, {
		id:      "result/terminal-state-only",
		rule:    "A result carries one of the five terminal states: one naming a state that is not terminal is refused as invalid, and never applied.",
		section: hubResultCall,
		needs:   heldRun,
		run:     checkResultIsTerminal,
	}, {
		id:      "result/applied",
		rule:    "A hub applies the terminal state the runner holding a run reports.",
		section: hubResults,
		needs:   heldRun,
		run:     checkResultApplied,
	}, {
		id:      "result/idempotent",
		rule:    "A result is applied at most once, and the same terminal state again is acknowledged: the runner retries from its outbox until it hears a 2xx.",
		section: hubResults,
		needs:   heldRun,
		run:     checkResultIdempotent,
	}, {
		id:      "result/conflict",
		rule:    "A different terminal state, once the hub holds one, is refused with 409 conflict; the hub's state stands and the runner stops reporting.",
		section: hubResults,
		needs:   heldRun,
		run:     checkResultConflict,
	}, {
		id:      "result/credential-required",
		rule:    "Every call but register is authenticated by the runner credential, on every call and not only on sync: a terminal state for a run this runner holds is refused when the bearer is one the hub never issued, or absent.",
		section: hubResultCall,
		needs:   heldRun,
		run:     checkResultCredentialRequired,
	}, {
		id:      "result/unknown-fields-ignored",
		rule:    "Unknown fields are ignored on every call, not only on sync: a result carrying a field this version of the protocol does not define is accepted.",
		section: hubWire,
		needs:   heldRun,
		run:     checkResultUnknownFields,
	}, {
		id:      "errors/too-large",
		rule:    "A body refused for its size is refused with 413, whatever the code: a runner halves an events batch and retries a result on a 413, and drops what was refused as invalid.",
		section: hubRunnerAnswers,
		needs:   heldRun,
		run:     checkTooLarge,
	}, {
		id:      "events/after-the-run-ends",
		rule:    "The runner a run was claimed by may append to it after it has ended, so a batch still in its spool when the result landed is not lost.",
		section: hubLeases,
		needs:   heldRun,
		run:     checkEventsAfterTheRunEnds,
	}, {
		id:      "lease/lapse",
		rule:    "Every sync renews the lease on every run it lists, and only those: a run whose lease lapses is lost on the hub's side, so the hub stops treating it as held and refuses a later result with 409 conflict.",
		section: hubLeases,
		needs:   secondRun,
		run:     checkLeaseLapse,
	}, {
		id:      "lease/offer-lapse",
		rule:    "An offer carries the same lease as a claim: an offer its runner does not claim within the lease named beside it goes back in the queue, a claim listed after that is answered with a cancel for the run, and the run is offered again.",
		section: hubLeases,
		needs:   credential,
		run:     checkOfferLapse,
	}, {
		id:      "result/refusal-after-offer-taken-back",
		rule:    "A hub takes a result from the runner a run is offered to only while the offer is open: once a sync has left the run out, taking the offer back, a refusal from that runner is refused with 403 not_holder, since the run may by then be another runner's.",
		section: hubResults,
		needs:   credential,
		run:     checkRefusalAfterOfferTakenBack,
	}, {
		id:      "result/refusal-before-claim",
		rule:    "A hub takes a result from the runner a run is offered to as well as from the one that claimed it — a failed result with class refused, for a run never listed, is how a runner declines one — and does not offer a run again once it is refused.",
		section: hubResults,
		needs:   credential,
		run:     checkRefusalBeforeClaim,
	}, {
		id:      "sync/offers-within-capacity",
		rule:    "A hub never offers a sync more runs than the free capacity that sync declared — in every answer it gives, not only in answer to a sync written to test it.",
		section: hubOffers,
		needs:   credential,
		run:     checkOffersWithinCapacity,
	}, {
		id:      "sync/timings",
		rule:    "Timings belong to the hub: the sync interval it names at register is between 5 s and 60 s, and the next_sync_ms of a sync between 3 s and 60 s.",
		section: hubLeases,
		needs:   credential,
		run:     checkTimings,
	}, {
		id:      "sync/lease-outlasts-the-interval",
		rule:    "The lease a hub names is never shorter than the interval it names beside it: a shorter one lapses on a runner that synced exactly when it was asked to, so the hub takes back the runs of a runner doing everything right.",
		section: hubLeases,
		needs:   credential,
		run:     checkLeaseOutlastsTheInterval,
	}, {
		id:      "versioning/controls-are-gated",
		rule:    "Neither side uses what the other did not advertise: drain, close_session, steer and interrupt go only to a runner whose capability document advertises each by name, start_login, login_code, login_token and cancel_login only to one advertising login, remove_account and a login carrying add only to one advertising accounts, and this runner advertises none.",
		section: hubControls,
		needs:   credential,
		run:     checkControlsAreGated,
	}, {
		id:      "errors/next-action",
		rule:    "Every error a hub returns carries a next_action, because a runner on a customer's machine is debugged by whoever reads it.",
		section: hubErrors,
		needs:   nothing,
		run:     checkNextAction,
	}}
}

// refusedWith checks an answer is the refusal a rule names by status and code.
func refusedWith(a *answer, status int, code string) error {
	if a.Status != status {
		return brokenf("this rule asks for %d %s, and the hub answered: %s", status, code, a)
	}
	e, ok := a.envelope()
	switch {
	case !ok:
		return brokenf("there is no {\"error\": {...}} envelope in the answer: %s", a)
	case e.Code != code:
		return brokenf("the error code is %q, not %q: %s", e.Code, code, a)
	}
	return nil
}

// refused checks an answer is a refusal carrying the envelope, where the rule
// names no status or code of its own. Which 4xx it is is the hub's business.
func refused(a *answer) error {
	if a.ok() {
		return brokenf("the hub accepted it: %s", a)
	}
	e, ok := a.envelope()
	switch {
	case !ok:
		return brokenf("there is no {\"error\": {...}} envelope in the answer: %s", a)
	case e.Code == "" || e.Message == "":
		return brokenf("the envelope has no %s: %s", missingField(e), a)
	}
	return nil
}

func missingField(e v1.Error) string {
	switch {
	case e.Code == "" && e.Message == "":
		return "code and no message"
	case e.Code == "":
		return "code"
	}
	return "message"
}

// hasCancel reports whether the controls carry a cancel for this run.
func hasCancel(controls []v1.Control, runID string) bool {
	return slices.ContainsFunc(controls, func(c v1.Control) bool {
		return c.Kind == v1.ControlCancel && c.RunID == runID
	})
}

func runIDs(runs []v1.Run) []string {
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.RunID)
	}
	return ids
}

// notJSON is a body no hub can decode, for the rules that are about what a hub
// checks before it reads the body.
var notJSON = json.RawMessage(`{ this is not JSON`)

// withField marshals v and splices one more field into the object, for the
// rule about a field this version of the protocol does not have.
func withField(v any, name string, value any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	m[name] = raw
	return json.Marshal(m)
}

// syncBodyWith returns this runner's sync body with extra fields spliced in,
// which the protocol's own types cannot carry.
func (s *session) syncBodyWith(free int, extra map[string]any) ([]byte, error) {
	b, err := json.Marshal(s.syncRequest(free))
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	for k, v := range extra {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		m[k] = raw
	}
	return json.Marshal(m)
}
