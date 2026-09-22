package conformance

import (
	"context"
	"net/http"
	"strings"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/buildinfo"
)

// The rules of HUB.md §3, The calls: what authenticates a request, what every answer
// that is not a success looks like, and what is checked before the body.

func checkUnknownPath(ctx context.Context, s *session) error {
	a, err := s.c.do(ctx, call{path: "/not-an-operation", bearer: s.cred, body: v1.Ack{}})
	if err != nil {
		return err
	}
	return refused(a)
}

func checkWrongMethod(ctx context.Context, s *session) error {
	// Every protocol call is a POST, so a GET on one of them is a method no
	// operation has, whatever else a hub mounts at that path.
	a, err := s.c.do(ctx, call{method: http.MethodGet, path: "/runners/register", bearer: s.cred})
	if err != nil {
		return err
	}
	return refused(a)
}

func checkRegisterNeedsToken(ctx context.Context, s *session) error {
	// No bearer at all, and then one the hub cannot have issued. The second
	// is the one that matters: a hub that takes any unseen string as a
	// registration token will spend it, refuse its reuse, and pass every
	// other rule here while registering anyone who asks.
	a, err := s.c.do(ctx, call{path: "/runners/register", body: v1.RegisterRequest{Capabilities: s.doc()}})
	if err != nil {
		return err
	}
	if err := refused(a); err != nil {
		return err
	}
	id, err := newID()
	if err != nil {
		return err
	}
	doc := s.doc()
	doc.RunnerID = DefaultHarness + "-" + id
	a, err = s.c.do(ctx, call{path: "/runners/register", bearer: "yad-conformance-never-issued-" + id, body: v1.RegisterRequest{Capabilities: doc}})
	if err != nil {
		return err
	}
	if a.ok() {
		return brokenf("the hub registered runner %s with a token it never issued: %s", doc.RunnerID, a)
	}
	return refused(a)
}

func checkRegister(ctx context.Context, s *session) error {
	a, err := s.c.do(ctx, call{path: "/runners/register", bearer: s.opts.Token, body: v1.RegisterRequest{Capabilities: s.doc()}})
	if err != nil {
		return err
	}
	if !a.ok() {
		// A hub may set a version floor and refuse below it — HUB.md §11
		// — and the token is not burned by that refusal. Blaming the hub for
		// what HUB.md grants it is the mistake this suite exists not to make.
		if e, ok := a.envelope(); ok && e.Code == v1.CodeVersionTooOld {
			// The hub's own words, through the same guard as any body: a hub
			// that names the token it refused would otherwise put it in the
			// report by way of a message this suite quotes approvingly.
			return skipf("this hub refuses yad %s and said so: %s. %s lets a hub set a version floor, so this is its right and not a fault — run the suite from a build at or above that floor to check the rest",
				buildinfo.Version, s.c.hide(e.Message), hubVersioning)
		}
		return brokenf("the hub refused the registration token: %s", a)
	}
	var res v1.RegisterResponse
	if err := a.decode(&res); err != nil {
		return err
	}
	if res.RunnerCredential == "" {
		// Not printed, unlike every other failure here. A 200 with no
		// runner_credential is most likely a hub that named the field
		// something else — so the body holds a credential this suite cannot
		// recognise, under a key the redaction does not know, and it is the
		// one answer that must be described rather than shown.
		return brokenf("the answer carries no runner_credential, so nothing after register can be authenticated: %s -> %d, and the body is not printed because a register answer can carry a credential under any name a hub gives it",
			a.Call, a.Status)
	}
	s.cred = res.RunnerCredential
	// From here the credential is a secret this suite holds, and no failure
	// prints it — including the answer that has just carried it.
	s.c.learn(res.RunnerCredential)
	s.note(a.Call, res.SyncIntervalMS, res.LeaseMS)
	return nil
}

