package conformance

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
)

const fakeToken = "fake-registration-token"

// A hub that follows HUB.md passes every check, and skips none: a suite that
// silently skipped half of itself would pass this too, which is why the skips
// are counted as failures here. Three runs: two for the claim, event, result
// and lease rules, and one to leave offered and unclaimed for its lease.
func TestAHubThatFollowsTheProtocolPasses(t *testing.T) {
	t.Parallel()
	f, url := newFake(t, "", fakeRunSpec(0), fakeRunSpec(1), fakeRunSpec(2))
	rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken, SecondToken: fakeSecondToken, LeaseWait: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	// A result names the last event its run had. The suite sends events for
	// one run alone, and a last_seq a run never reached leaves its stream
	// incomplete for good on a hub that waits for acked_through to meet it.
	f.mu.Lock()
	for id, run := range f.runs {
		if run.final != "" && run.lastSeq > run.through {
			t.Errorf("run %s ended with last_seq %d, and the suite sent its events only through %d", id, run.lastSeq, run.through)
		}
	}
	f.mu.Unlock()
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
		// second passes the fake's second registration token, which only
		// the holder rules spend.
		second bool
		// third queues the run lease/offer-lapse leaves unclaimed.
		third bool
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
		{flaw: flawStrictDashboardHealth, check: "sync/dashboard-health-optional", want: Failed},
		{flaw: flawOffersOverCapacity, check: "sync/free-capacity", want: Failed},
		{flaw: flawNoCancel, check: "sync/cancel-for-a-run-not-held", want: Failed},
		// A hub keeping the offers a sync left out takes a late claim of
		// them, which a hub that took them back answers with a cancel.
		{flaw: flawForgetsOffers, check: "sync/unlisted-offer-taken-back", want: Failed},
		{flaw: flawOffersTwice, check: "sync/claim-by-listing", want: Failed},
		{flaw: flawOffersInvalidRun, check: "run/offer-validates", want: Failed},
		{flaw: flawAckJumpsTheGap, check: "events/acked-through", want: Failed},
		{flaw: flawTakesAnyEvents, check: "events/not-held", want: Failed},
		{flaw: flawTakesAnyResult, check: "result/conflict", want: Failed},
		{flaw: flawShortInterval, check: "sync/timings", want: Failed},
		{flaw: flawShortLease, check: "sync/lease-outlasts-the-interval", want: Failed},
		{flaw: flawStrictResultFields, check: "result/unknown-fields-ignored", want: Failed},
		{flaw: flawOffersLiveMode, check: "run/gated-features", want: Failed},
		{flaw: flawUngatedControl, check: "versioning/controls-are-gated", want: Failed},
		{flaw: flawNoNextAction, check: "errors/next-action", want: Failed},
		{flaw: flawRenewsEverything, check: "lease/lapse", leaseWait: time.Minute, want: Failed},
		{flaw: flawOffersNeverLapse, check: "lease/offer-lapse", leaseWait: time.Minute, third: true, want: Failed},
		{flaw: flawAcceptsAnyToken, check: "register/token-required", want: Failed},
		{flaw: flawSameRunnerReuse, check: "register/token-is-one-time", want: Failed},
		{flaw: flawIgnoresHarnessCap, check: "sync/free-capacity", want: Failed},
		{flaw: flawGuardsSyncOnly, check: "protocol-header/missing", want: Failed},
		{flaw: flawGuardsSyncOnly, check: "events/credential-required", want: Failed},
		// The reserved update control is gated on nothing (decision 0018), so
		// the rule about ungated controls must not fire on it.
		{flaw: flawOverOffersWhenBusy, check: "sync/offers-within-capacity", want: Failed},
		{flaw: flawResultUnguarded, check: "protocol-header/missing", want: Failed},
		{flaw: flawResultUnguarded, check: "result/credential-required", want: Failed},
		{flaw: flawTakesNoBearer, check: "sync/credential-required", want: Failed},
		// One answer naming a run twice, and the offer never taken back.
		{flaw: flawOffersTwiceOver, check: "sync/unlisted-offer-taken-back", want: Failed},
		{flaw: flawStrictEventFields, check: "events/unknown-fields-ignored", want: Failed},
		{flaw: flawCredentialMisnamed, check: "register/exchange", want: Failed},
		{flaw: flawKeepsOneOffer, check: "sync/unlisted-offer-taken-back", want: Failed},
		// A hub setting a version floor is exercising a right HUB.md grants it,
		// so the suite says what happened and checks nothing further.
		{flaw: flawVersionFloorQuotes, check: "register/exchange", want: Skipped},
		{flaw: flawSendsUpdate, check: "versioning/controls-are-gated", want: Passed},
		// The near half passes on this hub — it refuses a run it has never
		// heard of — so only a second runner shows it is not asking who
		// holds the run.
		{flaw: flawNoHolderCheck, check: "events/not-held", second: true, want: Passed},
		{flaw: flawNoHolderCheck, check: "result/not-held", second: true, want: Passed},
		{flaw: flawNoHolderCheck, check: "events/held-by-another", second: true, want: Failed},
		{flaw: flawNoHolderCheck, check: "result/held-by-another", second: true, want: Failed},
		// A hub taking events from anyone for any run fails both halves.
		{flaw: flawTakesAnyEvents, check: "events/held-by-another", second: true, want: Failed},
		// Refused, in the envelope, and still wrong: a 500 sends the runner
		// back to retry what the hub will never take.
		{flaw: flawNonHolderGets500, check: "events/not-held", want: Failed},
		{flaw: flawNonHolderGets500, check: "result/not-held", want: Failed},
		{flaw: flawNonHolderGets500, check: "events/held-by-another", second: true, want: Failed},
		{flaw: flawNonHolderGets500, check: "result/held-by-another", second: true, want: Failed},
		// Refused correctly and stored anyway: the holder's own report is
		// what finds it, so the second runner's state must differ from it.
		{flaw: flawStoresRefusedResult, check: "result/applied", second: true, want: Failed},
		// A hub that sends no cancel for a run it does not hold fails for
		// that once. The rules that read a missing cancel as a run still
		// held say they cannot tell, or judge by what else they see, rather
		// than blame the lease for it (DEV-110).
		{flaw: flawNoCancel, check: "sync/unlisted-offer-taken-back", want: Skipped},
		{flaw: flawNoCancel, check: "lease/lapse", leaseWait: time.Minute, want: Passed},
		{flaw: flawNoCancel, check: "lease/offer-lapse", leaseWait: time.Minute, third: true, want: Skipped},
		{flaw: flawIgnoresBodyRunner, check: "sync/runner-id-matches-the-path", want: Failed},
		{flaw: flawAnyCredentialSyncs, check: "sync/another-runners-credential", second: true, want: Failed},
		{flaw: flawNoReportCaps, check: "sync/report-capabilities", want: Failed},
		{flaw: flawInvalidIs500, check: "errors/invalid-body", want: Failed},
		{flaw: flawTakesSeqZero, check: "events/seq-from-one", want: Failed},
		{flaw: flawTakesAnyState, check: "result/terminal-state-only", want: Failed},
		{flaw: flawTooLargeIsInvalid, check: "errors/too-large", want: Failed},
		{flaw: flawRefusalNeedsAClaim, check: "result/refusal-before-claim", third: true, want: Failed},
		{flaw: flawRefusalOutlivesTheOffer, check: "result/refusal-after-offer-taken-back", third: true, want: Failed},
		// The run the late refusal ended is the one the next rule needed, and
		// it says so rather than asking for a run to be queued.
		{flaw: flawRefusalOutlivesTheOffer, check: "result/refusal-before-claim", third: true, want: Skipped},
		{flaw: flawReoffersRefused, check: "result/refusal-before-claim", third: true, want: Failed},
	} {
		t.Run(tc.flaw+"/"+tc.check, func(t *testing.T) {
			t.Parallel()
			queued := []v1.Run{fakeRunSpec(0), fakeRunSpec(1)}
			if tc.third {
				queued = append(queued, fakeRunSpec(2))
			}
			_, url := newFake(t, tc.flaw, queued...)
			opts := Options{BaseURL: url, Token: fakeToken, LeaseWait: tc.leaseWait}
			if tc.second {
				opts.SecondToken = fakeSecondToken
			}
			rep, err := Run(context.Background(), opts)
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
			case !strings.HasPrefix(o.Section, "HUB.md §"):
				t.Errorf("the answer points at %q, not a section of HUB.md", o.Section)
			case o.Detail == "":
				t.Error("the answer says the rule was not kept and not what the hub did")
			}
		})
	}
}

