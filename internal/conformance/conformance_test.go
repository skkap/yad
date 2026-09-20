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
		// want is on every row on purpose: Passed is Status's zero, so a row
		// that left it out would assert the hub was fine and pass whatever
		// the suite did. Skipped is for a rule that cannot be judged in a
		// bounded number of syncs, and Passed for a hub doing something the
		// suite must not report at all.
		want Status
	}{
		{flaw: flawPlainTextNotFound, check: "errors/unknown-path", want: Failed},
		{flaw: flawIgnoresProtocol, check: "protocol-header/missing", want: Failed},
		{flaw: flawTokenIsReusable, check: "register/token-is-one-time", want: Failed},
		{flaw: flawSendsEmptyLists, check: "sync/empty-lists-omitted", want: Failed},
		{flaw: flawKeepsUnknownFields, check: "sync/unknown-fields-ignored", want: Failed},
		{flaw: flawOffersOverCapacity, check: "sync/free-capacity", want: Failed},
		{flaw: flawNoCancel, check: "sync/cancel-for-a-run-not-held", want: Failed},
		{flaw: flawForgetsOffers, check: "sync/offer-is-repeated", want: Skipped},
		{flaw: flawOffersTwice, check: "sync/claim-by-listing", want: Failed},
		{flaw: flawOffersInvalidRun, check: "run/offer-validates", want: Failed},
		{flaw: flawAckJumpsTheGap, check: "events/acked-through", want: Failed},
		{flaw: flawTakesAnyEvents, check: "events/not-held", want: Failed},
		{flaw: flawTakesAnyResult, check: "result/conflict", want: Failed},
		{flaw: flawShortInterval, check: "sync/timings", want: Failed},
		{flaw: flawUngatedControl, check: "versioning/controls-are-gated", want: Failed},
		{flaw: flawNoNextAction, check: "errors/next-action", want: Failed},
		{flaw: flawRenewsEverything, check: "lease/lapse", leaseWait: time.Minute, want: Failed},
		{flaw: flawAcceptsAnyToken, check: "register/token-required", want: Failed},
		{flaw: flawSameRunnerReuse, check: "register/token-is-one-time", want: Failed},
		{flaw: flawIgnoresHarnessCap, check: "sync/free-capacity", want: Failed},
		{flaw: flawGuardsSyncOnly, check: "protocol-header/missing", want: Failed},
		{flaw: flawGuardsSyncOnly, check: "events/credential-required", want: Failed},
		// The reserved update control is gated on nothing (decision 0018), so
		// the rule about ungated controls must not fire on it.
		{flaw: flawOverOffersWhenBusy, check: "sync/offers-within-capacity", want: Failed},
		{flaw: flawSendsUpdate, check: "versioning/controls-are-gated", want: Passed},
	} {
		t.Run(tc.flaw+"/"+tc.check, func(t *testing.T) {
			t.Parallel()
			_, url := newFake(t, tc.flaw, fakeRunSpec(0), fakeRunSpec(1))
			rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken, LeaseWait: tc.leaseWait})
			if err != nil {
				t.Fatal(err)
			}
			o := outcome(t, rep, tc.check)
			var out strings.Builder
			rep.Print(&out)
			if o.Status != tc.want {
				t.Fatalf("a hub where %s was %s by %s, wanted %s:\n%s", tc.flaw, label(o.Status), tc.check, label(tc.want), out.String())
			}
			if tc.want == Passed {
				return
			}
			// What the answer is worth is these three: the rule in words, the
			// place it is written, and what the hub did instead.
			switch {
			case o.Rule == "":
				t.Error("the answer names no rule")
			case !strings.HasPrefix(o.Section, "ARCHITECTURE.md §2"):
				t.Errorf("the answer points at %q, not a part of ARCHITECTURE.md §2", o.Section)
			case o.Detail == "":
				t.Error("the answer says the rule was not kept and not what the hub did")
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

// A failure prints what the hub answered, and the answer to the rule about a
// spent token is a registration that carries a live runner credential. The
// report is read in a terminal and kept in a pipeline's log, so the credential
// must not be in it (CLAUDE.md, Guardrails).
func TestNoSecretReachesTheReport(t *testing.T) {
	t.Parallel()
	f, url := newFake(t, flawTokenIsReusable, fakeRunSpec(0), fakeRunSpec(1))
	rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken})
	if err != nil {
		t.Fatal(err)
	}
	if o := outcome(t, rep, "register/token-is-one-time"); o.Status != Failed {
		t.Fatalf("the reusable-token hub was %s, so the answer that carries the credential was never printed", label(o.Status))
	}
	var out strings.Builder
	rep.Print(&out)
	f.mu.Lock()
	cred := f.cred
	f.mu.Unlock()
	if cred == "" {
		t.Fatal("the fake hub issued no credential, so this test proves nothing")
	}
	if strings.Contains(out.String(), cred) {
		t.Errorf("the runner credential %q is in the report:\n%s", cred, out.String())
	}
	if !strings.Contains(out.String(), "redacted") {
		t.Errorf("the credential was neither printed nor marked as removed; the report should say what it took out:\n%s", out.String())
	}
}

// A run stopped part way through must not read as a clean one: every check
// after the signal is reported as never made, and the summary says so.
func TestAnInterruptedRunIsNotAPass(t *testing.T) {
	t.Parallel()
	f, url := newFake(t, "", fakeRunSpec(0), fakeRunSpec(1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// From inside the hub, on its fourth request: a deadline here would race
	// with how long the checks before it take.
	f.cancelAfter, f.cancel = 4, cancel

	rep, err := Run(ctx, Options{BaseURL: url, Token: fakeToken})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Interrupted {
		t.Fatal("the report does not say the run was stopped early")
	}
	if got, want := len(rep.Outcomes), len(checks()); got != want {
		t.Errorf("the report lists %d of the suite's %d checks; the ones that never ran are missing from it", got, want)
	}
	var out strings.Builder
	rep.Print(&out)
	flat := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(flat, "stopped before it finished") {
		t.Errorf("the summary does not say the run was stopped:\n%s", out.String())
	}
	if rep.count(Skipped) == 0 {
		t.Error("no check is recorded as not made")
	}
}
