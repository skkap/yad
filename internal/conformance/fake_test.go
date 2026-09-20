package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// fake is a hub of about two hundred lines: enough of §2 for the suite to
// pass it, and a flaw switch for the tests that each break one rule. It is
// here rather than in internal/hub on purpose — a suite checked only against
// the implementation it was written beside proves nothing about the protocol,
// which is the whole reason this package exists.
type fake struct {
	// flaw is the one rule this hub breaks; empty is a hub that follows §2.
	flaw string

	// cancelAfter, with cancel, stops the suite from inside the hub after
	// that many requests, so the rule about an interrupted run is testable
	// without a race over how long a check takes.
	cancelAfter int
	cancel      context.CancelFunc

	mu       sync.Mutex
	requests int
	token    string
	spent    bool
	cred     string
	runner   string
	runs     map[string]*fakeRun
	order    []string
	interval time.Duration
	lease    time.Duration
}

type fakeRun struct {
	spec    v1.Run
	state   v1.RunState
	queued  bool
	holder  string
	expires time.Time
	events  map[int64]bool
	through int64
	final   v1.RunState
}

// The flaws, each named for the rule it breaks.
const (
	flawPlainTextNotFound  = "a path that is no operation answers text/plain"
	flawIgnoresProtocol    = "the Yad-Protocol header is not read"
	flawTokenIsReusable    = "a registration token registers a runner every time"
	flawSendsEmptyLists    = "runs and controls are sent as [] and null"
	flawKeepsUnknownFields = "a sync carrying an unknown field is refused"
	flawOffersOverCapacity = "a run is offered to a sync with no free capacity"
	flawNoCancel           = "a run the runner does not hold is answered with nothing"
	flawForgetsOffers      = "an offered run the next sync did not list is dropped"
	flawOffersTwice        = "a claimed run is offered again"
	flawOffersInvalidRun   = "a run with no model is offered"
	flawAckJumpsTheGap     = "acked_through is the highest seq stored, gap or not"
	flawTakesAnyEvents     = "events are taken for any run from anyone"
	flawTakesAnyResult     = "a second, different terminal state replaces the first"
	flawShortInterval      = "the sync interval named is a second"
	flawUngatedControl     = "a steer goes to a runner that never advertised one"
	flawNoNextAction       = "errors say what went wrong and not what to do"
	flawRenewsEverything   = "every run's lease is renewed, listed or not"
	flawAcceptsAnyToken    = "any bearer is taken as a registration token"
	flawSameRunnerReuse    = "a spent token registers the runner it was spent on, again"
	flawIgnoresHarnessCap  = "the per-harness free capacity is not read"
	flawGuardsSyncOnly     = "the credential and the protocol header are checked on sync alone"
	flawSendsUpdate        = "the reserved update control is sent"
	flawOverOffersWhenBusy = "one run more than the free capacity is offered, whenever there is any"
	flawBOMBeforeJSON      = "every body starts with a UTF-8 BOM, so no answer parses"
	flawResultUnguarded    = "the result call reads neither the credential nor the protocol header"
	flawTakesNoBearer      = "a request with no Authorization header at all is taken"
	// Both halves: the duplicate is what a suite counting it as a repeat
	// would take for the rule being kept, and the forgotten offer is the
	// defect that would then go unreported.
	flawOffersTwiceOver = "one answer names the same run twice, and a dropped offer is never repeated"
	// The missing next action is what makes the refusal reach the report at
	// all: a refusal the suite reads and passes is never printed, so a test
	// over it would prove nothing about what is printed.
	flawEchoesTheToken = "a refusal quotes the token it was given, and names no next action"
	// The floor quotes the token as well, and a version refusal is one §2
	// grants a hub — so the suite reads it, skips, and prints the hub's own
	// words rather than a failure.
	flawVersionFloorQuotes = "the version floor refuses this runner and quotes its token"
	flawCredentialMisnamed = "the credential comes back under a name of the hub's own"
	flawStrictEventFields  = "a batch carrying an unknown field is refused"
	flawKeepsOneOffer      = "of two dropped offers only one is ever offered again"
	// Not Go's escaping and not this suite's guess at one: every character
	// of the token written \uXXXX, which is legal JSON that no search for a
	// form somebody thought of will match.
	flawExoticEscape = "a refusal quotes the token with every character escaped, and names no next action"
)