// The rule about a lapsed offer needs a run of its own and a lease's worth of
// waiting, and a hub given neither has it skipped, saying which to supply —
// never passed.
func TestTheOfferLapseRuleSaysWhatItNeeds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		queued    []v1.Run
		leaseWait time.Duration
		want      string
	}{
		{"two runs queued", []v1.Run{fakeRunSpec(0), fakeRunSpec(1)}, time.Minute, "queue three runs"},
		{"no time budgeted", []v1.Run{fakeRunSpec(0), fakeRunSpec(1), fakeRunSpec(2)}, 0, "--lease-wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, url := newFake(t, "", tc.queued...)
			rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken, LeaseWait: tc.leaseWait})
			if err != nil {
				t.Fatal(err)
			}
			if o := outcome(t, rep, "lease/offer-lapse"); o.Status != Skipped || !strings.Contains(o.Detail, tc.want) {
				t.Errorf("%s %q, want a skip naming %q", label(o.Status), o.Detail, tc.want)
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
		case c.section == section{}:
			t.Errorf("%s: the rule says nowhere where it is written", c.id)
		}
		seen[c.id] = true
	}
}

// Every section a failure or the not-checked list cites is a heading HUB.md
// has, under the number it is cited by, so renaming or renumbering one there
// fails here instead of leaving a hub's author looking for a section that is
// gone.
func TestEveryCitedSectionIsInHubMD(t *testing.T) {
	t.Parallel()
	headings := hubHeadings(t)
	cited := map[section][]string{}
	for _, c := range checks() {
		cited[c.section] = append(cited[c.section], c.id)
	}
	for _, u := range unchecked {
		if len(u.sections) == 0 {
			t.Errorf("not checked: %s says nowhere where it is written", u.rule)
		}
		for _, sec := range u.sections {
			cited[sec] = append(cited[sec], "not checked: "+u.rule)
		}
	}
	for s, by := range cited {
		title, ok := hubSections[s.n]
		switch {
		case !ok:
			t.Errorf("%s cites §%d, which hubSections gives no heading", by[0], s.n)
		case !slices.Contains(headings[s.n].titles, title):
			t.Errorf("%s cites %q, and HUB.md has no heading %q", by[0], s, fmt.Sprintf("## %d. %s", s.n, title))
		case s.sub != "" && !slices.Contains(headings[s.n].subs, s.sub):
			t.Errorf("%s cites %q, and HUB.md §%d has no heading %q", by[0], s, s.n, "### "+s.sub)
		}
	}
}

