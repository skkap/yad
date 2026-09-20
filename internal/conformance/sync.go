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

// The rules of §2's "Sync", and the two wire rules under "Calls" that a sync
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
		return brokenf("run %s was listed as held by a runner it was never offered to, and the answer carries no cancel for it: %s", notOurs, a)
	}
	return nil
}

func checkOfferIsRepeated(ctx context.Context, s *session) error {
	// Ask for work, listing nothing each time: every run offered is therefore
	// one the next sync did not list, which is the state the rule is about.
	var (
		answers  [][]string // the runs each answer offered, in order, without repeats within one
		seen     []string   // every run offered so far, so a repeat is one from an earlier answer
		repeated bool
		last     *answer
	)
	for i := 0; i < offerSyncs; i++ {
		res, a, err := s.syncOK(ctx, runsWanted)
		if err != nil {
			return err
		}
		last = a
		// A hub that names one run twice in one answer has offered it once;
		// counting the second as a repeat would report the rule kept without
		// a sync ever having dropped it.
		ids := sorted(setOf(runIDs(res.Runs)))
		for _, r := range res.Runs {
			s.offered[r.RunID] = r
		}
		for _, id := range ids {
			if slices.Contains(seen, id) {
				repeated = true
				continue
			}
			seen = append(seen, id)
		}
		answers = append(answers, ids)
		s.offeredAtOnce = max(s.offeredAtOnce, len(ids))
		if repeated && len(ids) >= runsWanted {
			break
		}
	}
	// The runs later checks use are the ones the hub offered last. An id from
	// an earlier answer is one a sync did not list, so by this very rule the
	// hub has taken it back, and claiming it would be this suite's mistake.
	s.pick(answers[len(answers)-1])

	first := slices.IndexFunc(answers, func(ids []string) bool { return len(ids) > 0 })
	switch {
	case first < 0:
		return skipf("the hub offered no run for harness %s in %d syncs, so the rules that need one could not be checked; queue one or two runs for that harness and run the suite again",
			s.opts.Harness, offerSyncs)
	case repeated:
		return nil
	}
	// What the hub said after the answer that first offered something: how
	// many syncs followed it, and which runs it had to offer that were not
	// the ones it had just dropped.
	dropped, after := answers[first], answers[first+1:]
	var others []string
	for _, ids := range after {
		for _, id := range ids {
			if !slices.Contains(dropped, id) {
				others = append(others, id)
			}
		}
	}
	switch {
	case len(after) == 0:
		return skipf("run %s was offered on the last of this suite's %d syncs, so no sync followed it and whether the hub offers it again could not be seen. Run the suite again against a hub with a run already queued for harness %s.",
			strings.Join(dropped, ", "), offerSyncs, s.opts.Harness)
	case len(others) > 0:
		return skipf("run %s was offered and not listed, and in the %d sync(s) after it the hub had other runs to offer first (%s), so whether it comes back cannot be told in a bounded number of syncs — §2 names no deadline, and this is not a verdict on the hub. Queue only the runs this suite should use, and run it again: %s",
			strings.Join(dropped, ", "), len(after), strings.Join(sorted(setOf(others)), ", "), last)
	default:
		return skipf("run %s was offered and not listed, and the hub offered nothing at all in the %d sync(s) after it though this runner declared room for %d. §2 says the hub offers such a run again but names no deadline, so this is not a verdict — read it as a warning instead: a hub that never offers a dropped run again loses every run it drops. %s",
			strings.Join(dropped, ", "), len(after), runsWanted, last)
	}
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
	// no harness, and §2 has a run report running only once its workdir exists.
	return nil
}

func checkTimings(_ context.Context, s *session) error {
	for _, t := range s.timings {
		switch {
		case t.interval < v1MinInterval || t.interval > v1MaxInterval:
			return brokenf("answering %s the hub named a sync interval of %s, outside the %s to %s the protocol bounds it to",
				t.call, t.interval, v1MinInterval, v1MaxInterval)
		case t.lease < t.interval:
			return brokenf("answering %s the hub named a lease of %s and a sync interval of %s, so a run would be lost between two syncs that were both on time",
				t.call, t.lease, t.interval)
		}
	}
	return nil
}

// The interval a hub may name (§2, Sync — Timings). The default of 15 s is the
// hub's business; the bounds are the protocol's.
const (
	v1MinInterval = 5 * time.Second
	v1MaxInterval = 60 * time.Second
)

// gated is the controls §2 says a hub sends only to a runner that advertised
// the feature by name. The rest are not gated: cancel and report_capabilities
// go to every v1 runner, and update is reserved — a runner that does not
// implement self-update ignores it (decision 0018), so a hub sending one has
// broken no rule of §2's.
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
