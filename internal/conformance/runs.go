package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// The rules of HUB.md §6, Events and results, and of the lease: everything that needs a
// run the hub has offered this runner.

// minPoll is the shortest pause the suite puts between two syncs of its own.
const minPoll = time.Second

// The seqs the event rules are made with: two that are contiguous, one that
// leaves a gap, and the one that fills it.
const (
	seqFirst = 1
	seqAfter = 2
	seqGap   = 4
	seqFill  = 3
	seqLate  = 5
)

func checkAckedThrough(ctx context.Context, s *session) error {
	if err := s.wantAck(ctx, s.report, seqAfter, seqFirst, seqAfter); err != nil {
		return err
	}
	// A batch beyond the gap is held but not acknowledged: acked_through is
	// where the run's events are contiguous, which is what the runner resends
	// from.
	if err := s.wantAck(ctx, s.report, seqAfter, seqGap); err != nil {
		return err
	}
	return s.wantAck(ctx, s.report, seqGap, seqFill)
}

func checkEventsIdempotent(ctx context.Context, s *session) error {
	return s.wantAck(ctx, s.report, seqGap, seqFirst, seqAfter, seqFill, seqGap)
}

// checkEventsUnknownFields is HUB.md's second wire rule on the call a strict
// decoder is likeliest to be generated for: a hub permissive on sync and
// strict on a batch refuses a real runner the moment v1 adds an optional
// event field, which is the break the rule exists to prevent.
func checkEventsUnknownFields(ctx context.Context, s *session) error {
	event, err := withField(v1.Event{
		Seq: seqFirst, At: time.Now().UTC(), Kind: v1.EventText,
		Text: "yad conformance: this run was claimed by the conformance suite, which drives no harness",
	}, "a_field_from_a_later_v1", true)
	if err != nil {
		return err
	}
	batch, err := json.Marshal(map[string]any{
		"events":                  []json.RawMessage{event},
		"a_field_from_a_later_v1": "ignore me",
	})
	if err != nil {
		return err
	}
	a, err := s.c.do(ctx, call{path: eventsPath(s.report), bearer: s.cred, raw: batch})
	if err != nil {
		return err
	}
	if !a.ok() {
		return brokenf("the batch was refused for carrying a field the hub does not know: %s", a)
	}
	return nil
}

// checkResultUnknownFields is the same wire rule on the last call that takes a
// body. It re-sends the terminal state the hub already holds, so a hub that
// takes it changes nothing — what is being asked is whether a field it does
// not know is enough to make it refuse.
func checkResultUnknownFields(ctx context.Context, s *session) error {
	// One field beside the result's own and one inside its error, because a
	// hub decodes a nested object with its own schema and may be strict about
	// only that one.
	runError, err := withField(v1.RunError{Class: "refused", Message: "the yad conformance suite claimed this run to check the protocol and drives no harness"},
		"a_field_from_a_later_v1", true)
	if err != nil {
		return err
	}
	body, err := withField(v1.Result{State: v1.RunFailed, LastSeq: s.lastSeq}, "a_field_from_a_later_v1", true)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return err
	}
	fields["error"] = runError
	body, err = json.Marshal(fields)
	if err != nil {
		return err
	}
	a, err := s.c.do(ctx, call{path: resultPath(s.report), bearer: s.cred, raw: body})
	if err != nil {
		return err
	}
	switch {
	case a.Status == http.StatusConflict:
		// The hub holds a terminal state this run was never given, which is
		// what result/conflict reports. Calling that a refusal over an
		// unknown field would name this rule against a hub keeping it.
		return skipf("the hub answered 409 conflict: it holds a terminal state for run %s other than the one this runner reported, which result/conflict says more about. Until that is fixed, whether it ignores an unknown field here cannot be told", s.report)
	case !a.ok():
		return brokenf("the result was refused for carrying a field the hub does not know: %s", a)
	}
	return nil
}

func checkEventsNotHeld(ctx context.Context, s *session) error {
	id, err := s.strangeRun()
	if err != nil {
		return err
	}
	_, a, err := s.eventsFor(ctx, id, seqFirst)
	if err != nil {
		return err
	}
	return notHolder(a)
}

func checkResultNotHeld(ctx context.Context, s *session) error {
	id, err := s.strangeRun()
	if err != nil {
		return err
	}
	a, err := s.resultFor(ctx, id, v1.RunSucceeded)
	if err != nil {
		return err
	}
	return notHolder(a)
}

