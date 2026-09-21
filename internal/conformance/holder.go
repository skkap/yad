package conformance

import (
	"context"
	"net/url"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/buildinfo"
)

// The far half of §2's holder rule: a run one runner holds takes nothing from
// another. The near half — a run the hub cannot match to the caller at all —
// is events/not-held and result/not-held, and needs only the one runner.

// otherRunner is the second runner, registered with Options.SecondToken.
type otherRunner struct {
	id   string
	cred string
}

func checkEventsHeldByAnother(ctx context.Context, s *session) error {
	o, err := s.otherRunner(ctx)
	if err != nil {
		return err
	}
	// seqFirst, which the holder has already sent: a hub that wrongly takes
	// the batch then holds nothing the later event checks would read as a
	// gap, so this check's failure stays this check's.
	a, err := s.c.do(ctx, call{path: eventsPath(s.report), bearer: o.cred, body: v1.EventBatch{Events: []v1.Event{{
		Seq: seqFirst, At: time.Now().UTC(), Kind: v1.EventText,
		Text: "yad conformance: a runner that does not hold this run sent this event, and a hub following the protocol refuses it",
	}}}})
	if err != nil {
		return err
	}
	if a.ok() {
		return brokenf("the hub took a batch of events for run %s from runner %s, and it is runner %s's: whoever reads that run now reads output its harness never produced: %s",
			s.report, o.id, s.runner, a)
	}
	return notHolder(a)
}

func checkResultHeldByAnother(ctx context.Context, s *session) error {
	o, err := s.otherRunner(ctx)
	if err != nil {
		return err
	}
	// succeeded, never the failed the holder reports next: a hub that refuses
	// this in its answer and stores it anyway then holds a state the holder's
	// own report conflicts with, and result/applied fails on the 409. The
	// same state would pass there as an idempotent repeat and hide the write.
	a, err := s.c.do(ctx, call{path: resultPath(s.report), bearer: o.cred, body: v1.Result{
		State: v1.RunSucceeded, LastSeq: s.lastSeq,
	}})
	if err != nil {
		return err
	}
	if a.ok() {
		return brokenf("the hub took a terminal state for run %s from runner %s, which was never offered it and does not hold it — runner %s does: %s",
			s.report, o.id, s.runner, a)
	}
	return notHolder(a)
}

// otherRunner registers the second runner the first time a check needs it.
// A check marked second never gets here without the token; the guard is for
// one that forgets the mark, which would otherwise register with no bearer.
func (s *session) otherRunner(ctx context.Context) (*otherRunner, error) {
	if s.opts.SecondToken == "" {
		return nil, skip{noSecondToken}
	}
	if s.other == nil && s.otherErr == nil {
		s.other, s.otherErr = s.registerOther(ctx)
	}
	return s.other, s.otherErr
}

func (s *session) registerOther(ctx context.Context) (*otherRunner, error) {
	id, err := newID()
	if err != nil {
		return nil, err
	}
	fp, err := newID()
	if err != nil {
		return nil, err
	}
	doc := s.doc()
	doc.RunnerID = DefaultHarness + "-" + id
	a, err := s.c.do(ctx, call{path: registerPath, bearer: s.opts.SecondToken, body: v1.RegisterRequest{Capabilities: doc}})
	if err != nil {
		return nil, err
	}
	if !a.ok() {
		if e, ok := a.envelope(); ok && e.Code == v1.CodeVersionTooOld {
			return nil, skipf("this hub refuses yad %s and said so: %s. §2 lets a hub set a version floor, so run the suite from a build at or above it to check this",
				buildinfo.Version, s.c.hide(e.Message))
		}
		return nil, brokenf("the hub refused the second registration token, which this check registers a second runner with: %s", a)
	}
	var res v1.RegisterResponse
	if err := a.decode(&res); err != nil {
		return nil, err
	}
	if res.RunnerCredential == "" {
		// Described, not printed, for register/exchange's reason: the
		// credential may be in the body under a name of the hub's own.
		return nil, brokenf("the second registration answered with no runner_credential: %s -> %d, and the body is not printed because a register answer can carry a credential under any name a hub gives it",
			a.Call, a.Status)
	}
	s.c.learn(res.RunnerCredential)
	// One sync, declaring no free capacity, so the second runner is as live
	// as any runner a hub lets near a run. Without it a hub refusing a runner
	// only because it has never synced would pass both checks without having
	// asked who holds the run.
	sa, err := s.c.do(ctx, call{
		path: "/runners/" + url.PathEscape(doc.RunnerID) + "/sync", bearer: res.RunnerCredential,
		body: v1.SyncRequest{
			RunnerID: doc.RunnerID, Fingerprint: fp, Capabilities: &doc,
			Health: v1.Health{Harnesses: []v1.HarnessHealth{{ID: s.opts.Harness, Ready: true}}},
		},
	})
	if err != nil {
		return nil, err
	}
	if !sa.ok() {
		return nil, brokenf("the second runner registered and its first sync was refused: %s", sa)
	}
	// A sync answer may carry a run's grants whatever capacity was declared,
	// and a secret that reached this suite is never printed.
	var sync v1.SyncResponse
	if err := sa.decode(&sync); err == nil {
		for _, r := range sync.Runs {
			for _, g := range r.Grants {
				s.c.learn(g.Value)
			}
		}
	}
	return &otherRunner{id: doc.RunnerID, cred: res.RunnerCredential}, nil
}
