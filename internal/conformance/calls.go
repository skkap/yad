package conformance

import (
	"context"
	"net/http"

	v1 "github.com/skkap/yad/protocol/v1"
)

// The rules of §2's "Calls": what authenticates a request, what every answer
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
		return brokenf("the hub refused the registration token: %s", a)
	}
	var res v1.RegisterResponse
	if err := a.decode(&res); err != nil {
		return err
	}
	if res.RunnerCredential == "" {
		return brokenf("the answer carries no runner_credential, so nothing after register can be authenticated: %s", a)
	}
	s.cred = res.RunnerCredential
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
	a, err := s.c.do(ctx, call{path: s.syncPath(), bearer: "yad-conformance-not-a-credential-" + id, body: s.syncRequest(0)})
	if err != nil {
		return err
	}
	return refused(a)
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
	a, err := s.c.do(ctx, call{path: eventsPath(s.report), bearer: "yad-conformance-not-a-credential-" + id, body: batch})
	if err != nil {
		return err
	}
	if a.ok() {
		return brokenf("the hub took events for run %s from a bearer it never issued: %s", s.report, a)
	}
	return refused(a)
}

// The version is checked before the body, so these two send a body no hub can
// decode: a hub that answers 400 read the body first, which is what the rule
// forbids — a body shaped for another version fails validation in ways that
// say nothing about the cause.
//
// Both go to two calls rather than one. The rule is about every request, and a
// hub built route by route — which a hub generated from openapi.yaml is —
// can hold the header where its sync is and nowhere else. Register is left out
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

// everyCall makes the same request of the sync and the events paths and wants
// 426 unsupported_protocol from both.
func (s *session) everyCall(ctx context.Context, build func(path string) call) error {
	run, err := s.strangeRun()
	if err != nil {
		return err
	}
	for _, path := range []string{s.syncPath(), eventsPath(run)} {
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
