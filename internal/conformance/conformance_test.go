package conformance

import (
	"context"
	"strings"
	"testing"
	"time"
)

const fakeToken = "fake-registration-token"

// A hub that follows §2 passes every check, and skips none: a suite that
// silently skipped half of itself would pass this too, which is why the skips
// are counted as failures here.
func TestAHubThatFollowsTheProtocolPasses(t *testing.T) {
	t.Parallel()
	_, url := newFake(t, "", fakeRunSpec(0), fakeRunSpec(1))
	rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken, LeaseWait: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	rep.Print(&out)
	for _, o := range rep.Outcomes {
		if o.Status != Passed {
			t.Errorf("%s %s: %s", label(o.Status), o.ID, o.Detail)
		}
	}
	if t.Failed() {
		t.Log(out.String())
	}
}

// One broken rule, one named check — and the failure says which rule and where
// it is written, because its reader is someone implementing a hub who does not
// have this repository open.
func TestEachBrokenRuleIsReportedWithItsSection(t *testing.T) {
	// The table's own subtests are parallel and one of them waits a lease
	// out; a serial parent would hold every other parallel test in the
	// package until it had.
	t.Parallel()
	for _, tc := range []struct {
		flaw, check string
		leaseWait   time.Duration
	}{
		{flaw: flawPlainTextNotFound, check: "errors/unknown-path"},
		{flaw: flawIgnoresProtocol, check: "protocol-header/missing"},
		{flaw: flawTokenIsReusable, check: "register/token-is-one-time"},
		{flaw: flawSendsEmptyLists, check: "sync/empty-lists-omitted"},
		{flaw: flawKeepsUnknownFields, check: "sync/unknown-fields-ignored"},
		{flaw: flawOffersOverCapacity, check: "sync/free-capacity"},
		{flaw: flawNoCancel, check: "sync/cancel-for-a-run-not-held"},
		{flaw: flawForgetsOffers, check: "sync/offer-is-repeated"},
		{flaw: flawOffersTwice, check: "sync/claim-by-listing"},
		{flaw: flawOffersInvalidRun, check: "run/offer-validates"},
		{flaw: flawAckJumpsTheGap, check: "events/acked-through"},
		{flaw: flawTakesAnyEvents, check: "events/not-held"},
		{flaw: flawTakesAnyResult, check: "result/conflict"},
		{flaw: flawShortInterval, check: "sync/timings"},
		{flaw: flawUngatedControl, check: "versioning/controls-are-gated"},
		{flaw: flawNoNextAction, check: "errors/next-action"},
		{flaw: flawRenewsEverything, check: "lease/lapse", leaseWait: time.Minute},
	} {
		t.Run(tc.check, func(t *testing.T) {
			t.Parallel()
			_, url := newFake(t, tc.flaw, fakeRunSpec(0), fakeRunSpec(1))
			rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken, LeaseWait: tc.leaseWait})
			if err != nil {
				t.Fatal(err)
			}
			o := outcome(t, rep, tc.check)
			var out strings.Builder
			rep.Print(&out)
			if o.Status != Failed {
				t.Fatalf("a hub where %s was %s by %s, not failed:\n%s", tc.flaw, label(o.Status), tc.check, out.String())
			}
			// What the failure is worth is these three: the rule in words, the
			// place it is written, and what the hub did instead.
			switch {
			case o.Rule == "":
				t.Error("the failure names no rule")
			case !strings.HasPrefix(o.Section, "ARCHITECTURE.md §2"):
				t.Errorf("the failure points at %q, not a part of ARCHITECTURE.md §2", o.Section)
			case o.Detail == "":
				t.Error("the failure says the rule was broken and not what the hub did")
			}
		})
	}
}

// Every check carries the two things a failure is read for, and no two share
// an id — the id is how a run of the suite is compared with the last one.
func TestEveryCheckNamesItsRuleAndSection(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, c := range checks() {
		switch {
		case c.id == "" || seen[c.id]:
			t.Errorf("check %q has no id, or a second check has it", c.id)
		case !strings.HasSuffix(c.rule, "."):
			t.Errorf("%s: the rule is not a sentence: %q", c.id, c.rule)
		case !strings.HasPrefix(c.section, "ARCHITECTURE.md §2"):
			t.Errorf("%s: the section is %q, not a part of ARCHITECTURE.md §2", c.id, c.section)
		}
		seen[c.id] = true
	}
}

// The suite refuses to send a credential where it would go in cleartext, and
// says what it needs when it is given no token.
func TestTheSuiteRefusesWhatItCannotCheckSafely(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, url, token, want string }{
		{name: "plain http to another host", url: "http://hub.example/v1", token: fakeToken, want: "cleartext"},
		{name: "no token", url: "https://hub.example/v1", want: "registration token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Run(context.Background(), Options{BaseURL: tc.url, Token: tc.token})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error is %v, and should say %q", err, tc.want)
			}
		})
	}
}