// hubHeadings reads HUB.md's numbered ## headings, and the ### headings under
// each, by section number.
func hubHeadings(t *testing.T) map[int]struct{ titles, subs []string } {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "HUB.md"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]struct{ titles, subs []string }{}
	n := 0
	inFence := false
	for line := range strings.Lines(string(b)) {
		line = strings.TrimRight(line, "\n")
		// A fenced block holds code and diagrams, where a # is not a heading.
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		switch {
		case inFence:
		case strings.HasPrefix(line, "## "):
			n = 0
			num, title, ok := strings.Cut(strings.TrimPrefix(line, "## "), ". ")
			if v, err := strconv.Atoi(num); ok && err == nil {
				n = v
				h := out[n]
				h.titles = append(h.titles, title)
				out[n] = h
			}
		case strings.HasPrefix(line, "### ") && n != 0:
			h := out[n]
			h.subs = append(h.subs, strings.TrimPrefix(line, "### "))
			out[n] = h
		}
	}
	if len(out) == 0 {
		t.Fatal("HUB.md has no numbered ## headings to cite, so this test reads it wrong")
	}
	return out
}

// The suite refuses to send a credential where it would go in cleartext, and
// says what it needs when it is given no token.
func TestTheSuiteRefusesWhatItCannotCheckSafely(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, url, token, second, want string }{
		{name: "plain http to another host", url: "http://hub.example/v1", token: fakeToken, want: "cleartext"},
		{name: "no token", url: "https://hub.example/v1", want: "registration token"},
		// The first registration would spend it, and the hub's correct
		// refusal of the second would then be reported as its fault.
		{name: "the same token twice", url: "https://hub.example/v1", token: fakeToken, second: fakeToken, want: "--second-token is the same token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Run(context.Background(), Options{BaseURL: tc.url, Token: tc.token, SecondToken: tc.second})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error is %v, and should say %q", err, tc.want)
			}
		})
	}
}

