package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"runtime"
	"slices"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/config"
)

// DefaultHarness is the harness id the suite advertises. No run asks for it,
// so a suite pointed at a working hub is offered nothing anyone was waiting
// on: to check the rules that need a run, queue one for this harness.
const DefaultHarness = "yad-conformance"

// DefaultLeaseWait is how long the suite will spend waiting for a lease to
// lapse. The lease is the hub's to choose (HUB.md §5) and the wait is real
// time, so the budget is the caller's; a hub whose lease is longer has that
// check skipped rather than silently passed.
const DefaultLeaseWait = 90 * time.Second

// offerSyncs is how many syncs the suite asks for work in before concluding
// that no run is queued for it. A hub may answer the first one with nothing
// and still be offering runs a moment later.
const offerSyncs = 3

// Options configure one run of the suite.
type Options struct {
	// BaseURL is the connection: every path in the protocol is relative to it.
	BaseURL string
	// Token is a registration token the hub issued and nobody has used.
	Token string
	// SecondToken is another one, optional. With it the suite registers a
	// second runner and has it send events and a result for a run the first
	// one holds, which is the only way to see a hub tell two of its runners
	// apart; without it those two checks are skipped, saying so.
	SecondToken string
	// Harness is the harness id the suite advertises; DefaultHarness when empty.
	Harness string
	// LeaseWait bounds the wait for a lease to lapse. Zero waits for none of
	// it and skips that check, saying so: the wait is real time against a
	// clock this suite does not share, so how much of it to spend is the
	// caller's to decide. `yad conformance` defaults it to DefaultLeaseWait.
	LeaseWait time.Duration
}

// Status is how one check came out.
type Status int

const (
	// Passed: the hub followed the rule.
	Passed Status = iota
	// Failed: the hub broke it.
	Failed
	// Skipped: the hub gave the suite no way to make the check — no run was
	// offered, or its lease is longer than the caller's budget. A skip is not
	// a pass, and the report says which and why.
	Skipped
)

// Outcome is one check's verdict. Rule and Section travel with it because the
// reader of a failure is someone writing a hub who does not have this
// repository open: the assertion is useless to them, the rule is the product.
type Outcome struct {
	ID      string
	Rule    string
	Section string
	Status  Status
	// Detail is what was observed for a failure, or why a check was skipped.
	Detail string
}

// Report is what one run of the suite found.
type Report struct {
	BaseURL string
	Harness string
	// Interrupted says the run was stopped before every check was made, so
	// the outcomes below are not the whole suite. Nothing that was not run is
	// a pass, and a report that ended early must not read like one.
	Interrupted bool
	Outcomes    []Outcome
}

// Failed reports whether any check failed. A skip is not a failure: it is a
// check nobody made.
func (r *Report) Failed() bool {
	return r.count(Failed) > 0
}

func (r *Report) count(s Status) int {
	n := 0
	for _, o := range r.Outcomes {
		if o.Status == s {
			n++
		}
	}
	return n
}

// check is one rule of the protocol and the way to find out whether a hub
// follows it.
type check struct {
	// id is the check's name in the report, "<area>/<rule>".
	id string
	// rule is the sentence the hub must satisfy, written so that someone
	// implementing a hub can act on it without reading this repository.
	rule string
	// section is where the rule is written down.
	section section
	// needs is what must already have happened for the check to be possible.
	needs requirement
	// second says the check needs Options.SecondToken. It is judged before
	// needs, so a run with neither a queued run nor the token names the flag
	// too: the flag is the one of the two the operator can see is missing.
	second bool
	run    func(context.Context, *session) error
}

// noSecondToken is why a check needing a second runner was not made.
const noSecondToken = "it needs a second runner, and no second registration token was given; create another one-time token on the hub and pass it with --second-token to check it"

// requirement is what a check needs before it can be made at all.
type requirement int

