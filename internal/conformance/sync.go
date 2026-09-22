package conformance

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// The rules of HUB.md's sync, and the two wire rules of its §2 that a sync
// response is where a hub breaks.

func checkEmptyListsOmitted(ctx context.Context, s *session) error {
	// The first sync carries the capability document — the hub kept no
	// fingerprint from register — and may be answered with a control asking
	// for one. The rule is judged on the next, which expects neither.
	if _, _, err := s.syncOK(ctx, 0); err != nil {
		return err
	}
	res, a, err := s.syncOK(ctx, 0)
	if err != nil {
		return err
	}
	fields, err := a.fields()
	if err != nil {
		return err
	}
	for _, l := range []struct {
		name  string
		empty bool
	}{{"runs", len(res.Runs) == 0}, {"controls", len(res.Controls) == 0}} {
		raw, present := fields[l.name]
		if l.empty && present {
			return brokenf("the answer has no %s and sends %q for them; omit the field instead: %s", l.name, strings.TrimSpace(string(raw)), a)
		}
	}
	return nil
}

func checkUnknownFieldsIgnored(ctx context.Context, s *session) error {
	req := s.syncRequest(0)
	// One field beside the request's own and one inside health, because a hub
	// decodes the two separately and may be strict about only one of them.
	health, err := withField(req.Health, "a_field_from_a_later_v1", true)
	if err != nil {
		return err
	}
	raw, err := s.syncBodyWith(0, map[string]any{
		"a_field_from_a_later_v1": "ignore me",
		"health":                  json.RawMessage(health),
	})
	if err != nil {
		return err
	}
	_, a, err := s.syncWith(ctx, req, raw)
	if err != nil {
		return err
	}
	if !a.ok() {
		return brokenf("the sync was refused for carrying a field the hub does not know: %s", a)
	}
	return nil
}

func checkFreeCapacity(ctx context.Context, s *session) error {
	// Nothing free: a run offered here is one the runner has nowhere to put.
	res, a, err := s.syncOK(ctx, 0)
	if err != nil {
		return err
	}
	if len(res.Runs) > 0 {
		return brokenf("the sync declared no free capacity and the hub offered %d run(s), %s: %s", len(res.Runs), strings.Join(runIDs(res.Runs), ", "), a)
	}
	// Room for two runs but one of this harness. The per-harness figure is
	// the owner's cap, and a run over it must never be claimed and then found
	// to be over it, so it binds the answer exactly as the total does.
	capped, ca, err := s.syncCappedOK(ctx, runsWanted, 1)
	if err != nil {
		return err
	}
	if len(capped.Runs) > 1 {
		return brokenf("the sync declared free capacity for one run of harness %s and the hub offered %d, %s: %s",
			s.opts.Harness, len(capped.Runs), strings.Join(runIDs(capped.Runs), ", "), ca)
	}
	return nil
}

// checkOffersWithinCapacity is the same rule over every sync this run made,
// rather than over a probe written to test it: the syncs that ask for work
// take what they are given, and a hub that over-offers only when it has
// something to offer would pass the probes above.
func checkOffersWithinCapacity(_ context.Context, s *session) error {
	for _, seen := range s.syncs {
		if len(seen.res.Runs) > seen.free {
			return brokenf("answering %s the hub offered %d runs, %s, to a sync that declared free capacity for %d",
				seen.call, len(seen.res.Runs), strings.Join(runIDs(seen.res.Runs), ", "), seen.free)
		}
	}
	return nil
}

func checkCancelForRunNotHeld(ctx context.Context, s *session) error {
	notOurs, err := s.strangeRun()
	if err != nil {
		return err
	}
	req := s.syncRequest(0)
	req.Runs = append(req.Runs, v1.HeldRun{RunID: notOurs, State: v1.RunClaimed})
	res, a, err := s.syncWith(ctx, req, nil)
	if err != nil {
		return err
	}
	if !a.ok() {
		return brokenf("the sync was refused: %s", a)
	}
	if !hasCancel(res.Controls, notOurs) {
		// The rules after this one read a missing cancel as a hub still
		// holding a run for this runner. On this hub it says only that it
		// sends none, which is this failure and not theirs.
		s.noCancels = true
		return brokenf("run %s was listed as held by a runner it was never offered to, and the answer carries no cancel for it: %s", notOurs, a)
	}
	return nil
}