// Without a second token the holder rules are skipped, not passed, and the
// skip names the flag that would make them checkable; the rule is in the
// checked list either way, and no longer in the printed "not checked" one.
// With no run queued as well, the skip still names the flag: the token is the
// one missing piece the operator can see, and a skip that names only the run
// sends them back to queue one and meet a second skip.
func TestWithoutASecondTokenTheHolderRulesSaySo(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		queued []v1.Run
	}{
		{"with runs queued", []v1.Run{fakeRunSpec(0), fakeRunSpec(1)}},
		{"with nothing queued", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, url := newFake(t, "", tc.queued...)
			rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken})
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"events/held-by-another", "result/held-by-another"} {
				if o := outcome(t, rep, id); o.Status != Skipped || !strings.Contains(o.Detail, "--second-token") {
					t.Errorf("%s: %s %q, want a skip naming --second-token", id, label(o.Status), o.Detail)
				}
			}
			var out strings.Builder
			rep.Print(&out)
			if flat := strings.Join(strings.Fields(out.String()), " "); strings.Contains(flat, "while another runner holds it") {
				t.Errorf("the rule is checked and still listed as not checked:\n%s", out.String())
			}
		})
	}
}

// The second runner's token and credential are secrets like the first's: a
// hub that quotes its credential back in a refusal the report prints gets a
// report without it.
func TestTheSecondRunnersSecretsStayOutOfTheReport(t *testing.T) {
	t.Parallel()
	f, url := newFake(t, flawQuotesTheNonHolder, fakeRunSpec(0), fakeRunSpec(1))
	rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken, SecondToken: fakeSecondToken})
	if err != nil {
		t.Fatal(err)
	}
	if o := outcome(t, rep, "errors/next-action"); o.Status != Failed {
		t.Fatalf("errors/next-action was %s, so the refusal quoting the credential was never printed", label(o.Status))
	}
	var out strings.Builder
	rep.Print(&out)
	f.mu.Lock()
	cred := f.cred2
	f.mu.Unlock()
	if cred == "" {
		t.Fatal("the second runner never registered, so this test proves nothing")
	}
	for _, secret := range []string{fakeSecondToken, cred} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("%q is in the report:\n%s", secret, out.String())
		}
	}
}

// A failure prints what the hub answered, and the answer to the rule about a
// spent token is a registration that carries a live runner credential. The
// report is read in a terminal and kept in a pipeline's log, so the credential
// must not be in it (AGENTS.md, Guardrails).
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
	// A register answer that succeeded is described rather than printed at
	// all, since the credential in it may be under a name this suite does not
	// know. The report has to say that is what it did.
	flat := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(flat, "the body is not printed because a register answer carries a credential") {
		t.Errorf("the credential was neither printed nor accounted for; the report should say what it withheld:\n%s", out.String())
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
	// Including the check the signal landed in: a stopped suite must not
	// name a HUB.md rule against a hub that did nothing wrong.
	for _, o := range rep.Outcomes {
		if o.Status == Failed {
			t.Errorf("%s is reported as broken by a hub that only had the suite stopped on it: %s", o.ID, o.Detail)
		}
	}
}

// The redaction has to fail closed. A body that encoding/json refuses — a
// UTF-8 BOM before the brace, a body cut at the read limit — cannot be
// redacted, and the bodies most worth printing are the ones that carry the
// credential.
func TestAnUnreadableBodyIsNotPrinted(t *testing.T) {
	t.Parallel()
	f, url := newFake(t, flawBOMBeforeJSON, fakeRunSpec(0), fakeRunSpec(1))
	rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken})
	if err != nil {
		t.Fatal(err)
	}
	// The register answer is the one that carries the credential, and with a
	// body the suite cannot read it is also the answer a failure prints.
	if o := outcome(t, rep, "register/exchange"); o.Status != Failed {
		t.Fatalf("register/exchange was %s against a hub whose answers do not parse, so the body was never printed", label(o.Status))
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
		t.Errorf("the runner credential %q is in the report, from a body that could not be parsed to redact:\n%s", cred, out.String())
	}
	if !strings.Contains(out.String(), "not JSON") {
		t.Errorf("the report neither printed the body nor said why it did not:\n%s", out.String())
	}
}