func newFake(t *testing.T, flaw string, queued ...v1.Run) (*fake, string) {
	t.Helper()
	f := &fake{
		flaw: flaw, token: "fake-registration-token", runs: map[string]*fakeRun{},
		// The shortest pair §2 allows, so a test that waits a lease out waits
		// six seconds rather than a minute.
		interval: 5 * time.Second, lease: 5 * time.Second,
	}
	// One more run than the suite asks for, so the hub that over-offers has
	// something to over-offer with: with exactly as many runs as free
	// capacity, the defect cannot show.
	if flaw == flawOverOffersWhenBusy {
		queued = append(queued, fakeRunSpec(len(queued)))
	}
	for _, r := range queued {
		f.runs[r.RunID] = &fakeRun{spec: r, queued: true, events: map[int64]bool{}}
		f.order = append(f.order, r.RunID)
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv.URL + "/v1"
}

func fakeRunSpec(n int) v1.Run {
	return v1.Run{
		RunID:   fmt.Sprintf("fake-run-%d", n),
		Session: v1.SessionRef{ID: fmt.Sprintf("fake-session-%d", n), New: true, Mode: v1.SessionPerRun},
		Harness: DefaultHarness, Model: "none",
		Brief: v1.Brief{Instruction: "a run for the conformance suite to refuse"},
	}
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests++
	stop := f.cancel != nil && f.requests >= f.cancelAfter
	f.mu.Unlock()
	if stop {
		f.cancel()
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/v1")
	if !ok {
		f.fail(w, http.StatusNotFound, v1.CodeNotFound, "nothing is mounted at "+r.URL.Path, "the connection URL ends in the hub's base")
		return
	}
	if r.Method != http.MethodPost {
		f.fail(w, http.StatusMethodNotAllowed, v1.CodeInvalid, r.Method+" is not how a protocol call is made", "every protocol call is a POST")
		return
	}
	guarded := true
	switch f.flaw {
	case flawGuardsSyncOnly:
		guarded = strings.HasSuffix(path, "/sync")
	case flawResultUnguarded:
		guarded = !strings.HasSuffix(path, "/result")
	}
	if f.flaw != flawIgnoresProtocol && guarded && r.Header.Get(v1.HeaderProtocol) != v1.Version {
		f.fail(w, http.StatusUpgradeRequired, v1.CodeUnsupportedProtocol, "this hub speaks protocol 1", "upgrade yad or the hub")
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch {
	case path == "/runners/register":
		f.register(w, r)
	case len(parts) == 3 && parts[0] == "runners" && parts[2] == "sync":
		f.sync(w, r, parts[1])
	case len(parts) == 3 && parts[0] == "runs" && parts[2] == "events":
		f.events(w, r, parts[1], guarded)
	case len(parts) == 3 && parts[0] == "runs" && parts[2] == "result":
		f.result(w, r, parts[1], guarded)
	case f.flaw == flawPlainTextNotFound:
		http.NotFound(w, r)
	default:
		f.fail(w, http.StatusNotFound, v1.CodeNotFound, "no operation at "+r.URL.Path, "check the path against protocol/v1/openapi.yaml")
	}
}

func (f *fake) register(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req v1.RegisterRequest
	issued := bearer(r) == f.token || f.flaw == flawAcceptsAnyToken && bearer(r) != ""
	if !issued || json.NewDecoder(r.Body).Decode(&req) != nil {
		f.fail(w, http.StatusUnauthorized, v1.CodeUnauthorized, "that is not a registration token this hub issued", "ask the hub for a new one")
		return
	}
	sameRunner := f.flaw == flawSameRunnerReuse && req.Capabilities.RunnerID == f.runner
	if f.spent && f.flaw == flawExoticEscape {
		f.writeRaw(w, http.StatusUnauthorized, `{"error":{"code":"unauthorized","message":"the registration token `+
			escapeEvery(bearer(r))+` has been used","next_action":""}}`)
		return
	}
	if f.spent && f.flaw != flawTokenIsReusable && !sameRunner {
		// A hub quoting back what it was given: valid JSON, no field any
		// walk of it would know to redact, and one of the commonest ways a
		// token reaches somebody's log.
		message, next := "that registration token has been used", "ask the hub for a new one"
		if f.flaw == flawEchoesTheToken {
			message, next = "the registration token "+bearer(r)+" has been used", ""
		}
		f.fail(w, http.StatusUnauthorized, v1.CodeUnauthorized, message, next)
		return
	}
	if f.flaw == flawVersionFloorQuotes {
		f.fail(w, http.StatusUnauthorized, v1.CodeVersionTooOld,
			"this hub runs runners at 9.9.9 or newer; the token "+bearer(r)+" was not spent", "yad upgrade")
		return
	}
	f.spent = true
	if f.cred == "" {
		// A second registration keeps the first runner's credential working,
		// so a hub with the reusable-token flaw still answers everything
		// after it and the test reads one broken rule rather than a cascade.
		f.cred, f.runner = "fake-credential", req.Capabilities.RunnerID
	}
	if f.flaw == flawCredentialMisnamed {
		// A hub whose field is called something else: the answer is a 200
		// carrying a live credential that no walk by field name can find.
		f.writeRaw(w, http.StatusOK, fmt.Sprintf(`{"credential":%q,"sync_interval_ms":%d,"lease_ms":%d}`,
			f.cred, f.ms(f.interval), int(f.lease/time.Millisecond)))
		return
	}
	f.write(w, http.StatusOK, v1.RegisterResponse{
		RunnerCredential: f.cred, SyncIntervalMS: f.ms(f.interval), LeaseMS: int(f.lease / time.Millisecond),
	})
}

func (f *fake) sync(w http.ResponseWriter, r *http.Request, runner string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req v1.SyncRequest
	if !f.authenticated(w, r, &req, true) {
		return
	}
	if runner != f.runner {
		f.fail(w, http.StatusForbidden, v1.CodeUnauthorized, "this credential is another runner's", "sync as the runner it was issued to")
		return
	}
	now := time.Now()
	res := v1.SyncResponse{NextSyncMS: f.ms(f.interval), LeaseMS: int(f.lease / time.Millisecond)}
	if f.flaw == flawShortInterval {
		res.NextSyncMS = 1000
	}
	if f.flaw == flawUngatedControl {
		res.Controls = append(res.Controls, v1.Control{Kind: v1.ControlSteer, RunID: "fake-run-0", Text: "carry on"})
	}
	// Reserved, and gated on nothing: a runner that does not implement
	// self-update ignores it (decision 0018), so this must not be a finding.
	if f.flaw == flawSendsUpdate {
		res.Controls = append(res.Controls, v1.Control{Kind: v1.ControlUpdate})
	}
	for _, id := range f.order {
		run := f.runs[id]
		if run.expires.IsZero() || run.expires.After(now) || f.flaw == flawRenewsEverything {
			continue
		}
		// The lease lapsed: an offer goes back in the queue, and a run a
		// runner held is lost.
		if run.holder == "" {
			run.queued = true
		} else {
			run.state, run.final, run.holder = v1.RunLost, v1.RunLost, ""
		}
		run.expires = time.Time{}
	}
	listed := map[string]bool{}
	for _, held := range req.Runs {
		listed[held.RunID] = true
		run := f.runs[held.RunID]
		if run == nil || run.holder != "" && run.holder != runner || run.final != "" {
			if f.flaw != flawNoCancel {
				res.Controls = append(res.Controls, v1.Control{Kind: v1.ControlCancel, RunID: held.RunID})
			}
			continue
		}
		run.holder, run.state, run.queued, run.expires = runner, held.State, false, now.Add(f.lease)
	}
	for _, id := range f.order {
		run := f.runs[id]
		// An offer this sync did not list was never received (§2, Sync).
		if run.holder == "" && !run.queued && !listed[id] && f.flaw != flawForgetsOffers && f.flaw != flawOffersTwiceOver {
			// Except for the one this hub keeps losing: re-offering another
			// run for ever says nothing about the rule for this one.
			if f.flaw == flawKeepsOneOffer && id == f.order[len(f.order)-1] {
				continue
			}
			run.queued = true
		}
	}
	free := req.Health.FreeCapacity.Total
	if f.flaw == flawOffersOverCapacity {
		free = max(free, 1)
	}
	// Over-offering only when there is capacity to over-offer: the probe with
	// none declared cannot see this one, and the runs it hands back are runs
	// the runner has nowhere to put.
	if f.flaw == flawOverOffersWhenBusy && free > 0 {
		free++
	}
	// The owner's cap for this harness bounds the answer as the total does.
	if n, capped := req.Health.FreeCapacity.ByHarness[DefaultHarness]; capped && f.flaw != flawIgnoresHarnessCap {
		free = min(free, n)
	}
	for _, id := range f.order {
		run := f.runs[id]
		claimed := run.holder != "" && f.flaw == flawOffersTwice
		if len(res.Runs) >= free || run.final != "" || !run.queued && !claimed {
			continue
		}
		spec := run.spec
		if f.flaw == flawOffersInvalidRun {
			spec.Model = ""
		}
		run.queued, run.expires = false, now.Add(f.lease)
		res.Runs = append(res.Runs, spec)
		// The same run named twice in one answer: offered once, and never
		// dropped by a sync that did not list it.
		if f.flaw == flawOffersTwiceOver {
			run.queued = false
			res.Runs = append(res.Runs, spec)
			break
		}
	}
	if f.flaw == flawSendsEmptyLists {
		f.writeRaw(w, http.StatusOK, fmt.Sprintf(`{"next_sync_ms":%d,"lease_ms":%d,"runs":[],"controls":null}`, res.NextSyncMS, res.LeaseMS))
		return
	}
	f.write(w, http.StatusOK, res)
}

func (f *fake) events(w http.ResponseWriter, r *http.Request, runID string, guarded bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var batch v1.EventBatch
	if !f.authenticated(w, r, &batch, guarded) {
		return
	}
	run := f.runs[runID]
	if (run == nil || run.holder != f.runner) && f.flaw != flawTakesAnyEvents {
		f.notHolder(w, runID)
		return
	}
	if run == nil {
		run = &fakeRun{events: map[int64]bool{}}
	}
	for _, ev := range batch.Events {
		run.events[ev.Seq] = true
		if f.flaw == flawAckJumpsTheGap {
			run.through = max(run.through, ev.Seq)
		}
	}
	if f.flaw != flawAckJumpsTheGap {
		for run.events[run.through+1] {
			run.through++
		}
	}
	f.write(w, http.StatusOK, v1.EventAck{AckedThrough: run.through})
}

func (f *fake) result(w http.ResponseWriter, r *http.Request, runID string, guarded bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var res v1.Result
	if !f.authenticated(w, r, &res, guarded) {
		return
	}
	run := f.runs[runID]
	if run == nil || run.holder != f.runner && run.final != v1.RunLost {
		f.notHolder(w, runID)
		return
	}
	switch {
	case run.final == "" || f.flaw == flawTakesAnyResult:
		run.final, run.state, run.expires = res.State, res.State, time.Time{}
	case run.final != res.State:
		f.fail(w, http.StatusConflict, v1.CodeConflict,
			fmt.Sprintf("run %s is already %s on this hub", runID, run.final), "the hub's state stands; stop reporting this run")
		return
	}
	f.write(w, http.StatusOK, v1.Ack{OK: true})
}

// authenticated checks the runner credential and decodes the body. guarded is
// false on the calls a hub with the sync-only flaw does not authenticate.
func (f *fake) authenticated(w http.ResponseWriter, r *http.Request, body any, guarded bool) bool {
	// A hub that checks an Authorization header when there is one and takes
	// the request when there is not.
	if f.flaw == flawTakesNoBearer && bearer(r) == "" {
		return true
	}
	if guarded && (bearer(r) != f.cred || f.cred == "") {
		f.fail(w, http.StatusUnauthorized, v1.CodeUnauthorized, "this hub does not know that runner credential", "register again")
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(body); err != nil {
		f.fail(w, http.StatusBadRequest, v1.CodeInvalid, "the body is not the JSON this call takes", "check it against protocol/v1/openapi.yaml")
		return false
	}
	if f.flaw == flawStrictEventFields {
		if _, isBatch := body.(*v1.EventBatch); isBatch {
			f.fail(w, http.StatusBadRequest, v1.CodeInvalid, "the batch carries a field this hub does not know", "send only the fields in protocol/v1/openapi.yaml")
			return false
		}
	}
	if f.flaw == flawKeepsUnknownFields {
		// A hub whose decoder is strict: exactly what §2's second wire rule
		// forbids, and what a generated TypeScript client does by default.
		f.fail(w, http.StatusBadRequest, v1.CodeInvalid, "the body carries a field this hub does not know", "send only the fields in protocol/v1/openapi.yaml")
		return false
	}
	return true
}

func (f *fake) notHolder(w http.ResponseWriter, runID string) {
	f.fail(w, http.StatusForbidden, v1.CodeNotHolder, "run "+runID+" is not held by this runner", "stop reporting it")
}

func (f *fake) fail(w http.ResponseWriter, status int, code, message, next string) {
	if f.flaw == flawNoNextAction {
		next = ""
	}
	f.write(w, status, v1.ErrorEnvelope{Error: v1.Error{Code: code, Message: message, NextAction: next}})
}

func (f *fake) write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if f.flaw == flawBOMBeforeJSON {
		// encoding/json refuses a document that starts with one, which is
		// how a body carrying the credential becomes unreadable — and so
		// unredactable — while still being a hub's answer.
		_, _ = w.Write([]byte("\ufeff"))
	}
	_ = json.NewEncoder(w).Encode(body)
}

func (f *fake) writeRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func (f *fake) ms(d time.Duration) int { return int(d / time.Millisecond) }

func bearer(r *http.Request) string {
	scheme, secret, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(secret)
}

// outcome is one check's verdict in a report, by id.
func outcome(t *testing.T, rep *Report, id string) Outcome {
	t.Helper()
	i := slices.IndexFunc(rep.Outcomes, func(o Outcome) bool { return o.ID == id })
	if i < 0 {
		t.Fatalf("no check %q in the report", id)
	}
	return rep.Outcomes[i]
}

// escapeEvery writes every character of s as \uXXXX: legal JSON, and a form no
// search for an encoding somebody anticipated can match.
func escapeEvery(s string) string {
	var b strings.Builder
	for _, r := range s {
		fmt.Fprintf(&b, "\\u%04x", r)
	}
	return b.String()
}