// notHolder checks a call for a run this runner does not hold was refused as
// HUB.md fixes it: 403 not_holder, or 404 not_found from a hub that has never heard
// of the run or will not say to this runner that it has. Any other refusal is
// a finding, not a pass. A 5xx or a 401 tells the runner to try again, so it
// resends for ever what the hub will never take, and a 400 says the report
// was malformed when it was the sender that was wrong; none of them shows the
// hub asked who holds the run.
func notHolder(a *answer) error {
	if err := refused(a); err != nil {
		return err
	}
	e, _ := a.envelope()
	switch {
	case a.Status == http.StatusForbidden && e.Code != v1.CodeNotHolder:
		return brokenf("the hub refused it with 403 and the code %q rather than %q, which is the one a runner stops on: %s", e.Code, v1.CodeNotHolder, a)
	case a.Status == http.StatusNotFound && e.Code != v1.CodeNotFound:
		return brokenf("the hub refused it with 404 and the code %q rather than %q: %s", e.Code, v1.CodeNotFound, a)
	case a.Status != http.StatusForbidden && a.Status != http.StatusNotFound:
		return brokenf("the hub refused it with %d, and %s fixes this refusal as 403 %s, or 404 %s for a run the hub will not name to this runner: %s",
			a.Status, hubErrors, v1.CodeNotHolder, v1.CodeNotFound, a)
	}
	return nil
}

func checkResultApplied(ctx context.Context, s *session) error {
	a, err := s.resultFor(ctx, s.report, v1.RunFailed)
	if err != nil {
		return err
	}
	if !a.ok() {
		return brokenf("the hub refused the terminal state of a run it had offered this runner and this runner had claimed: %s", a)
	}
	// The result is acknowledged, so the run stops being listed: what keeps a
	// finished run in the syncs is an unacknowledged result, not the run.
	s.drop(s.report)
	return nil
}

func checkResultIdempotent(ctx context.Context, s *session) error {
	a, err := s.resultFor(ctx, s.report, v1.RunFailed)
	if err != nil {
		return err
	}
	if !a.ok() {
		return brokenf("the same terminal state sent again was refused, which leaves a runner whose first answer was lost retrying for ever: %s", a)
	}
	return nil
}

func checkResultConflict(ctx context.Context, s *session) error {
	a, err := s.resultFor(ctx, s.report, v1.RunSucceeded)
	if err != nil {
		return err
	}
	if a.ok() {
		return brokenf("the hub took a succeeded result for a run it had already recorded as failed; whoever asked for that run has now been told two different things about it: %s", a)
	}
	return refusedWith(a, http.StatusConflict, v1.CodeConflict)
}

// checkEventsSeqFromOne sends an event numbered 0 for a run this runner holds.
// seq is one-based, and a hub storing a zero has a stream that no
// acked_through can describe: the runner's first event would fill a gap that
// does not exist.
func checkEventsSeqFromOne(ctx context.Context, s *session) error {
	_, a, err := s.eventsFor(ctx, s.report, 0)
	if err != nil {
		return err
	}
	if a.ok() {
		return brokenf("an event with seq 0 was taken; a run's events are numbered from 1: %s", a)
	}
	return refusedInvalid(a)
}

// checkResultIsTerminal reports a state that is not terminal as a run's
// result, before the real one: a hub that took it would hold a run as ended
// that is not, and refuse the terminal state that follows as a conflict.
func checkResultIsTerminal(ctx context.Context, s *session) error {
	a, err := s.resultFor(ctx, s.report, v1.RunRunning)
	if err != nil {
		return err
	}
	if a.ok() {
		return brokenf("a result with state %q was taken; a result carries one of the five terminal states, and a non-terminal one travels in the sync: %s", v1.RunRunning, a)
	}
	return refusedInvalid(a)
}