const (
	// nothing: the hub's URL is enough.
	nothing requirement = iota
	// credential: the suite must have registered.
	credential
	// heldRun: the hub must have offered a run the suite now holds.
	heldRun
	// secondRun: and a second one, left unrenewed for the lease to lapse.
	secondRun
)

// broken is a rule the hub broke, carrying what was observed.
type broken struct{ observed string }

func (b broken) Error() string { return b.observed }

func brokenf(format string, a ...any) error { return broken{fmt.Sprintf(format, a...)} }

// skipped is a check the hub gave the suite no way to make, and why.
type skip struct{ reason string }

func (s skip) Error() string { return s.reason }

func skipf(format string, a ...any) error { return skip{fmt.Sprintf(format, a...)} }

// session is what the checks share: the hub, the credential the suite earned,
// the runs it holds, and what the hub has said so far.
type session struct {
	opts Options
	c    *client

	runner      string
	fingerprint string
	cred        string

	// interval and lease are the timings the hub last named.
	interval, lease time.Duration
	// timings is every pair of them, with the call that carried it, for the
	// check that judges them all at the end.
	timings []timing
	// syncs is every sync response with the call that carried it, for the
	// check on which controls a hub may send to a runner advertising no
	// feature: a failure has to name the answer it read.
	syncs []syncSeen

	// held is what the suite lists in its syncs, in order, so two runs of the
	// suite make the same requests.
	held []v1.HeldRun
	// sentDoc says the capability document has gone out, which the first sync
	// carries: the hub kept no fingerprint from register.
	sentDoc bool

	// offered is every run the hub offered this runner, by id, for the checks
	// that need one run's specification. offers is each offer as it arrived,
	// including a second offer of the same id, because a run offered valid
	// once and invalid again was offered invalid — and the map alone keeps
	// only the last.
	offered map[string]v1.Run
	offers  []v1.Run
	// offeredAtOnce is the most runs one answer offered, which is what bounds
	// how many the suite can hold — and so which rules it can check at all.
	offeredAtOnce int
	// report is the run the event and result rules are checked against;
	// lapse is the one left out of every sync so its lease runs out.
	report, lapse string
	// lastSeq is the highest event seq the suite has sent for the run it
	// reports on, which its result has to name.
	lastSeq int64
	// lapseAt is when the hub last renewed the lapse run's lease, as this
	// machine's clock saw it. There is no shared clock with a hub, so the
	// wait is the hub's own lease_ms from this moment, and then some.
	lapseAt time.Time
	// leaseAtClaim is the lease the hub named in the answer that claimed it.
	leaseAtClaim time.Duration

	// other is the runner the second token registered, once a check has
	// asked for it, and otherErr why it could not be: the token is spent by
	// the first attempt, so the second check reads the first one's outcome.
	other    *otherRunner
	otherErr error
}

// syncSeen is one sync answer, the request it answered, and the free capacity
// that request declared — which is what the answer's offers are judged against.
type syncSeen struct {
	call string
	free int
	res  v1.SyncResponse
}

// timing is one hub-named pair of interval and lease, and where it came from.
type timing struct {
	call     string
	interval time.Duration
	lease    time.Duration
}