func checkTokenIsOneTime(ctx context.Context, s *session) error {
	// Both halves of the rule: another runner id, and then the one the token
	// was spent on. A hub that binds a spent token to its first runner and
	// lets that runner exchange it again would pass the first alone — and a
	// replayed token then rotates that runner's credential and takes over its
	// sessions.
	id, err := newID()
	if err != nil {
		return err
	}
	for _, runner := range []string{DefaultHarness + "-" + id, s.runner} {
		doc := s.doc()
		doc.RunnerID = runner
		a, err := s.c.do(ctx, call{path: "/runners/register", bearer: s.opts.Token, body: v1.RegisterRequest{Capabilities: doc}})
		if err != nil {
			return err
		}
		if a.ok() {
			return brokenf("the hub registered runner %s with a token that had already been exchanged: %s", runner, a)
		}
		if err := refused(a); err != nil {
			return err
		}
	}
	return nil
}

func checkCredentialRequired(ctx context.Context, s *session) error {
	id, err := newID()
	if err != nil {
		return err
	}
	// A bearer the hub never issued, and then none at all: a hub that checks
	// an Authorization header when one is there and takes the request when it
	// is not would pass the first alone, while anyone at all could sync as a
	// runner it knows and be handed runs and their grants.
	for _, bearer := range []string{"yad-conformance-not-a-credential-" + id, ""} {
		a, err := s.c.do(ctx, call{path: s.syncPath(), bearer: bearer, body: s.syncRequest(0)})
		if err != nil {
			return err
		}
		if err := refused(a); err != nil {
			return err
		}
	}
	return nil
}

// checkEventsCredentialRequired is the same rule on another call, and it takes
// a run this runner holds to ask it: against a run the hub has never heard of,
// a hub that authenticates nothing still answers not_found, and the refusal
// the rule is looking for cannot be told from that one.
func checkEventsCredentialRequired(ctx context.Context, s *session) error {
	id, err := newID()
	if err != nil {
		return err
	}
	batch := v1.EventBatch{Events: []v1.Event{{Seq: seqFirst, Kind: v1.EventText, Text: "yad conformance"}}}
	for _, bearer := range []string{"yad-conformance-not-a-credential-" + id, ""} {
		a, err := s.c.do(ctx, call{path: eventsPath(s.report), bearer: bearer, body: batch})
		if err != nil {
			return err
		}
		if a.ok() {
			return brokenf("the hub took events for run %s from a bearer it never issued: %s", s.report, a)
		}
		if err := refused(a); err != nil {
			return err
		}
	}
	return nil
}

// checkResultCredentialRequired is the same rule on the last call that has
// one. It re-sends the terminal state the hub already holds, so a hub that
// authenticates nothing changes nothing by taking it — and still shows that it
// took a result from a caller it cannot identify.
func checkResultCredentialRequired(ctx context.Context, s *session) error {
	id, err := newID()
	if err != nil {
		return err
	}
	res := v1.Result{State: v1.RunFailed, LastSeq: s.lastSeq, Error: &v1.RunError{Class: "refused", Message: "the yad conformance suite claimed this run to check the protocol and drives no harness"}}
	for _, bearer := range []string{"yad-conformance-not-a-credential-" + id, ""} {
		a, err := s.c.do(ctx, call{path: resultPath(s.report), bearer: bearer, body: res})
		if err != nil {
			return err
		}
		if a.ok() {
			return brokenf("the hub took a terminal state for run %s from a bearer it never issued: %s", s.report, a)
		}
		if err := refused(a); err != nil {
			return err
		}
	}
	return nil
}

// The version is checked before the body, so these two send a body no hub can
// decode: a hub that answers 400 read the body first, which is what the rule
// forbids — a body shaped for another version fails validation in ways that
// say nothing about the cause.
//
// Both go to every call this suite may safely send twice, not to one. The rule
// is about every request, and a hub built route by route — which a hub
// generated from openapi.yaml is — can hold the header where its sync is and
// nowhere else. Register is left out
// deliberately: it is the one call the registration token authenticates, and a
// hub that reads the body before the header would burn the operator's token on
// a request this suite sent to check a header.

func checkProtocolHeaderMissing(ctx context.Context, s *session) error {
	none := ""
	return s.everyCall(ctx, func(path string) call {
		return call{path: path, bearer: s.cred, raw: notJSON, protocol: &none}
	})
}

