package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// fallbackPoll is how often a waiting events request looks at the store even
// when nothing rang. The hub's own writes ring at once; this covers writes
// that do not pass through it, such as another process on the same database.
const fallbackPoll = time.Second

// ServiceConfig is the OpenAPI frame of the service API. Like the protocol's,
// its version is the API's own and never the binary's.
func ServiceConfig() huma.Config {
	c := huma.DefaultConfig("YAD hub service API", hubapi.Version+".0.0")
	c.Info.Description = "How a service or a person submits runs to a standalone `yad hub` and follows them. " +
		"Not the runner protocol: hubs that embed the protocol never implement this. Paths are relative to " + hubapi.BasePath + "."
	c.Servers = []*huma.Server{{URL: hubapi.BasePath, Description: "where `yad hub` mounts it"}}
	c.DocsPath = ""
	c.SchemasPath = ""
	// Not served: the committed protocol/hubapi/openapi.yaml is the contract,
	// and every path here needs the admin token anyway.
	c.OpenAPIPath = ""
	c.CreateHooks = nil
	c.AllowAdditionalPropertiesByDefault = true
	c.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"admin": {Type: "http", Scheme: "bearer", Description: "An admin token from `yad hub admin-token create`. Runner credentials are refused."},
	}
	return c
}

type (
	submitInput struct{ Body hubapi.SubmitRequest }
	runOutput   struct{ Body hubapi.Run }
	getRunInput struct {
		Run string `path:"run" doc:"The run id."`
	}
	eventsPageInput struct {
		Run    string `path:"run" doc:"The run id."`
		After  int64  `query:"after" minimum:"0" doc:"Return events with a seq above this; 0 for the first page."`
		WaitMS int64  `query:"wait_ms" minimum:"0" default:"25000" doc:"How long to hold the request open when there is nothing new; at most 50000. 0 answers at once."`
	}
	eventsPageOutput struct{ Body hubapi.EventPage }
)

var adminSecurity = []map[string][]string{{"admin": {}}}

func (h *Hub) registerService(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "submitRun", Method: http.MethodPost, Path: "/runs",
		Summary: "Queue a run",
		Description: "Answers with the queued run. Submitting a run_id the hub already has gives the same answer, with the run " +
			"as it is now, when the content is the same — a safe retry — and 409 when it differs.",
		DefaultStatus: http.StatusCreated,
		Security:      adminSecurity, Errors: []int{400, 401, 404, 409},
	}, h.submitRun)

	huma.Register(api, huma.Operation{
		OperationID: "getRun", Method: http.MethodGet, Path: "/runs/{run}",
		Summary:  "Read a run's state, and its result once it has one",
		Security: adminSecurity, Errors: []int{401, 404},
	}, func(ctx context.Context, in *getRunInput) (*runOutput, error) {
		var view hubapi.Run
		err := h.store.Tx(ctx, func(q *db.Queries) (err error) {
			view, err = runView(ctx, q, in.Run)
			return err
		})
		if err != nil {
			return nil, err
		}
		return &runOutput{Body: view}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "runEvents", Method: http.MethodGet, Path: "/runs/{run}/events",
		Summary: "Long-poll a run's events",
		Description: "Answers at once with every event after `after` (up to a page), or when the run's state moves, or when it " +
			"finishes; otherwise holds the request up to wait_ms and answers with no events. Ask again with after = next_after " +
			"until done, which is true once the run is terminal and every event it will have has been returned.",
		Security: adminSecurity, Errors: []int{400, 401, 404},
	}, h.runEvents)
}

