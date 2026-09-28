package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// candidatePage is how many queued runs one query returns. The query already
// leaves out everything this runner cannot take, so a page is mostly offers;
// offer pages on until the free capacity is filled or the queue runs out.
const candidatePage = 64

func (h *Hub) sync(ctx context.Context, in *syncInput) (*syncOutput, error) {
	runner, err := h.authenticate(ctx, in.Runner)
	if err != nil {
		return nil, err
	}
	req := in.Body
	if req.RunnerID != in.Runner {
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid,
			fmt.Sprintf("body runner_id %q does not match the path's %q", req.RunnerID, in.Runner),
			"send the same runner id in the path and the body")
	}
	// The document names its runner too, and is stored as this one's: a
	// third id to disagree with is refused for the same reason (DEV-120).
	if req.Capabilities != nil && req.Capabilities.RunnerID != in.Runner {
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid,
			fmt.Sprintf("the capability document's runner_id %q does not match the path's %q", req.Capabilities.RunnerID, in.Runner),
			"send the capability document of the runner that is syncing")
	}
	health, err := json.Marshal(req.Health)
	if err != nil {
		return nil, err
	}
	now := h.now()
	lease := sql.NullInt64{Int64: store.Ms(now.Add(h.lease)), Valid: true}
	me := sql.NullString{String: runner.ID, Valid: true}
	out := v1.SyncResponse{
		NextSyncMS: int(h.interval / time.Millisecond),
		LeaseMS:    int(h.lease / time.Millisecond),
		MinVersion: h.minVersion,
	}

	err = h.store.Tx(ctx, func(q *db.Queries) error {
		// Lapsed leases are settled before this sync is judged, so a runner
		// back from a long absence hears that its runs were lost rather than
		// renewing them.
		if err := h.sweep(ctx, q, now); err != nil {
			return err
		}
		// Read again inside the transaction: a drain asked for since the
		// credential was checked must stop this sync's offers, and a
		// credential retired since must stop the sync.
		runner, err := h.current(ctx, q, runner)
		if err != nil {
			return err
		}

		docJSON, wants := runner.Capabilities, runner.WantsCapabilities != 0
		switch {
		case req.Capabilities != nil:
			b, err := json.Marshal(req.Capabilities)
			if err != nil {
				return err
			}
			docJSON, wants = string(b), false
			if err := q.SetCapabilities(ctx, db.SetCapabilitiesParams{Capabilities: docJSON, Fingerprint: req.Fingerprint, ID: runner.ID}); err != nil {
				return err
			}
		case req.Fingerprint != runner.Fingerprint:
			wants = true
		}
		// A fingerprint that moved with no document says the copy on file no
		// longer describes this runner — the report_capabilities below asks
		// for it. Until it lands, what the runner acts on is unknown, so a
		// control gated on a feature waits a sync rather than being spent
		// against a document known to be out of date. v1 says the document
		// goes with the first sync after a move, so this window opens only for
		// a runner that did not send it.
		described := req.Capabilities != nil || req.Fingerprint == runner.Fingerprint
		// The document holds the only version a sync knows — a sync request
		// carries none — so the floor is judged on what this runner last sent,
		// before anything is recorded: a refused sync renews no lease and
		// claims no run, and the transaction rolls back to prove it.
		//
		// Deliberately unlike the controls above, which wait when `described`
		// is false: the floor judges a document it knows to be stale rather
		// than deferring, because a check a runner could suspend by moving its
		// fingerprint and sending nothing is not a floor. Do not make these
		// two agree — the asymmetry is the point.
		var doc v1.Capabilities
		if err := json.Unmarshal([]byte(docJSON), &doc); err != nil {
			return fmt.Errorf("stored capability document for %s: %w", runner.ID, err)
		}
		if err := h.refuseOld(doc.YadVersion); err != nil {
			return err
		}
		if err := q.RecordSync(ctx, db.RecordSyncParams{
			LastSyncAt: sql.NullInt64{Int64: store.Ms(now), Valid: true}, Health: sql.NullString{String: string(health), Valid: true},
			WantsCapabilities: boolInt(wants), ID: runner.ID,
		}); err != nil {
			return err
		}
		if wants {
			out.Controls = append(out.Controls, v1.Control{Kind: v1.ControlReportCapabilities})
		}
		// A drain asked for goes out until a sync says draining; that sync
		// is the answer, and the request is done (decision 0029).
		switch {
		case runner.DrainRequestedAt.Valid && req.Health.Draining:
			if err := q.ClearDrain(ctx, runner.ID); err != nil {
				return err
			}
		case runner.DrainRequestedAt.Valid && described && advertises(doc, capability.FeatureDrain):
			out.Controls = append(out.Controls, v1.Control{Kind: v1.ControlDrain})
		}

		// The runs this answer cancels. One of them may be queued again — an
		// offer whose lease lapsed before this late claim arrived — and it is
		// not offered back in the same answer: a runner handed a cancel and
		// an offer for one run at once has to guess which the hub meant.
		cancelled := map[string]bool{}
		for _, held := range req.Runs {
			run, err := q.GetRun(ctx, held.RunID)
			if errors.Is(err, sql.ErrNoRows) || err == nil && !holdable(run, runner.ID) {
				// Not this runner's to hold: it must stop, and nothing it
				// reports for the run will be applied.
				out.Controls = append(out.Controls, v1.Control{Kind: v1.ControlCancel, RunID: held.RunID})
				cancelled[held.RunID] = true
				continue
			}
			if err != nil {
				return err
			}
			if err := q.RenewRun(ctx, db.RenewRunParams{
				State: string(held.State), LeaseExpiresAt: lease,
				ResumesAt: nullTime(held.ResumesAt), Reason: sql.NullString{String: held.Reason, Valid: held.Reason != ""},
				UpdatedAt: store.Ms(now), ID: run.ID, RunnerID: me,
			}); err != nil {
				return err
			}
			if run.State == "offered" {
				if err := q.BindSession(ctx, db.BindSessionParams{RunnerID: me, RunID: sql.NullString{String: run.ID, Valid: true}, ID: run.SessionID}); err != nil {
					return err
				}
			}
			controls, err := deliver(ctx, q, run.ID, doc, described)
			if err != nil {
				return err
			}
			out.Controls = append(out.Controls, controls...)
		}
		// A claim this sync leaves out, with a cancel asked for, was withdrawn:
		// the runner heard the cancel before any answer confirmed the claim —
		// that answer lost in transit — and owes no result (decision 0019).
		// The hub asked for this end, so the run is cancelled now rather than
		// lost when its lease lapses (decision 0061). The runner deleted the
		// session such a claim opened, so a session that claim bound goes
		// back to unbound, and its next run opens it (DEV-143). Before the
		// closes below: a close the runner reports for it is believed from
		// the runner it was offered to.
		withdrawn, err := cancelWithdrawn(ctx, q, runner.ID, req.Runs,
			fmt.Sprintf("cancelled before runner %s started it; the runner withdrew its claim", runner.ID), now)
		if err != nil {
			return err
		}
		for _, w := range withdrawn {
			if _, err := q.UnbindWithdrawnSession(ctx, db.UnbindWithdrawnSessionParams{
				ID: w.SessionID, RunnerID: me, RunID: sql.NullString{String: w.ID, Valid: true},
			}); err != nil {
				return err
			}
		}

		// A close the runner reports is what closes the session here, and
		// answers the close_session this hub was repeating (decision 0035).
		// It is believed from the runner holding the session, or from the one
		// its run was last offered to before any claim bound it. The runs
		// still waiting in it end now rather than being offered to be
		// refused: before the unlisted offers below go back in the queue, so
		// a run offered to this runner in the closed session is not among
		// them.
		for _, c := range req.ClosedSessions {
			at := c.ClosedAt
			if at.IsZero() {
				at = now
			}
			n, err := q.RecordSessionClosed(ctx, db.RecordSessionClosedParams{
				ClosedAt: sql.NullInt64{Int64: store.Ms(at), Valid: true},
				Reason:   sql.NullString{String: string(c.Reason), Valid: c.Reason != ""},
				ID:       c.SessionID, RunnerID: me,
			})
			if err != nil {
				return err
			}
			// A close the hub made while this runner was silent is answered
			// by the runner's report of it, whatever reason it gives.
			if err := q.SettleOwedClose(ctx, db.SettleOwedCloseParams{ID: c.SessionID, RunnerID: me}); err != nil {
				return err
			}
			if n == 0 {
				continue
			}
			if err := endSessionRuns(ctx, q, c.SessionID, closedByRunner(runner.ID, c.Reason), now); err != nil {
				return err
			}
		}
		closing, err := q.SessionsToClose(ctx, me)
		if err != nil {
			return err
		}
		if described && advertises(doc, capability.FeatureCloseSession) {
			for _, id := range closing {
				out.Controls = append(out.Controls, v1.Control{Kind: v1.ControlCloseSession, SessionID: id})
			}
		}

		// Logins before the early return below: a draining runner still
		// reports them, and still takes a cancel or a code.
		logins, err := syncLogins(ctx, q, runner.ID, req.Logins, described && advertises(doc, capability.FeatureLogin),
			described && advertises(doc, capability.FeatureAccounts), described, now)
		if err != nil {
			return err
		}
		out.Controls = append(out.Controls, logins...)
		removals, err := syncRemovals(ctx, q, runner.ID, req.Health, doc, described)
		if err != nil {
			return err
		}
		out.Controls = append(out.Controls, removals...)

		// Whatever is still offered to this runner was in the last response
		// and missing from this list: never received. Back in the queue.
		unlisted, err := q.RunsOfferedTo(ctx, me)
		if err != nil {
			return err
		}
		for _, r := range unlisted {
			if err := q.RequeueRun(ctx, db.RequeueRunParams{UpdatedAt: store.Ms(now), ID: r.ID}); err != nil {
				return err
			}
		}

		// A draining runner takes nothing, and one asked to drain is about
		// to: an offer now would only come back.
		if req.Health.Draining || runner.DrainRequestedAt.Valid {
			return nil
		}
		out.Runs, err = h.offer(ctx, q, runner.ID, doc, described, req.Health.FreeCapacity, cancelled, lease, now)
		if err != nil {
			return err
		}
		soon, err := h.waitsForThisRunner(ctx, q, runner.ID, doc, described, req.Runs, req.Health.FreeCapacity, now)
		if soon {
			out.NextSyncMS = int(h.quickInterval() / time.Millisecond)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	// A sync moves run states — offered, claimed, running, lost — and a
	// watcher waiting on one hears it now rather than at its next poll.
	h.bell.ring()
	return &syncOutput{Body: out}, nil
}

// offer picks queued runs for this runner and marks them offered. Never more
// than the free capacity it declared, in total or for any harness it capped,
// and never a harness it cannot drive: the runner would have to refuse it.
func (h *Hub) offer(ctx context.Context, q *db.Queries, runnerID string, doc v1.Capabilities, described bool, free v1.Capacity, cancelled map[string]bool, lease sql.NullInt64, now time.Time) ([]v1.Run, error) {
	me := sql.NullString{String: runnerID, Valid: true}
	left := map[string]int{}
	for id, n := range free.ByHarness {
		left[id] = n
	}
	// takeable is the harnesses this runner can drive and still has room
	// for; it shrinks as caps fill, and the next page asks for fewer.
	takeable := func() []string {
		var ids []string
		for _, hr := range doc.Harnesses {
			if n, capped := left[hr.ID]; capped && n <= 0 {
				continue
			}
			if capability.Drivable(doc, hr.ID) && !slices.Contains(ids, hr.ID) {
				ids = append(ids, hr.ID)
			}
		}
		return ids
	}
	if free.Total <= 0 {
		return nil, nil
	}
	admits := admission(doc, described, now)
	var runs []v1.Run
	err := walkCandidates(ctx, takeable, func(harnesses string, after db.Run) ([]db.Run, error) {
		return q.OfferCandidates(ctx, db.OfferCandidatesParams{
			HarnessesJson: harnesses, AfterCreatedAt: after.CreatedAt, AfterID: after.ID, RunnerID: me, Max: candidatePage,
		})
	}, func(c db.Run) (bool, error) {
		if n, capped := left[c.Harness]; capped && n <= 0 || cancelled[c.ID] {
			return true, nil
		}
		run, err := spec(c)
		if err != nil {
			return false, err
		}
		if err := opening(ctx, q, &run); err != nil {
			return false, err
		}
		if !admits(run) {
			return true, nil
		}
		if err := q.OfferRun(ctx, db.OfferRunParams{RunnerID: me, LeaseExpiresAt: lease, UpdatedAt: store.Ms(now), ID: c.ID}); err != nil {
			return false, err
		}
		if err := q.NoteSessionOffer(ctx, db.NoteSessionOfferParams{RunnerID: me, ID: c.SessionID}); err != nil {
			return false, err
		}
		if _, capped := left[c.Harness]; capped {
			left[c.Harness]--
		}
		runs = append(runs, run)
		return len(runs) < free.Total, nil
	})
	return runs, err
}

// waitsForThisRunner is whether a queued run waits for this runner and for
// nothing but a run it is executing — full capacity, or its session's live
// run — which is when the answer asks it back after quickInterval (decision
// 0063). A run submitted to an idle runner is not helped and cannot be: the
// answer that would have to change was sent before the run existed (DEV-49).
//
// Only a run it would be offered counts, so an idle fleet and a run nobody
// here can take change nothing. And only one that a run it is executing would
// let go by ending: a waiting run ends at an account's reset, hours off, and
// a runner listing none has its capacity taken by nothing this hub can see
// end, so asking either sooner finds it no freer. With its harness's own cap
// full, only a run of that harness ending frees it; otherwise the total is
// what is full, and any run ending frees that.
func (h *Hub) waitsForThisRunner(ctx context.Context, q *db.Queries, runnerID string, doc v1.Capabilities, described bool, held []v1.HeldRun, free v1.Capacity, now time.Time) (bool, error) {
	executing := map[string]bool{}
	for _, r := range held {
		if r.State == v1.RunWaiting {
			continue
		}
		run, err := q.GetRun(ctx, r.RunID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return false, err
		}
		executing[run.Harness] = true
	}
	if len(executing) == 0 {
		return false, nil
	}
	frees := func(harness string) bool {
		n, capped := free.ByHarness[harness]
		return executing[harness] || !capped || n > 0
	}
	var harnesses []string
	for _, hr := range doc.Harnesses {
		if n, capped := doc.Capacity.ByHarness[hr.ID]; capped && n <= 0 {
			continue
		}
		if capability.Drivable(doc, hr.ID) && !slices.Contains(harnesses, hr.ID) {
			harnesses = append(harnesses, hr.ID)
		}
	}
	me := sql.NullString{String: runnerID, Valid: true}
	admits := admission(doc, described, now)
	found := false
	err := walkCandidates(ctx, func() []string { return harnesses }, func(list string, after db.Run) ([]db.Run, error) {
		return q.SoonCandidates(ctx, db.SoonCandidatesParams{
			HarnessesJson: list, AfterCreatedAt: after.CreatedAt, AfterID: after.ID, RunnerID: me, Max: candidatePage,
		})
	}, func(c db.Run) (bool, error) {
		if !frees(c.Harness) {
			return true, nil
		}
		run, err := spec(c)
		if err != nil {
			return false, err
		}
		// Read-only: it decides whether the run would open its session,
		// which the rule about sources on the machine turns on.
		if err := opening(ctx, q, &run); err != nil {
			return false, err
		}
		found = admits(run)
		return !found, nil
	})
	return found && err == nil, err
}

// admission is what a runner's document says about the queued runs it may be
// offered, capacity aside, judged on a run opening has prepared. Shared by the
// offer and by waitsForThisRunner, so that a runner is never asked back
// sooner for a run it would then not be offered.
func admission(doc v1.Capabilities, described bool, now time.Time) func(v1.Run) bool {
	// A run whose start moment is still ahead goes only to a runner that will
	// hold it back: one without the feature starts it on arrival. Once the
	// moment has passed there is nothing left to hold, so the run is offered
	// to anyone — otherwise a fleet of runners without the feature would leave
	// it queued for good, and nothing else rescues it.
	//
	// The skip is here rather than in OfferCandidates, whose comment asks for
	// the opposite, because the moment lives in the run's JSON spec in
	// whatever zone the submitter wrote it, and SQLite date maths over that is
	// a worse bet than a page walked twice. The cost is paid only by a runner
	// that does not advertise start_at — no yad build produces one — and only
	// against runs still waiting for their moment.
	//
	// A document the hub has asked to replace cannot answer this either, and
	// an offer is the one thing a later sync cannot take back: a run started
	// early has started. So an undescribed runner is offered nothing it would
	// have to hold, and hears about those runs a sync later.
	holdsStartAt := described && advertises(doc, capability.FeatureStartAt)
	// A run carrying an effort goes only to a runner that will hand it to the
	// harness. Unlike a start moment it never expires: one without the
	// feature would drop the field and run at the harness's default, and the
	// run would succeed with nothing saying so (decision 0049). On a fleet
	// that advertises none the run stays queued, which is the honest answer.
	// An undescribed runner is offered none, for the reason above.
	takesEffort := described && advertises(doc, capability.FeatureEffort)
	// A run opening a session with a source on the machine goes only to a
	// runner whose owner has not switched those off: that runner would fail
	// it source_refused, where another may take it (decision 0062). A run in
	// a session already bound here is offered anyway — it can go nowhere
	// else, and the runner's refusal names the setting, where a run left
	// queued would say nothing. An undescribed runner is offered none, for
	// the reason above.
	takesLocal := described && (doc.PathSources == nil || *doc.PathSources)
	return func(run v1.Run) bool {
		if run.StartAt != nil && run.StartAt.After(now) && !holdsStartAt {
			return false
		}
		if run.Effort != "" && !takesEffort {
			return false
		}
		return !run.Session.New || takesLocal || !slices.ContainsFunc(run.Sources, onTheMachine)
	}
}

// walkCandidates pages through queued runs oldest first, asking each page for
// the harnesses harnesses names at that moment — offer's list shrinks as caps
// fill — until visit says it has seen enough or the queue runs out.
func walkCandidates(ctx context.Context, harnesses func() []string, page func(harnessesJSON string, after db.Run) ([]db.Run, error), visit func(db.Run) (more bool, err error)) error {
	var cursor db.Run
	for {
		ids := harnesses()
		if len(ids) == 0 {
			return nil
		}
		// A JSON array rather than sqlc.slice: sqlc numbers its parameters,
		// and expanding a slice of two or more shifted every one after it.
		list, err := json.Marshal(ids)
		if err != nil {
			return err
		}
		runs, err := page(string(list), cursor)
		if err != nil {
			return err
		}
		for _, c := range runs {
			cursor = c
			more, err := visit(c)
			if err != nil || !more {
				return err
			}
		}
		if len(runs) < candidatePage {
			return nil
		}
	}
}

func spec(c db.Run) (v1.Run, error) {
	var run v1.Run
	if err := json.Unmarshal([]byte(c.Spec), &run); err != nil {
		return v1.Run{}, fmt.Errorf("stored run %s: %w", c.ID, err)
	}
	return run, nil
}

// onTheMachine is a source a runner's path_sources governs: a path, or a git
// URL that names a repository on the runner's own disk. It follows the
// runner's reading of a URL (parseRemote in internal/workdir) — anything it
// would take for a network URL goes, and anything malformed it refuses
// whatever the setting.
func onTheMachine(s v1.Source) bool {
	if s.Git == nil {
		return s.Path != ""
	}
	return strings.HasPrefix(s.Git.URL, "/") || strings.HasPrefix(strings.ToLower(s.Git.URL), "file://")
}

// opening sets session.new on a run about to be offered: true while no claim
// has bound its session, false after (decision 0047). It is decided here and
// not at submit because the run that opens a session is the first one a
// runner claims, and which that is only the hub's history can say: a session
// whose first run was refused, cancelled on its claim or withdrawn exists on
// no runner, and its next run has to open it. What the submitter said at
// submit decided only whether the hub created the session or found it.
//
// A run opening the session that names no sources itself — a continuation
// whose session's first run never bound it — carries the sources of the run
// whose submission created the session: the runner builds a new session's
// workdir from the run that opens it, and would build this one empty. A
// continuing run is sent as submitted; the runner holds its session's
// sources and prepares a run naming none from them.
func opening(ctx context.Context, q *db.Queries, run *v1.Run) error {
	sess, err := q.GetSession(ctx, run.Session.ID)
	if err != nil {
		return err
	}
	run.Session.New = !sess.RunnerID.Valid
	if !run.Session.New || len(run.Sources) > 0 {
		return nil
	}
	spec, err := q.SessionCreatorSpec(ctx, run.Session.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var creator v1.Run
	if err := json.Unmarshal([]byte(spec), &creator); err != nil {
		return fmt.Errorf("stored run in session %s: %w", run.Session.ID, err)
	}
	run.Sources = creator.Sources
	return nil
}

// Sweep withdraws offers and loses runs whose leases lapsed, and gives up the
// sessions of runners silent for longer than the hub's abandon-after. Every
// sync sweeps first; `yad hub serve` also sweeps on a timer, so a hub whose
// only runner went away still marks that runner's runs lost.
func (h *Hub) Sweep(ctx context.Context) error {
	err := h.store.Tx(ctx, func(q *db.Queries) error { return h.sweep(ctx, q, h.now()) })
	h.bell.ring()
	return err
}

// SweepEvery is how often `yad hub serve` should call Sweep.
func (h *Hub) SweepEvery() time.Duration { return h.interval }

// sweep settles what time alone decides. An offer carries a lease exactly as
// a claim does: one not claimed within it goes back in the queue for any
// runner, and a claim arriving after that is answered with a cancel, because
// the run may already be another runner's. Then the runs whose leases lapsed
// end: cancelled for a claim the hub was asked to cancel (decision 0061),
// lost for every other. Last the runners silent past abandon-after are given
// up — after the leases, so that by then nothing they held is still leased.
func (h *Hub) sweep(ctx context.Context, q *db.Queries, now time.Time) error {
	if _, err := q.RequeueWithdrawnOffers(ctx, store.Ms(now)); err != nil {
		return err
	}
	if _, err := q.CancelLapsedClaims(ctx, store.Ms(now)); err != nil {
		return err
	}
	if _, err := q.LoseLapsedRuns(ctx, store.Ms(now)); err != nil {
		return err
	}
	if err := sweepLogins(ctx, q, now); err != nil {
		return err
	}
	return h.abandonSilent(ctx, q, now)
}

// authenticate finds the runner the bearer credential belongs to, and checks
// it is the runner the path names.
func (h *Hub) authenticate(ctx context.Context, pathRunner string) (db.Runner, error) {
	r, err := h.caller(ctx)
	if err != nil {
		return db.Runner{}, err
	}
	if r.ID != pathRunner {
		return db.Runner{}, Fail(http.StatusForbidden, v1.CodeUnauthorized,
			fmt.Sprintf("the credential belongs to runner %q, not %q", r.ID, pathRunner),
			"sync with the runner id this credential was issued to")
	}
	return r, nil
}

// cancelWithdrawn ends cancelled every claim of this runner's that a cancel
// was asked for and that held does not list, and returns them. Deregister
// passes no runs: a runner that has gone holds nothing.
func cancelWithdrawn(ctx context.Context, q *db.Queries, runnerID string, held []v1.HeldRun, reason string, now time.Time) ([]db.CancelWithdrawnClaimsRow, error) {
	ids := make([]string, 0, len(held))
	for _, r := range held {
		ids = append(ids, r.RunID)
	}
	// A JSON array rather than sqlc.slice, for the reason offer gives.
	listed, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	return q.CancelWithdrawnClaims(ctx, db.CancelWithdrawnClaimsParams{
		Reason: sql.NullString{String: reason, Valid: true}, Now: store.Ms(now),
		RunnerID: sql.NullString{String: runnerID, Valid: true}, ListedJson: string(listed),
	})
}

// holdable is whether a listed run is this runner's: offered to it and not yet
// claimed, or claimed by it and not finished.
func holdable(r db.Run, runnerID string) bool {
	if !r.RunnerID.Valid || r.RunnerID.String != runnerID {
		return false
	}
	switch r.State {
	case "offered", "claimed", "preparing", "running", "waiting":
		return true
	}
	return false
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func nullTime(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: store.Ms(*t), Valid: true}
}