func checkProtocolHeaderOtherVersion(ctx context.Context, s *session) error {
	// A version this protocol will never be: the check is about the header
	// being read, not about which version comes next.
	other := "0"
	return s.everyCall(ctx, func(path string) call {
		return call{path: path, bearer: s.cred, raw: notJSON, protocol: &other}
	})
}

// everyCall makes the same request of the sync, events and result calls and
// wants 426 unsupported_protocol from each. Register is left out: it
// is the one call the registration token authenticates, and a hub that reads
// the body before the header would burn the operator's token on a request sent
// to check a header.
func (s *session) everyCall(ctx context.Context, build func(path string) call) error {
	run, err := s.strangeRun()
	if err != nil {
		return err
	}
	for _, path := range []string{s.syncPath(), eventsPath(run), resultPath(run)} {
		a, err := s.c.do(ctx, build(path))
		if err != nil {
			return err
		}
		if err := refusedWith(a, http.StatusUpgradeRequired, v1.CodeUnsupportedProtocol); err != nil {
			return err
		}
	}
	return nil
}

func checkNextAction(_ context.Context, s *session) error {
	for _, a := range s.c.seen {
		if a.ok() {
			continue
		}
		e, ok := a.envelope()
		switch {
		case !ok:
			return brokenf("this answer carries no {\"error\": {...}} envelope: %s", a)
		case e.NextAction == "":
			return brokenf("this error says what went wrong and not what to do about it: %s", a)
		}
	}
	return nil
}

// checkRunnerIDMatchesThePath sends this runner's own credential to its own
// path with another runner's id in the body. A hub that reads only one of the
// two ids has two answers to "which runner is this", and the one it records
// the sync against need not be the one it authenticated.
func checkRunnerIDMatchesThePath(ctx context.Context, s *session) error {
	id, err := newID()
	if err != nil {
		return err
	}
	req := s.syncRequest(0)
	req.RunnerID = DefaultHarness + "-" + id
	// Without the document, which names the runner too: a hub comparing the
	// document's id with the body's would refuse this for a reason of its own
	// and pass a rule it never checked.
	req.Capabilities = nil
	a, err := s.c.do(ctx, call{path: s.syncPath(), bearer: s.cred, body: req})
	if err != nil {
		return err
	}
	if a.ok() {
		return brokenf("a sync to runner %s's path carrying runner_id %s in its body was taken: %s", s.runner, req.RunnerID, a)
	}
	return refused(a)
}

// checkDocumentRunnerIDMatchesThePath sends this runner's own credential, id
// and fingerprint to its own path with a capability document naming another
// runner. The document is stored as the syncing runner's, so a hub taking it
// describes one runner by what another advertised (DEV-120). No capacity is
// declared, so a hub that takes it offers nothing on the strength of it.
func checkDocumentRunnerIDMatchesThePath(ctx context.Context, s *session) error {
	id, err := newID()
	if err != nil {
		return err
	}
	req := s.syncRequest(0)
	doc := s.doc()
	doc.RunnerID = DefaultHarness + "-" + id
	req.Capabilities = &doc
	a, err := s.c.do(ctx, call{path: s.syncPath(), bearer: s.cred, body: req})
	if err != nil {
		return err
	}
	if a.ok() {
		return brokenf("a sync to runner %s's path carrying a capability document for runner %s was taken: %s", s.runner, doc.RunnerID, a)
	}
	return refused(a)
}

// checkAnotherRunnersCredential syncs as this runner with the second runner's
// credential. A hub that checks only that a credential is one it issued lets
// any runner it knows claim, renew and be offered another's runs — and the
// grants that come with them.
func checkAnotherRunnersCredential(ctx context.Context, s *session) error {
	o, err := s.otherRunner(ctx)
	if err != nil {
		return err
	}
	a, err := s.c.do(ctx, call{path: s.syncPath(), bearer: o.cred, body: s.syncRequest(0)})
	if err != nil {
		return err
	}
	if a.ok() {
		return brokenf("runner %s's credential was taken for a sync as runner %s: %s", o.id, s.runner, a)
	}
	return refused(a)
}