// Run drives every check against the hub at opts.BaseURL and reports what it
// found. The error is returned only when the suite could not start — a URL it
// will not send a credential to, or a missing token. Everything a hub did
// wrong is in the report, not in the error.
func Run(ctx context.Context, opts Options) (*Report, error) {
	if err := config.CheckHubURL(opts.BaseURL); err != nil {
		return nil, err
	}
	if opts.Token == "" {
		return nil, errors.New("a registration token is needed: create one on the hub (`yad hub token create`, or the hub's Add runner) and pass it with --token")
	}
	if opts.SecondToken != "" && opts.SecondToken == opts.Token {
		// The first registration spends it, and the second would then be
		// reported as the hub refusing a token it was right to refuse.
		return nil, errors.New("--second-token is the same token as --token, and a token registers one runner: create another on the hub and pass that")
	}
	if opts.Harness == "" {
		opts.Harness = DefaultHarness
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	fp, err := newID()
	if err != nil {
		return nil, err
	}
	s := &session{
		opts: opts,
		c:    newClient(opts.BaseURL, opts.Token, opts.SecondToken),
		// Every id this suite invents begins with the same word, whichever
		// harness was named, so whoever reads the hub's records afterwards
		// can see where this runner and its runs came from.
		runner:      DefaultHarness + "-" + id,
		fingerprint: fp,
		offered:     map[string]v1.Run{},
	}
	rep := &Report{BaseURL: config.RedactURL(opts.BaseURL), Harness: opts.Harness}
	all := checks()
	for i, ch := range all {
		rep.Outcomes = append(rep.Outcomes, s.make(ctx, ch))
		// A Ctrl-C between two checks ends the run. The rest are recorded as
		// what they are — not made — because a report listing only the checks
		// that happened to run before the signal, all of them passed, is a
		// clean bill of health for a suite that stopped early.
		if ctx.Err() != nil {
			rep.Interrupted = true
			for _, rest := range all[i+1:] {
				rep.Outcomes = append(rep.Outcomes, Outcome{
					ID: rest.id, Rule: rest.rule, Section: rest.section.String(), Status: Skipped,
					Detail: "the suite was stopped before this check ran",
				})
			}
			break
		}
	}
	return rep, nil
}

// make runs one check, or says why it could not be made. Whatever it returns,
// the deferred guard is the last gate before a sentence becomes part of the
// report, and the only one that covers all of them: a check writes the hub's
// own strings into its message — an error code, a run id, a control kind, the
// raw value of a field, the words of a validation failure — and each is a
// place a hub could have put a secret. Hiding here rather than at each site is
// what stops the next message anyone adds from being the one that leaks.
// The result is named so the deferred guard below mutates what is returned
// rather than a copy of it.
func (s *session) make(ctx context.Context, ch check) (out Outcome) {
	out = Outcome{ID: ch.id, Rule: ch.rule, Section: ch.section.String()}
	// One exit, so the guard at the end of this function covers every sentence
	// the report can carry and not only the ones a check wrote.
	defer func() { out.Detail = s.c.hide(out.Detail) }()
	if ch.second && s.opts.SecondToken == "" {
		out.Status, out.Detail = Skipped, noSecondToken
		return out
	}
	if reason := s.missing(ch.needs); reason != "" {
		out.Status, out.Detail = Skipped, reason
		return out
	}
	err := ch.run(ctx, s)
	var sk skip
	switch {
	case err == nil:
		out.Status = Passed
	case errors.As(err, &sk):
		out.Status, out.Detail = Skipped, sk.reason
	case ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)):
		// The check the signal landed in is not a rule the hub broke, and a
		// report naming a HUB.md rule against a hub that did nothing is worse
		// than one check fewer.
		//
		// Both halves are needed. On the error alone, a hub that does not
		// answer inside requestTimeout hands back a deadline of the client's,
		// and calling that "the suite was stopped" reports a stalled hub as
		// this suite's own interruption — and, since nothing then failed,
		// exits 0 on it. On the context alone, a signal arriving while a hub
		// was already answering wrongly relabels the hub's own failure as an
		// interruption, which loses a finding the suite had already made.
		out.Status, out.Detail = Skipped, "the suite was stopped while this check was being made"
	default:
		// A transport error is reported like any other failure: from the
		// outside, a hub that cannot be reached and a hub that answers
		// nonsense are the same finding for the person reading this.
		out.Status, out.Detail = Failed, err.Error()
	}
	return out
}