// Every run the hub offered is judged, not only the ones the suite went on to
// use: a run offered and taken back is one a runner would have had to refuse.
func TestEveryOfferedRunIsValidated(t *testing.T) {
	t.Parallel()
	// Offered in an earlier answer and taken back, so it is in the offers but
	// not among the runs the suite went on to use.
	takenBack := v1.Run{RunID: "taken-back", Session: v1.SessionRef{ID: "s"}, Harness: DefaultHarness, Brief: v1.Brief{Instruction: "hi"}}
	s := &session{
		opts:    Options{Harness: DefaultHarness},
		c:       newClient("http://hub.example/v1"),
		offered: map[string]v1.Run{takenBack.RunID: takenBack, "kept": fakeRunSpec(0)},
		offers:  []v1.Run{takenBack, fakeRunSpec(0)},
	}
	s.pick([]string{"kept"})
	err := checkOfferedRunIsValid(context.Background(), s)
	if err == nil {
		t.Fatal("a run with no model was offered and taken back, and the check passed")
	}
	if !strings.Contains(err.Error(), "taken-back") {
		t.Errorf("the failure does not name the run: %v", err)
	}
}

// Redacting by field name cannot see a secret a hub puts inside a message.
// The suite holds the token it presented and the credential it was given, so
// neither reaches the report whatever shape the hub wrapped it in.
func TestASecretQuotedBackIsNotPrinted(t *testing.T) {
	t.Parallel()
	_, url := newFake(t, flawEchoesTheToken, fakeRunSpec(0), fakeRunSpec(1))
	rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken})
	if err != nil {
		t.Fatal(err)
	}
	// That refusal names no next action, so it is printed — without which
	// this test would assert only that an answer nobody prints is safe.
	if o := outcome(t, rep, "errors/next-action"); o.Status != Failed {
		t.Fatalf("errors/next-action was %s, so the answer quoting the token was never printed", label(o.Status))
	}
	var out strings.Builder
	rep.Print(&out)
	// The report wraps its prose, so the phrase is matched with the spacing
	// flattened rather than as it happens to have broken.
	if flat := strings.Join(strings.Fields(out.String()), " "); !strings.Contains(flat, "has been used") {
		t.Fatalf("the report does not carry the refusal that quotes the token, so it proves nothing:\n%s", out.String())
	}
	if strings.Contains(out.String(), fakeToken) {
		t.Errorf("the registration token is in the report, quoted inside an error message:\n%s", out.String())
	}
}

// A hub that stops answering is a finding about the hub. Only this suite's own
// context ending means the suite was stopped — and a check that says so while
// nothing failed is a run that exits 0 over a hub that stalled.
func TestAStalledHubIsNotAnInterruption(t *testing.T) {
	t.Parallel()
	s := &session{opts: Options{Harness: DefaultHarness}, c: newClient("http://hub.example/v1"), offered: map[string]v1.Run{}}
	timedOut := check{
		id: "probe", rule: "A rule.", section: hubCalls,
		run: func(context.Context, *session) error {
			return fmt.Errorf("Post \"http://hub.example/v1/runners/x/sync\": context deadline exceeded (Client.Timeout exceeded): %w", context.DeadlineExceeded)
		},
	}
	if got := s.make(context.Background(), timedOut); got.Status != Failed {
		t.Errorf("a hub that did not answer inside the request timeout was %s, not failed: %s", label(got.Status), got.Detail)
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.make(stopped, timedOut); got.Status != Skipped {
		t.Errorf("a check the suite was stopped in was %s, not skipped", label(got.Status))
	}
	// And the other way about: a signal arriving while a hub was already
	// answering wrongly must not relabel the hub's failure as this suite's
	// interruption, which would lose a finding it had already made.
	broke := check{
		id: "probe", rule: "A rule.", section: hubCalls,
		run: func(context.Context, *session) error { return brokenf("the hub answered 500") },
	}
	if got := s.make(stopped, broke); got.Status != Failed {
		t.Errorf("a rule the hub broke was %s once the suite was stopped, not failed: %s", label(got.Status), got.Detail)
	}
}

// Redaction reads the decoded document, so it must reach every decoded place a
// string can be — including an object's keys, and a body that is one string.
func TestASecretIsFoundWhereverAStringCanBe(t *testing.T) {
	t.Parallel()
	const secret = "tok-abcdef-0123456789"
	c := newClient("http://hub.example/v1", secret)
	for _, body := range []string{
		`{"` + secret + ` is not valid":true}`,
		`"the registration token ` + secret + ` has been used"`,
		`{"error":{"message":"the token ` + secret + ` has been used"}}`,
	} {
		text, read := c.redacted([]byte(body))
		switch {
		case !read:
			t.Errorf("%s could not be read", body)
		case strings.Contains(text, secret):
			t.Errorf("the secret survived redaction: %s -> %s", body, text)
		}
	}
}

// A register answer that succeeded is never printed by any check, because the
// credential in it may be under a name this suite has no way to recognise. The
// hub here registers a runner with no bearer at all and names the field its own
// way, so the check that catches it is one that prints the answer it got.
func TestAMisnamedCredentialIsNeverPrinted(t *testing.T) {
	t.Parallel()
	f, url := newFake(t, flawRegistersAnyone, fakeRunSpec(0), fakeRunSpec(1))
	rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken})
	if err != nil {
		t.Fatal(err)
	}
	if o := outcome(t, rep, "register/token-required"); o.Status != Failed {
		t.Fatalf("register/token-required was %s against a hub that registers anyone", label(o.Status))
	}
	var out strings.Builder
	rep.Print(&out)
	f.mu.Lock()
	cred := f.cred
	f.mu.Unlock()
	if cred != "" && strings.Contains(out.String(), cred) {
		t.Errorf("the credential is in the report, under a name the redaction could not know:\n%s", out.String())
	}
}