func (h *Hub) submitRun(ctx context.Context, in *submitInput) (*runOutput, error) {
	req := in.Body
	run := v1.Run{
		RunID: req.RunID, Harness: req.Harness, Model: req.Model, Brief: req.Brief,
		Sources: req.Sources, Grants: req.Grants, StartAt: req.StartAt,
		MaxWaitMS: req.MaxWaitMS, WallClockMS: req.WallClockMS, InactivityMS: req.InactivityMS,
		Session: v1.SessionRef{New: true, Mode: v1.SessionPerRun},
	}
	if req.Session != nil {
		run.Session.ID, run.Session.New = req.Session.ID, req.Session.New
	} else {
		run.Session.ID = newID("ses_")
	}
	if run.RunID == "" {
		run.RunID = newID("run_")
	}
	for _, id := range []struct{ field, v string }{{"run_id", run.RunID}, {"session.id", run.Session.ID}} {
		// Both become path segments and database keys, on this hub and on the runner.
		if !runnerIDPattern.MatchString(id.v) {
			return nil, Fail(http.StatusBadRequest, v1.CodeInvalid,
				id.field+" must be 1–128 letters, digits, dots, dashes or underscores, starting with a letter or digit",
				"pick another id, or leave it out and the hub generates one")
		}
	}
	if err := run.Validate(); err != nil {
		return nil, Fail(http.StatusBadRequest, v1.CodeInvalid, err.Error(), "fix the request to match protocol/hubapi/openapi.yaml")
	}

	err := h.store.EnqueueRun(ctx, run, h.now())
	switch {
	case errors.Is(err, store.ErrRunExists):
		same, err := h.sameRun(ctx, run, req.Session == nil)
		if err != nil {
			return nil, err
		}
		if !same {
			return nil, Fail(http.StatusConflict, v1.CodeConflict,
				fmt.Sprintf("the hub already has run %q, with different content", run.RunID),
				"use a new run_id for a new run; resubmit the same content only to retry")
		}
	case errors.Is(err, store.ErrSessionExists):
		return nil, Fail(http.StatusConflict, v1.CodeConflict, fmt.Sprintf("session %q already exists", run.Session.ID),
			"set session.new to false to continue it, or pick a new session id")
	case errors.Is(err, store.ErrNoSession):
		return nil, Fail(http.StatusNotFound, v1.CodeNotFound, fmt.Sprintf("this hub has no session %q", run.Session.ID),
			"set session.new to true to start it, or check the id against an earlier run's session_id")
	case errors.Is(err, store.ErrSessionHarness):
		return nil, Fail(http.StatusConflict, v1.CodeConflict, err.Error(), "continue the session with its own harness, or start a new session")
	case err != nil:
		return nil, err
	default:
		h.bell.ring()
	}

	var view hubapi.Run
	err = h.store.Tx(ctx, func(q *db.Queries) (err error) {
		view, err = runView(ctx, q, run.RunID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &runOutput{Body: view}, nil
}

// sameRun reports whether a resubmitted run is the one the hub already holds.
// A caller that let the hub pick the session cannot send its id again, so
// then the session id is not compared — but the stored run must have started
// its session too: a retry asking for a new session is not the run that
// continued an old one.
func (h *Hub) sameRun(ctx context.Context, run v1.Run, anySession bool) (bool, error) {
	stored, err := h.store.GetRun(ctx, run.RunID)
	if err != nil {
		return false, err
	}
	if anySession {
		var was v1.Run
		if err := json.Unmarshal([]byte(stored.Spec), &was); err != nil {
			return false, fmt.Errorf("stored run %s: %w", stored.ID, err)
		}
		if !was.Session.New {
			return false, nil
		}
		run.Session = was.Session
	}
	b, err := json.Marshal(run)
	if err != nil {
		return false, err
	}
	return bytes.Equal(b, []byte(stored.Spec)), nil
}

func (h *Hub) runEvents(ctx context.Context, in *eventsPageInput) (*eventsPageOutput, error) {
	wait := min(time.Duration(in.WaitMS)*time.Millisecond, hubapi.MaxWait)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	poll := time.NewTicker(fallbackPoll)
	defer poll.Stop()

	var started hubapi.RunState
	for first := true; ; first = false {
		// Taken before the read, so a write landing between the read and
		// the wait still wakes this request.
		rang := h.bell.wait()
		page, err := h.eventPage(ctx, in.Run, in.After)
		if err != nil {
			return nil, err
		}
		if first {
			started = page.Run.State
		}
		if len(page.Events) > 0 || page.Done || page.Run.State != started || wait == 0 {
			return &eventsPageOutput{Body: page}, nil
		}
		select {
		case <-rang:
		case <-poll.C:
		case <-deadline.C:
			return &eventsPageOutput{Body: page}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// eventPage reads the run and its events after the cursor in one transaction,
// so Done never claims the end of a stream the same read did not see.
func (h *Hub) eventPage(ctx context.Context, runID string, after int64) (hubapi.EventPage, error) {
	page := hubapi.EventPage{NextAfter: after}
	err := h.store.Tx(ctx, func(q *db.Queries) error {
		view, err := runView(ctx, q, runID)
		if err != nil {
			return err
		}
		page.Run = view
		rows, err := q.EventsAfter(ctx, db.EventsAfterParams{RunID: runID, Seq: after, Limit: hubapi.MaxPage})
		if err != nil {
			return err
		}
		for _, r := range rows {
			var ev v1.Event
			if err := json.Unmarshal([]byte(r.Body), &ev); err != nil {
				return fmt.Errorf("stored event %s/%d: %w", runID, r.Seq, err)
			}
			page.Events = append(page.Events, ev)
			page.NextAfter = r.Seq
		}
		// The result names the run's last event, and may arrive before the
		// runner's final batch: until that event is here, there is more.
		// A run lost by its runner has no result, and ends with what came.
		caughtUp := len(rows) < hubapi.MaxPage
		if view.Result != nil {
			caughtUp = caughtUp && page.NextAfter >= view.Result.LastSeq
		}
		page.Done = view.State.Terminal() && caughtUp
		return nil
	})
	return page, err
}

// runView is a run as the service API shows it. The stored spec, which holds
// the grants, is never read into it.
func runView(ctx context.Context, q *db.Queries, runID string) (hubapi.Run, error) {
	r, err := q.GetRun(ctx, runID)
	if errors.Is(err, sql.ErrNoRows) {
		return hubapi.Run{}, Fail(http.StatusNotFound, v1.CodeNotFound, fmt.Sprintf("this hub has no run %q", runID),
			"check the run id — `yad hub submit` prints it")
	}
	if err != nil {
		return hubapi.Run{}, err
	}
	view := hubapi.Run{
		RunID: r.ID, SessionID: r.SessionID, Harness: r.Harness, Model: r.Model, State: hubapi.RunState(r.State),
		RunnerID: r.RunnerID.String, Reason: r.Reason.String,
		CreatedAt: time.UnixMilli(r.CreatedAt).UTC(), UpdatedAt: time.UnixMilli(r.UpdatedAt).UTC(),
	}
	if r.ResumesAt.Valid {
		t := time.UnixMilli(r.ResumesAt.Int64).UTC()
		view.ResumesAt = &t
	}
	res, err := q.GetResult(ctx, runID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return hubapi.Run{}, err
	default:
		var body v1.Result
		if err := json.Unmarshal([]byte(res.Body), &body); err != nil {
			return hubapi.Run{}, fmt.Errorf("stored result %s: %w", runID, err)
		}
		view.Result = &body
	}
	return view, nil
}

// newID is a hub-generated run or session id. 80 random bits never collide
// within one hub; a runner keys ids by connection, so two hubs never meet.
func newID(prefix string) string {
	b := make([]byte, 10)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on the platforms yad builds for
	return prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

// bell wakes every waiting events request when something they may be waiting
// for was written. Each waiter takes the current channel; a ring closes it and
// starts a new one.
type bell struct {
	mu sync.Mutex
	ch chan struct{}
}

func (b *bell) wait() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ch == nil {
		b.ch = make(chan struct{})
	}
	return b.ch
}

func (b *bell) ring() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ch != nil {
		close(b.ch)
		b.ch = nil
	}
}

// Changed wakes waiting events requests after a write that did not go through
// the hub's own handlers — a test writing through the store, for one. They
// would notice within fallbackPoll anyway; this makes it immediate.
func (h *Hub) Changed() { h.bell.ring() }