// checkRefusalBeforeClaim is the wider door: a runner that will not take a run
// says so with a failed result, class refused, while the run is only offered
// to it — it never lists the run. The hub must take that result, and must not
// offer the run again: the refusal is the runner saying no retry will change
// its mind. It uses the run the offer-lapse rule left queued, or any other the
// hub offers now besides the two the suite already ended.
func checkRefusalBeforeClaim(ctx context.Context, s *session) error {
	var offer string
	for i := 0; i < offerSyncs && offer == ""; i++ {
		res, _, err := s.syncOK(ctx, 1)
		if err != nil {
			return err
		}
		for _, r := range res.Runs {
			if r.RunID != s.report && r.RunID != s.lapse {
				offer = r.RunID
				break
			}
		}
	}
	switch {
	case offer == "" && s.keepsOffers:
		return skipf("no run was offered in %d syncs: this hub keeps an offer a sync leaves out (sync/unlisted-offer-taken-back), so the run it last offered is still open to this runner and never offered again", offerSyncs)
	case offer == "":
		return skipf("no third run was offered in %d syncs, and this rule needs one besides the two the other rules used; queue three runs for harness %s and run the suite again", offerSyncs, s.opts.Harness)
	}
	a, err := s.resultFor(ctx, offer, v1.RunFailed)
	if err != nil {
		return err
	}
	if !a.ok() {
		return brokenf("run %s was offered to this runner, which reported it failed with class refused without claiming it — how a runner declines a run — and the hub refused the report; it will offer the run again, and hear the same refusal, for as long as it picks this runner: %s", offer, a)
	}
	for i := 0; i < offerSyncs; i++ {
		res, a, err := s.syncOK(ctx, 1)
		if err != nil {
			return err
		}
		if slices.Contains(runIDs(res.Runs), offer) {
			return brokenf("run %s was refused before its claim, the hub acknowledged the refusal, and then offered the run again; a refused run is failed, and offering it again only earns the same refusal: %s", offer, a)
		}
	}
	return nil
}

func checkEventsAfterTheRunEnds(ctx context.Context, s *session) error {
	return s.wantAck(ctx, s.report, seqLate, seqLate)
}

