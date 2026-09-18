package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	}

	err = h.store.Tx(ctx, func(q *db.Queries) error {
		// Lapsed leases are settled before this sync is judged, so a runner
		// back from a long absence hears that its runs were lost rather than
		// renewing them.
		if err := sweep(ctx, q, now); err != nil {
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
		if err := q.RecordSync(ctx, db.RecordSyncParams{
			LastSyncAt: sql.NullInt64{Int64: store.Ms(now), Valid: true}, Health: sql.NullString{String: string(health), Valid: true},
			WantsCapabilities: boolInt(wants), ID: runner.ID,
		}); err != nil {
			return err
		}
		if wants {
			out.Controls = append(out.Controls, v1.Control{Kind: v1.ControlReportCapabilities})
		}

		for _, held := range req.Runs {
			run, err := q.GetRun(ctx, held.RunID)
			if errors.Is(err, sql.ErrNoRows) || err == nil && !holdable(run, runner.ID) {
				// Not this runner's to hold: it must stop, and nothing it
				// reports for the run will be applied.
				out.Controls = append(out.Controls, v1.Control{Kind: v1.ControlCancel, RunID: held.RunID})
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
				if err := q.BindSession(ctx, db.BindSessionParams{RunnerID: me, ID: run.SessionID}); err != nil {
					return err
				}
			}
		}

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

		var doc v1.Capabilities
		if err := json.Unmarshal([]byte(docJSON), &doc); err != nil {
			return fmt.Errorf("stored capability document for %s: %w", runner.ID, err)
		}
		out.Runs, err = h.offer(ctx, q, runner.ID, doc, req.Health.FreeCapacity, lease, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &syncOutput{Body: out}, nil
}

// offer picks queued runs for this runner and marks them offered. Never more
// than the free capacity it declared, in total or for any harness it capped,
// and never a harness it cannot drive: the runner would have to refuse it.
func (h *Hub) offer(ctx context.Context, q *db.Queries, runnerID string, doc v1.Capabilities, free v1.Capacity, lease sql.NullInt64, now time.Time) ([]v1.Run, error) {
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
	var (
		runs   []v1.Run
		cursor db.Run
	)
	for len(runs) < free.Total {
		harnesses := takeable()
		if len(harnesses) == 0 {
			break
		}
		// A JSON array rather than sqlc.slice: sqlc numbers its parameters,
		// and expanding a slice of two or more shifted every one after it.
		list, err := json.Marshal(harnesses)
		if err != nil {
			return nil, err
		}
		page, err := q.OfferCandidates(ctx, db.OfferCandidatesParams{
			HarnessesJson: string(list), AfterCreatedAt: cursor.CreatedAt, AfterID: cursor.ID, RunnerID: me, Max: candidatePage,
		})
		if err != nil {
			return nil, err
		}
		for _, c := range page {
			cursor = c
			if len(runs) >= free.Total {
				break
			}
			if n, capped := left[c.Harness]; capped && n <= 0 {
				continue
			}
			var run v1.Run
			if err := json.Unmarshal([]byte(c.Spec), &run); err != nil {
				return nil, fmt.Errorf("stored run %s: %w", c.ID, err)
			}
			if err := q.OfferRun(ctx, db.OfferRunParams{RunnerID: me, LeaseExpiresAt: lease, UpdatedAt: store.Ms(now), ID: c.ID}); err != nil {
				return nil, err
			}
			if _, capped := left[c.Harness]; capped {
				left[c.Harness]--
			}
			runs = append(runs, run)
		}
		if len(page) < candidatePage {
			break
		}
	}
	return runs, nil
}

// Sweep withdraws offers and loses runs whose leases lapsed. Every sync sweeps
// first; `yad hub serve` also sweeps on a timer, so a hub whose only runner
// went away still marks that runner's runs lost.
func (h *Hub) Sweep(ctx context.Context) error {
	return h.store.Tx(ctx, func(q *db.Queries) error { return sweep(ctx, q, h.now()) })
}

// SweepEvery is how often `yad hub serve` should call Sweep.
func (h *Hub) SweepEvery() time.Duration { return h.interval }

func sweep(ctx context.Context, q *db.Queries, now time.Time) error {
	if _, err := q.RequeueWithdrawnOffers(ctx, store.Ms(now)); err != nil {
		return err
	}
	_, err := q.LoseLapsedRuns(ctx, store.Ms(now))
	return err
}

// authenticate finds the runner the bearer credential belongs to, and checks
// it is the runner the path names.
func (h *Hub) authenticate(ctx context.Context, pathRunner string) (db.Runner, error) {
	cred := bearer(ctx)
	if cred == "" {
		return db.Runner{}, Fail(http.StatusUnauthorized, v1.CodeUnauthorized, "no runner credential", "run `yad connect` to register this runner")
	}
	r, err := h.store.GetRunnerByCredential(ctx, hashSecret(cred))
	if errors.Is(err, sql.ErrNoRows) {
		return db.Runner{}, Fail(http.StatusUnauthorized, v1.CodeUnauthorized,
			"the hub does not know this runner credential — a newer registration replaced it, or the hub's database was reset",
			newTokenAction)
	}
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