// checkInvalidBody sends a body that is not JSON, with every header right, to
// each call that takes one, about a run this runner holds. invalid is the code
// a runner drops a report on and backs a sync off on; a 5xx sends it back to
// retry for ever what no retry will change, and a 413 has it halve a batch
// that was never too large.
func checkInvalidBody(ctx context.Context, s *session) error {
	for _, path := range []string{s.syncPath(), eventsPath(s.report), resultPath(s.report)} {
		a, err := s.c.do(ctx, call{path: path, bearer: s.cred, raw: notJSON})
		if err != nil {
			return err
		}
		if err := refusedInvalid(a); err != nil {
			return err
		}
	}
	return nil
}

// refusedInvalid checks an answer refuses a request as invalid: the code a
// runner acts on, under a 4xx that tells it nothing else. Which 4xx is the
// hub's (HUB.md §10), except 413, whose status a runner acts on first.
func refusedInvalid(a *answer) error {
	if err := refused(a); err != nil {
		return err
	}
	e, _ := a.envelope()
	switch {
	case a.Status < 400 || a.Status >= 500 || a.Status == http.StatusRequestEntityTooLarge:
		return brokenf("the hub refused it with %d, and a request that does not validate is a 4xx with the code %q — a 5xx is retried for ever, and a 413 has a runner halve a batch that was never too large: %s",
			a.Status, v1.CodeInvalid, a)
	case e.Code != v1.CodeInvalid:
		return brokenf("the error code is %q, not %q, which is the code a runner drops a report on rather than resending it for ever: %s", e.Code, v1.CodeInvalid, a)
	}
	return nil
}

// oversize is how much padding makes a body too large for a hub that keeps
// HUB.md's limit: yad hub reads under 16 MiB of an events or result body, and
// a mebibyte past it is past it without being a load test.
//
// A variable only for this package's own tests, which run the suite against a
// fake some sixty times over and give that fake a limit a thousandth the size
// (TestMain). Nothing outside the package can reach it.
var oversize = 17 << 20

// checkTooLarge sends an events batch and a result that are valid and too
// large: each carries what the hub already holds — the first event again, the
// terminal state again — and a field no version defines, holding the bytes.
// Unknown fields are ignored, so a hub may take either; a hub that refuses one
// for its size must say 413, because a runner halves a batch on a 413 and
// drops one refused as invalid.
func checkTooLarge(ctx context.Context, s *session) error {
	padding := strings.Repeat("x", oversize)
	batch, err := withField(v1.EventBatch{Events: []v1.Event{{
		Seq: seqFirst, At: time.Now().UTC(), Kind: v1.EventText,
		Text: "yad conformance: this run was claimed by the conformance suite, which drives no harness",
	}}}, "a_field_from_a_later_v1", padding)
	if err != nil {
		return err
	}
	result, err := withField(v1.Result{State: v1.RunFailed, LastSeq: s.lastSeq, Error: &v1.RunError{
		Class: "refused", Message: "the yad conformance suite claimed this run to check the protocol and drives no harness",
	}}, "a_field_from_a_later_v1", padding)
	if err != nil {
		return err
	}
	for _, c := range []call{
		{path: eventsPath(s.report), bearer: s.cred, raw: batch},
		{path: resultPath(s.report), bearer: s.cred, raw: result},
	} {
		a, err := s.c.do(ctx, c)
		if err != nil {
			return err
		}
		switch {
		case a.ok() || a.Status == http.StatusRequestEntityTooLarge:
			continue
		case a.Status == http.StatusConflict && c.path == resultPath(s.report):
			return skipf("the hub answered 409 conflict: it holds a terminal state for run %s other than the one this runner reported, which result/conflict says more about. Until that is fixed, how it refuses a body too large cannot be told", s.report)
		}
		return brokenf("a body of %d bytes, valid but for its size, was refused with %d and not 413; a runner halves an events batch and retries a result on a 413, and drops one refused any other way: %s",
			oversize, a.Status, a)
	}
	return nil
}
