package conformance

import (
	"context"
	"encoding/json"
	"fmt"
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
	res, a, err := s.syncOK(ctx, 0)
	if err != nil {
		return err
	}
	if len(res.Runs) > 0 {
		return brokenf("the sync declared no free capacity and the hub offered %d run(s), %s: %s", len(res.Runs), strings.Join(runIDs(res.Runs), ", "), a)
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
	var offered []v1.Run
	for i := 0; i < offerSyncs && len(offered) == 0; i++ {
		res, _, err := s.syncOK(ctx, runsWanted)
		if err != nil {
			return err
		}
		offered = res.Runs
	}
	if len(offered) == 0 {
		return skipf("the hub offered no run for harness %s in %d syncs, so the rules that need one could not be checked; queue one or two runs for that harness and run the suite again",
			s.opts.Harness, offerSyncs)
	}
	// What the runs are used for is settled here rather than after the rule
	// below, so a hub that breaks this one is still checked against the rest.
	for _, r := range offered {
		s.offered[r.RunID] = r
	}
	want := runIDs(offered)
	s.pick(want)
	// Nothing is listed in the syncs below, so every run above was never
	// received as far as the hub is concerned, and must come back.
	seen, instead := map[string]bool{}, map[string]bool{}
	var last *answer
	for i := 0; i < offerSyncs && len(seen) < len(want); i++ {
		res, a, err := s.syncOK(ctx, runsWanted)
		if err != nil {
			return err
		}
		last = a
		for _, r := range res.Runs {
			if slices.Contains(want, r.RunID) {
				seen[r.RunID] = true
			} else {
				instead[r.RunID] = true
			}
		}
	}
	if missing := absent(want, seen); len(missing) > 0 {
		note := ""
		if len(instead) > 0 {
			note = fmt.Sprintf(", and it offered %s instead", strings.Join(sorted(instead), ", "))
		}
		// §2 says the hub offers it again and does not say how soon, so the
		// number of syncs waited is this suite's tolerance and not a verdict
		// about the protocol. A hub with a deep queue for this harness can
		// have something else to say first, and the failure says so rather
		// than calling queue depth a broken rule.
		return brokenf("run %s was offered, was not listed in the next sync, and had not been offered again %d syncs later%s; §2 does not say how soon a hub must offer it again, so %d syncs is this suite's tolerance rather than the protocol's number: %s",
			strings.Join(missing, ", "), offerSyncs, note, offerSyncs, last)
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

func checkControlsAreGated(_ context.Context, s *session) error {
	for _, seen := range s.syncs {
		for _, c := range seen.res.Controls {
			switch c.Kind {
			case v1.ControlCancel, v1.ControlReportCapabilities:
			default:
				return brokenf("answering %s the hub sent a %q control, and this runner's capability document advertises no protocol feature; only cancel and report_capabilities go to every v1 runner",
					seen.call, c.Kind)
			}
		}
	}
	return nil
}

// absent is the members of want that seen does not hold, in want's order.
func absent(want []string, seen map[string]bool) []string {
	var missing []string
	for _, id := range want {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

func sorted(set map[string]bool) []string { return slices.Sorted(maps.Keys(set)) }