// checkGatedRunsAreNotOffered is the offer side of HUB.md's feature rule: the
// two things in a run that only a runner advertising a feature may be given.
// This runner advertises none, so it must be offered neither.
func checkGatedRunsAreNotOffered(_ context.Context, s *session) error {
	for _, run := range s.offers {
		switch {
		case run.Session.Mode == v1.SessionLive:
			return brokenf("the hub offered run %s in a live session, which goes only to a runner advertising live_sessions; this runner advertises no feature at all", run.RunID)
		case run.StartAt != nil && run.StartAt.After(time.Now()):
			return brokenf("the hub offered run %s with a start_at still ahead (%s), which goes only to a runner advertising start_at — any other starts it on arrival, which is the one thing that moment exists to prevent",
				run.RunID, run.StartAt.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

func checkOfferedRunIsValid(_ context.Context, s *session) error {
	// Every offer, not only the two the suite went on to use and not only the
	// last of each id: a run offered and taken back is one a runner would
	// have had to refuse, and it is exactly as much a mistake as one still
	// open.
	for _, run := range s.offers {
		if err := run.Validate(); err != nil {
			return brokenf("the hub offered run %s, and %s", run.RunID, err)
		}
	}
	return nil
}

func checkLeaseLapse(ctx context.Context, s *session) error {
	lease := s.leaseAtClaim
	if lease <= 0 {
		return skipf("the hub named no lease in the answer that claimed the run, so there is nothing to wait out")
	}
	// The hub's lease is measured on the hub's clock from the sync it last
	// renewed in.
	wait := leaseWait(lease)
	switch {
	case s.opts.LeaseWait <= 0:
		return skipf("no time was budgeted for waiting a lease out; this hub's lease is %s, so run with --lease-wait %s to check it", lease, wait.Round(time.Second))
	case wait > s.opts.LeaseWait:
		return skipf("this hub's lease is %s and waiting it out would take %s, more than the %s budgeted; run with --lease-wait %s to check it",
			lease, wait.Round(time.Second), s.opts.LeaseWait, wait.Round(time.Second))
	}
	deadline := s.lapseAt.Add(wait)
	// Syncing all the while, as a runner does. A sync renews the lease on the
	// runs it lists and on no others, so these must not save the run.
	for time.Now().Before(deadline) {
		// Never faster than minPoll, whatever the hub named. A hub that has
		// not wired next_sync_ms yet — a hub still being built, which is this
		// suite's whole audience — names zero, and an unclamped pause would
		// then be no pause at all: back-to-back syncs for the length of the
		// lease. The rule about what a hub may name is checked separately;
		// this suite must not flood a hub in order to reach it.
		pause := min(max(s.interval, minPoll), time.Until(deadline))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
		if _, _, err := s.syncOK(ctx, 0); err != nil {
			return err
		}
	}
	req := s.syncRequest(0)
	req.Runs = append(req.Runs, v1.HeldRun{RunID: s.lapse, State: v1.RunClaimed})
	res, a, err := s.syncWith(ctx, req, nil)
	if err != nil {
		return err
	}
	if !a.ok() {
		return brokenf("the sync was refused: %s", a)
	}
	// A hub that cancels nothing it does not hold has already failed for
	// that, and its missing cancel here says nothing about the lease: the
	// result below is what shows whether the run was lost.
	if !hasCancel(res.Controls, s.lapse) && !s.noCancels {
		return brokenf("run %s went %s without a sync listing it, past the %s lease the hub itself named, and a sync listing it then was answered with no cancel for it; a run whose lease lapsed is lost, and a runner not told to stop goes on with it: %s",
			s.lapse, wait.Round(time.Second), lease, a)
	}
	ra, err := s.resultFor(ctx, s.lapse, v1.RunSucceeded)
	if err != nil {
		return err
	}
	if ra.ok() {
		return brokenf("run %s lost its lease, and the hub then took a succeeded result for it; a lapsed run is lost, and lost stands: %s", s.lapse, ra)
	}
	return refusedWith(ra, http.StatusConflict, v1.CodeConflict)
}

// checkOfferLapse is the rule for a runner that goes silent holding an offer
// it never claimed: crashed, switched off, cut off. It needs a run of its own
// — the two the other rules use are finished or lost by now — so it takes a
// third one, and is skipped without one.
//
// Silence is the point, so nothing is sent for the whole lease: a sync that
// left the offer out would requeue it by claim-by-listing, and prove nothing
// about the lease. The late claim is what tells the two apart. A hub holding
// the offer for its runner however long it is gone takes that claim; one
// whose offers lapse answers it with a cancel, since by then the run may be
// another runner's.
func checkOfferLapse(ctx context.Context, s *session) error {
	if s.opts.LeaseWait <= 0 {
		return skipf("no time was budgeted for waiting a lease out; this hub's lease is %s, so run with --lease-wait %s to check it", s.lease, leaseWait(s.lease).Round(time.Second))
	}
	if wait := leaseWait(s.lease); wait > s.opts.LeaseWait {
		return skipf("this hub's lease is %s and waiting it out would take %s, more than the %s budgeted; run with --lease-wait %s to check it",
			s.lease, wait.Round(time.Second), s.opts.LeaseWait, wait.Round(time.Second))
	}
	var (
		offer string
		lease time.Duration
	)
	for i := 0; i < offerSyncs && offer == ""; i++ {
		res, _, err := s.syncOK(ctx, 1)
		if err != nil {
			return err
		}
		for _, r := range res.Runs {
			if r.RunID != s.report && r.RunID != s.lapse {
				offer, lease = r.RunID, time.Duration(res.LeaseMS)*time.Millisecond
				break
			}
		}
	}
	offeredAt := time.Now()
	switch {
	case offer == "":
		return skipf("no third run was offered in %d syncs, and this rule needs one besides the two the other rules used; queue three runs for harness %s and run the suite again", offerSyncs, s.opts.Harness)
	case lease <= 0:
		return skipf("the hub named no lease in the answer that offered run %s, so there is nothing to wait out", offer)
	case leaseWait(lease) > s.opts.LeaseWait:
		return skipf("the hub offered run %s with a lease of %s, and waiting it out would take more than the %s budgeted; run with --lease-wait %s to check it",
			offer, lease, s.opts.LeaseWait, leaseWait(lease).Round(time.Second))
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Until(offeredAt.Add(leaseWait(lease)))):
	}

	// No free capacity: the cancel is the whole answer asked for here.
	req := s.syncRequest(0)
	req.Runs = append(req.Runs, v1.HeldRun{RunID: offer, State: v1.RunClaimed})
	res, a, err := s.syncWith(ctx, req, nil)
	if err != nil {
		return err
	}
	if !a.ok() {
		return brokenf("the sync was refused: %s", a)
	}
	switch {
	case !hasCancel(res.Controls, offer) && s.noCancels:
		return skipf("run %s was offered and not claimed for %s, past the %s lease the hub named beside the offer, and a claim listed after that was answered with no cancel; this hub sends no cancel for any run a runner does not hold (sync/cancel-for-a-run-not-held), so whether the offer lapsed cannot be told from here",
			offer, leaseWait(lease).Round(time.Second), lease)
	case !hasCancel(res.Controls, offer):
		return brokenf("run %s was offered and not claimed for %s, past the %s lease the hub named beside the offer, and a claim of it listed then was answered with no cancel, so the hub took it; an offer lapses with its lease, and a claim after that is answered with a cancel, because the run may by then be another runner's: %s",
			offer, leaseWait(lease).Round(time.Second), lease, a)
	}
	for i := 0; i < offerSyncs; i++ {
		res, _, err := s.syncOK(ctx, 1)
		if err != nil {
			return err
		}
		if slices.Contains(runIDs(res.Runs), offer) {
			return nil
		}
		if len(res.Runs) > 0 {
			return skipf("run %s lapsed and its late claim was cancelled, and the hub then had other runs to offer first (%s), so whether it comes back cannot be told in a bounded number of syncs — this is not a verdict on the hub",
				offer, strings.Join(runIDs(res.Runs), ", "))
		}
	}
	// HUB.md names no deadline for the offer after the lapse, as it names none
	// for claim-by-listing's, so this is a warning and not a verdict.
	return skipf("run %s lapsed and its late claim was cancelled, and in %d syncs declaring room for it the hub did not offer it again. A lapsed offer goes back in the queue, and a hub that never offers it again has lost the run — read this as a warning, since HUB.md names no deadline", offer, offerSyncs)
}

// leaseWait is how long the suite waits a lease out. There is no clock in
// common with a hub, so the wait is the whole lease from the moment the
// answer naming it arrived here, and a tenth again.
func leaseWait(lease time.Duration) time.Duration {
	return lease + max(lease/10, time.Second)
}

// wantAck uploads a batch and checks the hub acknowledged up to want.
func (s *session) wantAck(ctx context.Context, runID string, want int64, seqs ...int64) error {
	ack, a, err := s.eventsFor(ctx, runID, seqs...)
	if err != nil {
		return err
	}
	if !a.ok() {
		return brokenf("the hub refused a batch of events %v for a run this runner holds: %s", seqs, a)
	}
	switch {
	case ack.AckedThrough > want:
		return brokenf("after events %v the hub answered acked_through %d, and the events it holds run without a gap only to %d; a runner sends again only what comes after acked_through, so the events in between would never arrive: %s",
			seqs, ack.AckedThrough, want, a)
	case ack.AckedThrough < want:
		return brokenf("after events %v the hub holds every event up to %d and answered acked_through %d; a runner sends again everything after that, so it would send those for ever: %s",
			seqs, want, ack.AckedThrough, a)
	}
	return nil
}

// eventsFor uploads a batch. The events say what they are: whoever reads this
// run on the hub afterwards should not have to work out why a harness
// produced nothing.
func (s *session) eventsFor(ctx context.Context, runID string, seqs ...int64) (v1.EventAck, *answer, error) {
	var batch v1.EventBatch
	for _, seq := range seqs {
		batch.Events = append(batch.Events, v1.Event{
			Seq: seq, At: time.Now().UTC(), Kind: v1.EventText,
			Text: "yad conformance: this run was claimed by the conformance suite, which drives no harness",
		})
		s.lastSeq = max(s.lastSeq, seq)
	}
	a, err := s.c.do(ctx, call{path: eventsPath(runID), bearer: s.cred, body: batch})
	if err != nil {
		return v1.EventAck{}, nil, err
	}
	var ack v1.EventAck
	if a.ok() {
		if err := a.decode(&ack); err != nil {
			return ack, a, err
		}
	}
	return ack, a, nil
}

// resultFor reports a terminal state. A failure is the refusal HUB.md asks of a
// runner that will not take a run, which is exactly what this one is.
func (s *session) resultFor(ctx context.Context, runID string, state v1.RunState) (*answer, error) {
	res := v1.Result{State: state, LastSeq: s.lastSeq}
	if state == v1.RunFailed {
		res.Error = &v1.RunError{
			Class:   "refused",
			Message: "the yad conformance suite claimed this run to check the protocol and drives no harness",
		}
	}
	return s.c.do(ctx, call{path: resultPath(runID), bearer: s.cred, body: res})
}

// The two run-scoped paths, which several checks build by hand.
func eventsPath(runID string) string { return "/runs/" + url.PathEscape(runID) + "/events" }
func resultPath(runID string) string { return "/runs/" + url.PathEscape(runID) + "/result" }

// strangeRun is a run id this hub cannot have offered anyone.
func (s *session) strangeRun() (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	return DefaultHarness + "-never-offered-" + id, nil
}
