package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// fake is a hub of about two hundred lines: enough of HUB.md for the suite to
// pass it, and a flaw switch for the tests that each break one rule. It is
// here rather than in internal/hub on purpose — a suite checked only against
// the implementation it was written beside proves nothing about the protocol,
// which is the whole reason this package exists.
type fake struct {
	// flaw is the one rule this hub breaks; empty is a hub that follows HUB.md.
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
	// The second registration token, and the runner it registers: enough
	// for the holder rules, which need a runner the hub knows that is not
	// the one holding the run.
	token2  string
	spent2  bool
	cred2   string
	runner2 string
	runs    map[string]*fakeRun
	order   []string
	// fingerprint is the one that came with the document the hub holds:
	// none after register, which carries no fingerprint.
	fingerprint string
	interval    time.Duration
	lease       time.Duration
}

type fakeRun struct {
	spec   v1.Run
	state  v1.RunState
	queued bool
	offers int
	// offeredTo is the runner the open offer went to, whose next sync alone
	// decides whether it was received.
	offeredTo string
	holder    string
	expires   time.Time
	events    map[int64]bool
	through   int64
	final     v1.RunState
	// lastSeq is the last_seq the result that ended the run named.
	lastSeq int64
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
	flawForgetsOffers      = "an offered run the next sync did not list stays offered"
	flawOffersTwice        = "a claimed run is offered again"
	flawOffersInvalidRun   = "a run with no model is offered"
	flawAckJumpsTheGap     = "acked_through is the highest seq stored, gap or not"
	flawTakesAnyEvents     = "events are taken for any run from anyone"
	flawTakesAnyResult     = "a second, different terminal state replaces the first"
	flawShortInterval      = "the sync interval named is a second"
	flawShortLease         = "the lease named is shorter than the interval named beside it"
	flawStrictResultFields = "a result carrying an unknown field is refused"
	// The token in the error code rather than the message: a place a check
	// writes into its own sentence, not one the answer's printing covers.
	flawTokenInTheCode   = "the code of a refusal is built from the bearer it was given"
	flawOffersLiveMode   = "a run is offered in a live session to a runner advertising nothing"
	flawOffersEffort     = "a run carrying an effort is offered to a runner advertising nothing"
	flawGrantInTheOpen   = "a run's grant value comes back quoted in a later refusal"
	flawRegistersAnyone  = "anyone registers, and the credential comes back under a name of the hub's own"
	flawUngatedControl   = "a steer goes to a runner that never advertised one"
	flawNoNextAction     = "errors say what went wrong and not what to do"
	flawRenewsEverything = "every run's lease is renewed, listed or not"
	// The gap DEV-94 closed: an offer held for its runner however long that
	// runner is gone, and claimed by its listing whenever it comes back.
	flawOffersNeverLapse   = "an offer waits for its runner's next sync, however late"
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
	// The floor quotes the token as well, and a version refusal is one HUB.md
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
	// Unknown runs are still refused, so the near half of the rule passes and
	// only a second runner can show the hub is not asking who holds the run.
	flawNoHolderCheck = "events and a result for a held run are taken from any runner"
	// Only to the second runner, so the answer errors/next-action prints
	// first is the one carrying the second credential.
	flawQuotesTheNonHolder = "a refusal to a runner that does not hold the run quotes its credential, and names no next action"
	// A refusal, in the envelope, that tells the runner to try again: it
	// resends for ever what the hub will never take.
	flawNonHolderGets500 = "a run the caller does not hold is refused with 500 internal"
	// The answer is right and the write happened anyway: only what the
	// holder hears afterwards can show it.
	flawStoresRefusedResult = "a result from a runner that does not hold the run is refused with 403 and stored"
	// The flaws the clean-room hub of DEV-110 showed no check could see.
	flawIgnoresBodyRunner = "the body's runner_id is not compared with the path"
	// DEV-120: a document naming another runner, stored as this one's.
	flawIgnoresDocumentRunner = "the capability document's runner_id is not compared with the path"
	// Refused, and in a way a runner retries for ever.
	flawDocumentRunnerIs500 = "a capability document naming another runner is refused with 500 internal"
	flawAnyCredentialSyncs  = "any credential the hub issued syncs as any runner"
	flawNoReportCaps        = "a fingerprint that moves without a document is not asked about"
	flawInvalidIs500        = "a body that does not parse is answered 500 internal"
	flawTakesSeqZero        = "an event numbered 0 is stored"
	flawTakesAnyState       = "a result's state is not checked to be terminal"
	flawTooLargeIsInvalid   = "a body over the size limit is refused 400 invalid"
	flawRefusalNeedsAClaim  = "a result is taken only from the runner that claimed the run"
	flawReoffersRefused     = "a run refused before its claim is offered again"
	// The document marked the dashboard health required until DEV-117, and a
	// hub generated from it refuses a sync that leaves any of it out. The
	// suite's own syncs send zeros, which v1 omits, so this refuses them all:
	// the named check is what says why.
	flawStrictDashboardHealth = "a sync that leaves out load, disk, spool or outbox depth is refused"
	// The clean-room hub's guess of DEV-110: the last runner a queued run was
	// offered to may still refuse it.
	flawRefusalOutlivesTheOffer = "a refusal is taken from the runner a run was last offered to after the offer was taken back"
)