// A secret a hub wrote with escapes is the same secret. Matching the decoded
// text is what makes that true whatever encoder the hub used — and the suite's
// own re-marshalling escapes what it did not redact, so matching the encoded
// text would leave a second secret beyond reach.
func TestAnEscapedSecretIsStillFound(t *testing.T) {
	t.Parallel()
	// Characters Go's own encoder escapes, which another hub's may not.
	const token = "tok-a&b<c>d-0123456789"
	f, url := newFake(t, flawEchoesTheToken)
	f.token = token
	rep, err := Run(context.Background(), Options{BaseURL: url, Token: token, LeaseWait: 0})
	if err != nil {
		t.Fatal(err)
	}
	if o := outcome(t, rep, "errors/next-action"); o.Status != Failed {
		t.Fatalf("errors/next-action was %s, so the answer quoting the token was never printed", label(o.Status))
	}
	var out strings.Builder
	rep.Print(&out)
	for _, form := range []string{token, encoded(token)} {
		if form != "" && strings.Contains(out.String(), form) {
			t.Errorf("the token is in the report as %q:\n%s", form, out.String())
		}
	}
}

// The fields v1 puts a secret in are redacted whatever the hub put there: a
// hub is not obliged to send a string, and a walk that only replaces strings
// prints an object holding a credential.
func TestASecretIsRedactedWhateverItsShape(t *testing.T) {
	t.Parallel()
	c := newClient("http://hub.example/v1")
	for _, body := range []string{
		`{"runner_credential":{"value":"cred-abcdef"}}`,
		`{"runner_credential":12345678}`,
		`{"runs":[{"grants":[{"name":"TOKEN","as":"env","value":{"inner":"grant-abcdef"}}]}]}`,
	} {
		text, read := c.redacted([]byte(body))
		switch {
		case !read:
			t.Errorf("%s could not be read", body)
		case strings.Contains(text, "abcdef") || strings.Contains(text, "12345678"):
			t.Errorf("the secret survived redaction: %s -> %s", body, text)
		}
	}
	// A password inside a source URL is a secret the hub sent and this suite
	// can never have learned, so it comes out of the URL rather than off a
	// list — and the host and path stay, because a run's source is the thing
	// the reader is trying to see. Asserted here rather than through a report,
	// where the excerpt can cut the URL off and pass for the wrong reason.
	const password = "hunter2-0123456789"
	text, read := c.redacted([]byte(`{"runs":[{"run_id":"r1","sources":[{"git":{"url":"https://runner:` + password + `@git.example/repo.git"}}]}]}`))
	switch {
	case !read:
		t.Error("a run with a git source could not be read")
	case strings.Contains(text, password):
		t.Errorf("the password is still in the source URL: %s", text)
	case !strings.Contains(text, "git.example/repo.git"):
		t.Errorf("the source URL no longer says which repository it was: %s", text)
	}
}