// checkReportCapabilities moves the fingerprint and sends no document, as a
// runner does whose document changed and whose sync carrying it was lost. The
// hub's copy no longer describes the runner, and the control is the only way
// it gets the new one. The next sync carries the document, as a runner asked
// for it does, so the checks after this one meet a hub that is up to date.
func checkReportCapabilities(ctx context.Context, s *session) error {
	fp, err := newID()
	if err != nil {
		return err
	}
	s.fingerprint = fp
	res, a, err := s.syncOK(ctx, 0)
	if err != nil {
		return err
	}
	s.sentDoc = false
	if !slices.ContainsFunc(res.Controls, func(c v1.Control) bool { return c.Kind == v1.ControlReportCapabilities }) {
		return brokenf("a sync whose fingerprint differs from the one that came with the document the hub holds, and which carries no document, was answered with no report_capabilities control; the hub goes on routing by a document it knows is out of date, and nothing else will make the runner send the new one: %s", a)
	}
	_, _, err = s.syncOK(ctx, 0)
	return err
}

// checkUnlistedOfferTakenBack is HUB.md's step 6 as a runner sees it. The
// hub offers runs; the next sync lists none of them and has no room, so the
// hub can neither keep them nor offer them back in that answer; and the sync
// after that lists them as claimed. By then each is back in the queue, not
// offered to this runner, and a listing of a run the runner does not hold is
// answered with a cancel. A hub that kept the offers open takes the late
// claim instead — the one thing a runner can see of offers stranded until
// their lease lapses.
//
// Whether the hub then offers them again is its own business and not judged:
// it may have other runners, or other runs first. The runs the later checks
// use are the ones it offers when this runner asks for work next.
func checkUnlistedOfferTakenBack(ctx context.Context, s *session) error {
	var offered []string
	for i := 0; i < offerSyncs && len(offered) == 0; i++ {
		res, _, err := s.syncOK(ctx, runsWanted)
		if err != nil {
			return err
		}
		// A hub that names one run twice in one answer has offered it once.
		offered = sorted(setOf(runIDs(res.Runs)))
	}
	if len(offered) == 0 {
		return skipf("the hub offered no run for harness %s in %d syncs, so the rules that need one could not be checked; queue three runs for that harness and run the suite again",
			s.opts.Harness, offerSyncs)
	}
	if _, _, err := s.syncOK(ctx, 0); err != nil {
		return err
	}
	for _, id := range offered {
		s.hold(id, v1.RunClaimed)
	}
	res, a, err := s.syncOK(ctx, 0)
	if err != nil {
		return err
	}
	var kept []string
	for _, id := range offered {
		if !hasCancel(res.Controls, id) {
			kept = append(kept, id)
		}
	}
	if len(kept) > 0 && !s.noCancels {
		// The hub has just taken these as claimed, so they are this runner's
		// and the checks after this one use them: a report on a hub that
		// strands offers is still a report on everything else.
		for _, id := range offered {
			if !slices.Contains(kept, id) {
				s.drop(id)
			}
		}
		s.pick(kept)
		s.keepsOffers = true
		return brokenf("offered run %s: the next sync did not list it, and a sync listing it after that was answered with no cancel, so the hub had kept the offer open. An offer a sync leaves out was never received and goes back in the queue at that sync, or it waits out its lease on a runner that does not have it: %s",
			strings.Join(kept, ", "), a)
	}
	for _, id := range offered {
		s.drop(id)
	}
	res, _, err = s.syncOK(ctx, runsWanted)
	if err != nil {
		return err
	}
	s.pick(runIDs(res.Runs))
	if len(kept) > 0 {
		return skipf("run %s was left out of a sync and then listed as claimed, and the hub answered with no cancel for it; it sends no cancel for any run a runner does not hold (sync/cancel-for-a-run-not-held), so whether it had taken the offer back cannot be told from here",
			strings.Join(kept, ", "))
	}
	return nil
}

