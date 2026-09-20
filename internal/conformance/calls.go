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
	a, err := s.c.do(ctx, call{path: "/runners/register", body: v1.RegisterRequest{Capabilities: s.doc()}})
	if err != nil {
		return err
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
	// A second runner id, so a hub that refuses this is refusing the token
	// rather than the runner it already knows.
	id, err := newID()
	if err != nil {
		return err
	}
	doc := s.doc()
	doc.RunnerID = DefaultHarness + "-" + id
	a, err := s.c.do(ctx, call{path: "/runners/register", bearer: s.opts.Token, body: v1.RegisterRequest{Capabilities: doc}})
	if err != nil {
		return err
	}
	if a.ok() {
		return brokenf("the hub registered a second runner, %s, with a token that had already been exchanged: %s", doc.RunnerID, a)
	}
	return refused(a)
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

// The version is checked before the body, so these two send a body no hub can
// decode: a hub that answers 400 read the body first, which is what the rule
// forbids — a body shaped for another version fails validation in ways that
// say nothing about the cause.

func checkProtocolHeaderMissing(ctx context.Context, s *session) error {
	none := ""
	a, err := s.c.do(ctx, call{path: s.syncPath(), bearer: s.cred, raw: notJSON, protocol: &none})
	if err != nil {
		return err
	}
	return refusedWith(a, http.StatusUpgradeRequired, v1.CodeUnsupportedProtocol)
}

func checkProtocolHeaderOtherVersion(ctx context.Context, s *session) error {
	// A version this protocol will never be: the check is about the header
	// being read, not about which version comes next.
	other := "0"
	a, err := s.c.do(ctx, call{path: s.syncPath(), bearer: s.cred, raw: notJSON, protocol: &other})
	if err != nil {
		return err
	}
	return refusedWith(a, http.StatusUpgradeRequired, v1.CodeUnsupportedProtocol)
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