// missing says why a check cannot be made yet, or "" when it can.
func (s *session) missing(need requirement) string {
	switch {
	case need >= credential && s.cred == "":
		return "the suite never registered with this hub, so nothing needing a runner credential could be checked"
	case need >= heldRun && s.report == "" && len(s.offered) > 0:
		return "the hub offered runs and had taken them all back before the suite could claim one — an offer a sync does not list is requeued — so nothing needing a run it holds could be checked"
	case need >= heldRun && s.report == "":
		return "no run was offered to this runner, so nothing needing a run it holds could be checked; queue a run for harness " + s.opts.Harness + " and run the suite again"
	case need >= secondRun && s.lapse == "" && s.offeredAtOnce > 1:
		return "the hub had two runs open to this runner earlier and only one by the last sync, so the suite could not claim the two the lease rules need — one to report on, and one to leave unrenewed"
	case need >= secondRun && s.lapse == "" && len(s.offered) > 1:
		// Offering one run at a time breaks no rule of HUB.md's, and no way of
		// queueing runs gets around it: say so rather than repeat advice the
		// operator has already followed.
		return "this hub offered its runs one at a time, and the lease rules need two held at once — one to report on and one to leave unrenewed — so they could not be checked against it"
	case need >= secondRun && s.lapse == "":
		return "only one run was offered, and the lease rules need a second one to leave unrenewed; queue three runs for harness " + s.opts.Harness + " and run the suite again"
	}
	return ""
}

// doc is the capability document the suite advertises: one harness nobody
// queues runs for, and no protocol feature at all, so a hub that sends a
// control it must gate on one has broken HUB.md's feature rule visibly.
func (s *session) doc() v1.Capabilities {
	return v1.Capabilities{
		RunnerID:   s.runner,
		Name:       "yad conformance",
		YadVersion: buildinfo.Version,
		OS:         runtime.GOOS,
		Arch:       runtime.GOARCH,
		Harnesses: []v1.HarnessReport{{
			ID: s.opts.Harness, Label: "yad conformance", Kind: "first-class", Present: true,
			Version: buildinfo.Version,
		}},
		Capacity:   v1.Capacity{Total: advertisedCapacity},
		ObservedAt: time.Now().UTC(),
	}
}

// runsWanted is the most the suite holds at once: one run for the event and
// result rules, one to leave unrenewed for the lease to lapse.
//
// advertisedCapacity is larger so that the health in every sync adds up —
// a runner holding two runs and declaring two free is a runner that takes
// four, and a hub is entitled to believe both halves of what it is told.
const (
	runsWanted         = 2
	advertisedCapacity = 4
)

// syncRequest is what this runner sends, holding what it holds. byHarness is
// the free capacity declared for the suite's own harness, and is left out when
// it is negative — every harness a runner drives is optional there.
func (s *session) syncRequest(free int) v1.SyncRequest {
	return s.syncRequestCapped(free, -1)
}

func (s *session) syncRequestCapped(free, byHarness int) v1.SyncRequest {
	req := v1.SyncRequest{
		RunnerID:    s.runner,
		Fingerprint: s.fingerprint,
		Health: v1.Health{
			FreeCapacity: v1.Capacity{Total: free},
			Harnesses:    []v1.HarnessHealth{{ID: s.opts.Harness, Ready: true}},
		},
		Runs: slices.Clone(s.held),
	}
	if byHarness >= 0 {
		req.Health.FreeCapacity.ByHarness = map[string]int{s.opts.Harness: byHarness}
	}
	if !s.sentDoc {
		doc := s.doc()
		req.Capabilities = &doc
	}
	return req
}

// sync sends one sync and records what the hub said. The answer is returned
// whatever its status: which statuses are a finding is each check's business.
func (s *session) sync(ctx context.Context, free int) (v1.SyncResponse, *answer, error) {
	return s.syncWith(ctx, s.syncRequest(free), nil)
}