// The escaping a hub chooses is its own, and the set of forms is unbounded —
// \u0074 is a legal encoding of "t". Only reading the body as JSON collapses
// them all, which is why the secrets are taken out of the decoded strings
// rather than searched for in the text.
func TestASecretEscapedBeyondGuessingIsStillFound(t *testing.T) {
	t.Parallel()
	_, url := newFake(t, flawExoticEscape)
	rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken, LeaseWait: 0})
	if err != nil {
		t.Fatal(err)
	}
	if o := outcome(t, rep, "errors/next-action"); o.Status != Failed {
		t.Fatalf("errors/next-action was %s, so the answer quoting the token was never printed", label(o.Status))
	}
	var out strings.Builder
	rep.Print(&out)
	flat := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(flat, "has been used") {
		t.Fatalf("the report does not carry the refusal that quotes the token, so it proves nothing:\n%s", out.String())
	}
	// Both forms: the token as it is, and the form the hub wrote it in. A
	// report carrying \u0066\u0061… has printed the token to anyone who can
	// read four characters of JSON, and asserting only the raw form would
	// pass while it did.
	for _, form := range []string{fakeToken, escapeEvery(fakeToken)} {
		if strings.Contains(out.String(), form) {
			t.Errorf("the token is in the report as %q:\n%s", form, out.String())
		}
	}
}

// A check writes the hub's own strings into its sentence — an error code, a
// run id, a control kind — and each is a place a hub could have put a secret.
// The answer's printing is guarded; this is about everything written around it.
func TestASecretInWhatACheckWritesIsNotPrinted(t *testing.T) {
	t.Parallel()
	_, url := newFake(t, flawTokenInTheCode, fakeRunSpec(0), fakeRunSpec(1))
	rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken})
	if err != nil {
		t.Fatal(err)
	}
	// The credential, not the token: the protocol-header probes are made with
	// the credential as the bearer, and it is the code built from it that the
	// check quotes into its own message.
	o := outcome(t, rep, "protocol-header/missing")
	if o.Status != Failed {
		t.Fatalf("protocol-header/missing was %s, so the code quoting the bearer was never written into a message", label(o.Status))
	}
	var out strings.Builder
	rep.Print(&out)
	if !strings.Contains(o.Detail, "the error code is") {
		t.Fatalf("the failure does not quote the hub's code, so it proves nothing: %s", o.Detail)
	}
	for _, secret := range []string{"fake-credential", fakeToken} {
		if strings.Contains(out.String(), secret) {
			t.Errorf("%q is in the report, quoted inside an error code:\n%s", secret, out.String())
		}
	}
}

// The connection URL is printed at the top of every report, and a person may
// well have been given one with a password in it.
func TestAPasswordInTheURLIsNotPrinted(t *testing.T) {
	t.Parallel()
	rep := &Report{BaseURL: config.RedactURL("https://runner:hunter2@hub.example/v1"), Harness: DefaultHarness}
	var out strings.Builder
	rep.Print(&out)
	if strings.Contains(out.String(), "hunter2") {
		t.Errorf("the password is in the report:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "hub.example/v1") {
		t.Errorf("the report no longer says which hub it ran against:\n%s", out.String())
	}
}

// Secrets the hub sends, rather than ones this suite presented: a grant's
// value, which it learns from the run it was offered, and a password inside a
// source URL, which it can never learn and takes out of the URL instead.
func TestASecretTheHubSentIsNotPrinted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ flaw, secret, printedBy string }{
		{flaw: flawGrantInTheOpen, secret: grantValue, printedBy: "errors/next-action"},
	} {
		t.Run(tc.flaw, func(t *testing.T) {
			t.Parallel()
			_, url := newFake(t, tc.flaw, fakeRunSpec(0), fakeRunSpec(1))
			rep, err := Run(context.Background(), Options{BaseURL: url, Token: fakeToken})
			if err != nil {
				t.Fatal(err)
			}
			// The check that prints the answer the secret arrived in — without
			// it this test would assert that something nobody prints is safe.
			if o := outcome(t, rep, tc.printedBy); o.Status != Failed {
				t.Fatalf("%s was %s, so the answer carrying the secret was never printed", tc.printedBy, label(o.Status))
			}
			var out strings.Builder
			rep.Print(&out)
			if strings.Contains(out.String(), tc.secret) {
				t.Errorf("%q is in the report:\n%s", tc.secret, out.String())
			}
		})
	}
}