func checkClaimByListing(ctx context.Context, s *session) error {
	for _, id := range s.claimable() {
		s.hold(id, v1.RunClaimed)
	}
	// Free capacity is declared so the hub *could* offer these runs again:
	// with none declared the rule would pass without being tested.
	res, a, err := s.syncOK(ctx, runsWanted)
	if err != nil {
		return err
	}
	claimed := s.claimable()
	for _, id := range runIDs(res.Runs) {
		if slices.Contains(claimed, id) {
			return brokenf("run %s was listed by the runner that had just been offered it, and the hub offered it again: %s", id, a)
		}
	}
	for _, id := range claimed {
		if hasCancel(res.Controls, id) {
			return brokenf("run %s was listed by the runner the hub had offered it to, and the hub answered with a cancel for it: %s", id, a)
		}
	}
	// From here the second run is never listed again: its lease is what the
	// lapse rule waits out, and this answer is the last renewal it gets.
	if s.lapse != "" {
		s.drop(s.lapse)
		s.lapseAt, s.leaseAtClaim = time.Now(), time.Duration(res.LeaseMS)*time.Millisecond
	}
	// Still claimed, never running: this runner prepares no workdir and spawns
	// no harness, and HUB.md has a run report running only once its workdir exists.
	return nil
}

func checkTimings(_ context.Context, s *session) error {
	for _, t := range s.timings {
		if t.interval < v1MinInterval || t.interval > v1MaxInterval {
			return brokenf("answering %s the hub named a sync interval of %s, outside the %s to %s the protocol bounds it to",
				t.call, t.interval, v1MinInterval, v1MaxInterval)
		}
	}
	return nil
}

// checkLeaseOutlastsTheInterval is its own check rather than a second arm of
// the one above, so that the failure points at the sentence the rule is
// written in rather than at the section next to it.
func checkLeaseOutlastsTheInterval(_ context.Context, s *session) error {
	for _, t := range s.timings {
		if t.lease < t.interval {
			return brokenf("answering %s the hub named a lease of %s beside a sync interval of %s, so a runner syncing exactly when it was asked to would find its runs taken back — the lease lapses before the sync that would renew it",
				t.call, t.lease, t.interval)
		}
	}
	return nil
}

// The interval a hub may name (HUB.md §5). The default of 15 s is the
// hub's business; the bounds are the protocol's.
const (
	v1MinInterval = 5 * time.Second
	v1MaxInterval = 60 * time.Second
)

// gated is the controls HUB.md §7 says a hub sends only to a runner that advertised
// the feature by name. The rest are not gated: cancel and report_capabilities
// go to every v1 runner, and update is reserved — a runner that does not
// implement self-update ignores it (decision 0018), so a hub sending one has
// broken no rule of HUB.md's.
var gated = []v1.ControlKind{v1.ControlDrain, v1.ControlCloseSession, v1.ControlSteer, v1.ControlInterrupt}

func checkControlsAreGated(_ context.Context, s *session) error {
	for _, seen := range s.syncs {
		for _, c := range seen.res.Controls {
			if slices.Contains(gated, c.Kind) {
				return brokenf("answering %s the hub sent a %q control, which goes only to a runner whose capability document advertises %q; this one advertises no protocol feature at all",
					seen.call, c.Kind, c.Kind)
			}
		}
	}
	return nil
}

func sorted(set map[string]bool) []string { return slices.Sorted(maps.Keys(set)) }

func setOf(ids []string) map[string]bool {
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	return set
}