// fakeBodyLimit is the most of a body the fake reads. TestMain shrinks it with
// the suite's oversize, so sixty runs of the suite do not each send 34 MiB.
var fakeBodyLimit int64 = 16 << 20

func TestMain(m *testing.M) {
	oversize, fakeBodyLimit = 17<<10, 16<<10
	os.Exit(m.Run())
}

const fakeSecondToken = "fake-second-registration-token"

func newFake(t *testing.T, flaw string, queued ...v1.Run) (*fake, string) {
	t.Helper()
	f := &fake{
		flaw: flaw, token: "fake-registration-token", token2: fakeSecondToken, runs: map[string]*fakeRun{},
		// The shortest pair HUB.md allows, so a test that waits a lease out waits
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

// A secret a hub sends rather than one this suite presented.
const grantValue = "grant-value-0123456789"

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
		code := v1.CodeUnsupportedProtocol
		if f.flaw == flawTokenInTheCode {
			code += "_for_" + bearer(r)
		}
		f.fail(w, http.StatusUpgradeRequired, code, "this hub speaks protocol 1", "upgrade yad or the hub")
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
	if bearer(r) == f.token2 {
		if f.spent2 || json.NewDecoder(r.Body).Decode(&req) != nil {
			f.fail(w, http.StatusUnauthorized, v1.CodeUnauthorized, "that registration token has been used", "ask the hub for a new one")
			return
		}
		f.spent2, f.cred2, f.runner2 = true, "fake-second-credential", req.Capabilities.RunnerID
		f.write(w, http.StatusOK, v1.RegisterResponse{
			RunnerCredential: f.cred2, SyncIntervalMS: f.ms(f.interval), LeaseMS: int(f.lease / time.Millisecond),
		})
		return
	}
	issued := bearer(r) == f.token || f.flaw == flawAcceptsAnyToken && bearer(r) != "" || f.flaw == flawRegistersAnyone
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
		if f.flaw == flawGrantInTheOpen {
			// The hub quoting back a secret of its own, in a place no field
			// name would find it and no list of this suite's own secrets
			// would cover.
			message, next = "the grant "+grantValue+" was not accepted", ""
		}
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
	if f.flaw == flawCredentialMisnamed || f.flaw == flawRegistersAnyone {
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
	if runner != f.caller(r) && f.flaw != flawAnyCredentialSyncs {
		f.fail(w, http.StatusForbidden, v1.CodeUnauthorized, "this credential is another runner's", "sync as the runner it was issued to")
		return
	}
	if f.flaw == flawStrictDashboardHealth && (req.Health.Load == 0 || req.Health.DiskFreeBytes == 0 || req.Health.SpoolDepth == 0 || req.Health.OutboxDepth == 0) {
		f.fail(w, http.StatusBadRequest, v1.CodeInvalid, "health is missing a required field", "send every field protocol/v1/openapi.yaml requires")
		return
	}
	if req.RunnerID != runner && f.flaw != flawIgnoresBodyRunner {
		f.fail(w, http.StatusBadRequest, v1.CodeInvalid, "the body's runner_id is not the path's", "send the same runner id in both")
		return
	}
	if req.Capabilities != nil && req.Capabilities.RunnerID != runner && f.flaw == flawDocumentRunnerIs500 {
		f.fail(w, http.StatusInternalServerError, v1.CodeInternal, "the capability document's runner_id is not the path's", "send the syncing runner's own document")
		return
	}
	if req.Capabilities != nil && req.Capabilities.RunnerID != runner && f.flaw != flawIgnoresDocumentRunner {
		f.fail(w, http.StatusBadRequest, v1.CodeInvalid, "the capability document's runner_id is not the path's", "send the syncing runner's own document")
		return
	}
	now := time.Now()
	res := v1.SyncResponse{NextSyncMS: f.ms(f.interval), LeaseMS: int(f.lease / time.Millisecond)}
	if f.flaw == flawShortLease {
		res.LeaseMS = f.ms(f.interval) / 2
	}
	if f.flaw == flawShortInterval {
		res.NextSyncMS = 1000
	}
	// Only the first runner's fingerprint is kept: it is the one the suite
	// moves, and the second runner's first sync carries its document.
	if runner == f.runner {
		switch {
		case req.Capabilities != nil:
			f.fingerprint = req.Fingerprint
		case req.Fingerprint != f.fingerprint && f.flaw != flawNoReportCaps:
			res.Controls = append(res.Controls, v1.Control{Kind: v1.ControlReportCapabilities})
		}
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
			if f.flaw == flawOffersNeverLapse {
				continue
			}
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
		// This runner's to list: held by it, or offered to it with the offer
		// still open. An offer that lapsed is queued again, and a claim of it
		// now may be of a run another runner has since been offered.
		mine := run != nil && (run.holder == runner || run.holder == "" && !run.queued && run.offeredTo == runner)
		if !mine || run.final != "" {
			if f.flaw != flawNoCancel {
				res.Controls = append(res.Controls, v1.Control{Kind: v1.ControlCancel, RunID: held.RunID})
			}
			continue
		}
		run.holder, run.state, run.queued, run.expires = runner, held.State, false, now.Add(f.lease)
	}
	for _, id := range f.order {
		run := f.runs[id]
		// An offer this sync did not list was never received (HUB.md §3).
		if run.holder == "" && !run.queued && run.offeredTo == runner && !listed[id] && f.flaw != flawForgetsOffers && f.flaw != flawOffersTwiceOver {
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
		if f.flaw == flawOffersLiveMode {
			spec.Session.Mode = v1.SessionLive
		}
		if f.flaw == flawOffersEffort {
			spec.Effort = "high"
		}
		if f.flaw == flawGrantInTheOpen {
			spec.Grants = []v1.Grant{{Name: "TOKEN", Value: grantValue, As: v1.GrantEnv}}
		}
		run.queued, run.expires, run.offeredTo = false, now.Add(f.lease), runner
		run.offers++
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
	for _, ev := range batch.Events {
		if ev.Seq < 1 && f.flaw != flawTakesSeqZero {
			f.fail(w, http.StatusBadRequest, v1.CodeInvalid, "a run's events are numbered from 1", "number them from 1")
			return
		}
	}
	run := f.runs[runID]
	if !f.holds(r, run) && f.flaw != flawTakesAnyEvents {
		f.notHolder(w, r, runID)
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
	if !res.State.IsTerminal() && f.flaw != flawTakesAnyState {
		f.fail(w, http.StatusBadRequest, v1.CodeInvalid, "a result carries a terminal state", "report the others in the sync")
		return
	}
	run := f.runs[runID]
	// The runner the open offer went to may refuse the run with a result.
	open := run != nil && (!run.queued || f.flaw == flawRefusalOutlivesTheOffer)
	offeredHere := open && run.holder == "" && run.offeredTo == f.caller(r) && f.flaw != flawRefusalNeedsAClaim
	if offeredHere && run.final == "" && f.flaw == flawReoffersRefused {
		run.queued = true
		f.write(w, http.StatusOK, v1.Ack{OK: true})
		return
	}
	if run == nil || !f.holds(r, run) && !offeredHere && run.final != v1.RunLost {
		if run != nil && run.final == "" && f.flaw == flawStoresRefusedResult {
			run.final, run.state = res.State, res.State
		}
		f.notHolder(w, r, runID)
		return
	}
	switch {
	case run.final == "" || f.flaw == flawTakesAnyResult:
		run.final, run.state, run.expires, run.lastSeq = res.State, res.State, time.Time{}, res.LastSeq
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
		guarded = false
	}
	if guarded && (f.cred == "" || bearer(r) != f.cred && (f.cred2 == "" || bearer(r) != f.cred2)) {
		f.fail(w, http.StatusUnauthorized, v1.CodeUnauthorized, "this hub does not know that runner credential", "register again")
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, fakeBodyLimit)).Decode(body); err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge) && f.flaw == flawTooLargeIsInvalid:
			f.fail(w, http.StatusBadRequest, v1.CodeInvalid, "the body is too large", "send less")
		case errors.As(err, &tooLarge):
			f.fail(w, http.StatusRequestEntityTooLarge, v1.CodeInvalid, "the body is too large", "send less")
		case f.flaw == flawInvalidIs500:
			f.fail(w, http.StatusInternalServerError, v1.CodeInternal, "the body could not be read", "retry later")
		default:
			f.fail(w, http.StatusBadRequest, v1.CodeInvalid, "the body is not the JSON this call takes", "check it against protocol/v1/openapi.yaml")
		}
		return false
	}
	if f.flaw == flawStrictEventFields {
		if _, isBatch := body.(*v1.EventBatch); isBatch {
			f.fail(w, http.StatusBadRequest, v1.CodeInvalid, "the batch carries a field this hub does not know", "send only the fields in protocol/v1/openapi.yaml")
			return false
		}
	}
	if f.flaw == flawStrictResultFields {
		if _, isResult := body.(*v1.Result); isResult {
			f.fail(w, http.StatusBadRequest, v1.CodeInvalid, "the result carries a field this hub does not know", "send only the fields in protocol/v1/openapi.yaml")
			return false
		}
	}
	if f.flaw == flawKeepsUnknownFields {
		// A hub whose decoder is strict: exactly what HUB.md's second wire rule
		// forbids, and what a generated TypeScript client does by default.
		f.fail(w, http.StatusBadRequest, v1.CodeInvalid, "the body carries a field this hub does not know", "send only the fields in protocol/v1/openapi.yaml")
		return false
	}
	return true
}

// caller is the runner a request's credential belongs to. Anything but the
// second runner's credential is taken as the first's, as it was before there
// was a second: the flaws about a missing or unread bearer rely on it.
func (f *fake) caller(r *http.Request) string {
	if f.cred2 != "" && bearer(r) == f.cred2 {
		return f.runner2
	}
	return f.runner
}

// holds says the calling runner may report on run: it holds it — or, on the
// hub with the holder check switched off, somebody does.
func (f *fake) holds(r *http.Request, run *fakeRun) bool {
	if run == nil {
		return false
	}
	return run.holder == f.caller(r) || f.flaw == flawNoHolderCheck && run.holder != ""
}

func (f *fake) notHolder(w http.ResponseWriter, r *http.Request, runID string) {
	if f.flaw == flawNonHolderGets500 {
		f.fail(w, http.StatusInternalServerError, v1.CodeInternal, "run "+runID+" could not be matched to this runner", "retry later")
		return
	}
	if f.flaw == flawQuotesTheNonHolder && f.cred2 != "" && bearer(r) == f.cred2 {
		f.fail(w, http.StatusForbidden, v1.CodeNotHolder, "run "+runID+" is not held by the runner with credential "+bearer(r), "")
		return
	}
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