// A connection URL with userinfo is refused before the suite starts, so the
// route left for a URL-shaped credential into the report is a hub that
// redirects to one: the refusal quotes the Location, Go's transport error
// quotes it again with only the password starred, and a Location that does not
// parse is quoted whole inside the error net/http builds for it.
func TestACredentialInARedirectIsNotPrinted(t *testing.T) {
	t.Parallel()
	const secret = "sk-secret-token-abc"
	for _, tc := range []struct{ name, location, says string }{
		{"token as username", "http://" + secret + "@HOST/elsewhere", "redirected"},
		{"token as password", "http://runner:" + secret + "@HOST/elsewhere", "redirected"},
		{"unparseable, token as password", "http://runner:" + secret + "@HOST/%zz", "redirected"},
		{"unparseable, token as username", "http://" + secret + "@HOST/%zz", "redirected"},
		{"unparseable host", "http://runner:" + secret + "@[::1/elsewhere", "redirected"},
		// A byte net/textproto refuses in a header value, so the answer fails
		// to parse before net/http ever looks for a redirect in it.
		{"unreadable header", "http://runner:" + secret + "@HOST/\x7f", "the hub's answer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Set by hand: http.Redirect would clean a Location it can parse.
				w.Header().Set("Location", strings.ReplaceAll(tc.location, "HOST", r.Host))
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			t.Cleanup(srv.Close)
			rep, err := Run(context.Background(), Options{BaseURL: srv.URL + "/v1", Token: fakeToken})
			if err != nil {
				t.Fatal(err)
			}
			if o := outcome(t, rep, "errors/unknown-path"); o.Status != Failed {
				t.Fatalf("errors/unknown-path was %s against a hub that only redirects, so no redirect was printed", label(o.Status))
			}
			var out strings.Builder
			rep.Print(&out)
			if strings.Contains(out.String(), secret) {
				t.Errorf("the credential in the redirect is in the report:\n%s", out.String())
			}
			if !strings.Contains(out.String(), tc.says) {
				t.Errorf("the report no longer says %q:\n%s", tc.says, out.String())
			}
		})
	}
}

// The refusal a bad connection URL earns is printed before there is a report
// to redact, and a URL is a place people are handed credentials.
func TestARefusedURLIsQuotedWithoutItsCredentials(t *testing.T) {
	t.Parallel()
	_, err := Run(context.Background(), Options{BaseURL: "http://runner:hunter2@hub.example/v1", Token: fakeToken})
	switch {
	case err == nil:
		t.Fatal("plain http to another host was accepted")
	case strings.Contains(err.Error(), "hunter2"):
		t.Errorf("the password is in the refusal: %v", err)
	case !strings.Contains(err.Error(), "hub.example"):
		t.Errorf("the refusal no longer says which URL it refused: %v", err)
	}
}

// HUB.md §12 says how many checks the suite makes and how many rules it leaves
// unchecked, and a hub author reads those numbers to know whether the report in
// front of them is the whole suite. They are counted here from the suite
// itself, so a check added or a rule moved off the list fails until HUB.md
// says so.
func TestHubMDCountsTheSuite(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile(filepath.Join("..", "..", "HUB.md"))
	if err != nil {
		t.Fatal(err)
	}
	flat := strings.Join(strings.Fields(string(b)), " ")
	checksIn := map[int]string{44: "Forty-four", 45: "Forty-five", 46: "Forty-six", 47: "Forty-seven", 48: "Forty-eight", 49: "Forty-nine", 50: "Fifty"}
	rulesIn := map[int]string{14: "fourteen", 15: "fifteen", 16: "sixteen", 17: "seventeen", 18: "eighteen", 19: "nineteen", 20: "twenty"}
	n, u := len(checks()), len(unchecked)
	if checksIn[n] == "" || rulesIn[u] == "" {
		t.Fatalf("%d checks and %d unchecked rules: add the words for them to this test", n, u)
	}
	for _, want := range []string{
		checksIn[n] + " black-box checks",
		fmt.Sprintf("It ends `%d passed, 0 failed, 0 skipped`", n),
		"it is " + rulesIn[u] + " rules",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("HUB.md §12 does not say %q: the suite has %d checks and %d rules it does not check", want, n, u)
		}
	}
}