// syncWith sends a sync body a check built, or raw bytes where the check is
// about a field the protocol's own types cannot carry.
func (s *session) syncWith(ctx context.Context, req v1.SyncRequest, raw []byte) (v1.SyncResponse, *answer, error) {
	a, err := s.c.do(ctx, call{path: s.syncPath(), bearer: s.cred, body: req, raw: raw})
	if err != nil {
		return v1.SyncResponse{}, nil, err
	}
	var res v1.SyncResponse
	if !a.ok() {
		return res, a, nil
	}
	s.sentDoc = true
	if err := a.decode(&res); err != nil {
		return res, a, err
	}
	s.syncs = append(s.syncs, syncSeen{call: a.Call, free: req.Health.FreeCapacity.Total, res: res})
	// Every run the hub offers is recorded here, at the one place every sync
	// answer passes through: a run offered by any sync is a run a runner would
	// have had to take or refuse, and the rules about offers are about all of
	// them rather than about the two this suite goes on to use.
	for _, r := range res.Runs {
		s.offered[r.RunID] = r
		s.offers = append(s.offers, r)
		// A grant's value is a secret this suite now holds, and a hub that
		// quotes one back anywhere — in an error, in another run — must not
		// have it printed. Redacting it by the field it arrived in covers
		// only the field it arrived in.
		for _, g := range r.Grants {
			s.c.learn(g.Value)
		}
	}
	s.offeredAtOnce = max(s.offeredAtOnce, len(res.Runs))
	s.note(a.Call, res.NextSyncMS, res.LeaseMS)
	return res, a, nil
}

// syncCappedOK sends a sync declaring free capacity for the suite's harness
// as well as in total, and fails the check when the hub does not answer it.
func (s *session) syncCappedOK(ctx context.Context, free, byHarness int) (v1.SyncResponse, *answer, error) {
	res, a, err := s.syncWith(ctx, s.syncRequestCapped(free, byHarness), nil)
	switch {
	case err != nil:
		return res, a, err
	case !a.ok():
		return res, a, brokenf("the sync was refused: %s", a)
	}
	return res, a, nil
}

// syncOK sends a sync the check needs to have succeeded.
func (s *session) syncOK(ctx context.Context, free int) (v1.SyncResponse, *answer, error) {
	res, a, err := s.sync(ctx, free)
	if err != nil {
		return res, a, err
	}
	if !a.ok() {
		return res, a, brokenf("the sync was refused: %s", a)
	}
	return res, a, nil
}

func (s *session) syncPath() string {
	return "/runners/" + url.PathEscape(s.runner) + "/sync"
}

// note records the timings an answer named, for the check that judges them.
func (s *session) note(call string, intervalMS, leaseMS int) {
	s.interval = time.Duration(intervalMS) * time.Millisecond
	s.lease = time.Duration(leaseMS) * time.Millisecond
	s.timings = append(s.timings, timing{call: call, interval: s.interval, lease: s.lease})
}

// hold adds a run to what every later sync lists; drop stops listing one.
func (s *session) hold(runID string, state v1.RunState) {
	for i, r := range s.held {
		if r.RunID == runID {
			s.held[i].State = state
			return
		}
	}
	s.held = append(s.held, v1.HeldRun{RunID: runID, State: state})
}

func (s *session) drop(runID string) {
	s.held = slices.DeleteFunc(s.held, func(r v1.HeldRun) bool { return r.RunID == runID })
}

// pick chooses what the runs the hub offered are used for: the first carries
// the event and result rules, and a second — when the hub offered one — is
// left unlisted for its lease to lapse. The ids must be distinct; one run
// taken for both would be reported terminal and then waited on for a lease.
func (s *session) pick(offered []string) {
	for _, id := range offered {
		switch {
		case s.report == "":
			s.report = id
		case s.lapse == "" && id != s.report:
			s.lapse = id
		}
	}
}

// claimable is the runs the suite took, in the order it took them.
func (s *session) claimable() []string {
	ids := []string{}
	for _, id := range []string{s.report, s.lapse} {
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// newID is a fresh identifier for this run of the suite, so two suites against
// one hub never collide and a re-run never meets its own leftovers.
func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate an id for this runner: %w", err)
	}
	return hex.EncodeToString(b), nil
}
