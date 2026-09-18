// Package hubapi is `yad hub`'s service API: how a service or a person puts a
// run into a standalone hub and follows it. It is not the runner protocol — a
// hub that embeds protocol/v1 (Zumino, yashiki) has its own way to make runs
// and never implements this — so it has its own base path, its own token and
// its own generated openapi.yaml beside this file (decision 0022).
//
// It reuses the protocol's types for everything a run already says — brief,
// sources, grants, events, result — so a service that knows one knows both.
// The same wire rules hold: an absent list is an empty list, unknown fields are
// ignored, and renames or removals are a new version, never an edit.
package hubapi

import (
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// Version is the API's major version, carried in BasePath.
const Version = "1"

// BasePath is where `yad hub` mounts this API, beside the protocol's /v1.
const BasePath = "/api/v1"

// SubmitRequest puts one run in the hub's queue. It is the protocol's Run
// minus what the hub decides: ids are generated when absent, and the session
// mode is always per_run until live sessions exist.
//
// RunID is how a service retries safely: submitting the same run id with the
// same content again answers the run already queued, and with different
// content is refused with 409.
type SubmitRequest struct {
	RunID string `json:"run_id,omitempty" doc:"Chosen by the caller to make a retried submit idempotent; generated when absent."`
	// Session absent starts a new session with a generated id.
	Session *SessionChoice `json:"session,omitempty"`
	Harness string         `json:"harness" minLength:"1"`
	Model   string         `json:"model" minLength:"1"`
	Brief   v1.Brief       `json:"brief"`
	Sources []v1.Source    `json:"sources,omitempty"`
	// Grants go to the runner with the run and are never returned by this API.
	Grants       []v1.Grant `json:"grants,omitempty"`
	StartAt      *time.Time `json:"start_at,omitempty"`
	MaxWaitMS    int64      `json:"max_wait_ms,omitempty" minimum:"0"`
	WallClockMS  int64      `json:"wall_clock_ms,omitempty" minimum:"0"`
	InactivityMS int64      `json:"inactivity_ms,omitempty" minimum:"0"`
}

// SessionChoice names the session a submitted run belongs to. New true starts
// a session with this id and is refused with 409 if the hub has one; false
// continues one the hub has, and is refused with 404 if it has none. Guessing
// either way would have a runner resume a conversation that does not exist or
// silently start over one that does.
type SessionChoice struct {
	ID  string `json:"id" minLength:"1"`
	New bool   `json:"new"`
}

// RunState is where a run is, as the hub sees it: the protocol's run states
// plus the two that exist only on a hub, before a runner has it.
type RunState string

const (
	// RunQueued waits for a runner that can take it.
	RunQueued RunState = "queued"
	// RunOffered was sent to a runner that has not yet listed it back.
	RunOffered RunState = "offered"
)

// Terminal reports whether a run in state s is finished for good.
func (s RunState) Terminal() bool { return v1.RunState(s).IsTerminal() }

// Run is a run as a service sees it. Grants are never included.
type Run struct {
	RunID     string   `json:"run_id"`
	SessionID string   `json:"session_id"`
	Harness   string   `json:"harness"`
	Model     string   `json:"model"`
	State     RunState `json:"state" enum:"queued,offered,claimed,preparing,running,waiting,succeeded,failed,cancelled,timed_out,lost"`
	// RunnerID is the runner the run was offered to or is held by.
	RunnerID string `json:"runner_id,omitempty"`
	// Reason says why a run is waiting or was lost.
	Reason    string     `json:"reason,omitempty"`
	ResumesAt *time.Time `json:"resumes_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	// Result is the runner's terminal report, once the hub has it. A run lost
	// by its runner is terminal with no result.
	Result *v1.Result `json:"result,omitempty"`
}

// EventPage is one answer to a long poll on a run's events: every event after
// the cursor that the hub has, up to a page, and the run as it is now.
//
// A caller follows a run by asking again with After set to NextAfter until
// Done. Done is true once the run is terminal and its stream is complete:
// every event up to the result's last_seq has been returned. A result can
// arrive before the runner's final batch of events, so a terminal run whose
// held events are all returned is not yet done until those arrive. A run lost
// by its runner has no result, and is done once the events the hub holds are
// returned.
type EventPage struct {
	Events    []v1.Event `json:"events,omitempty"`
	NextAfter int64      `json:"next_after"`
	Done      bool       `json:"done"`
	Run       Run        `json:"run"`
}

// Longest and default time the hub holds an events request open when there is
// nothing new. Under a minute, so the common proxy idle timeout of 60 s never
// cuts a poll that is working as intended.
const (
	DefaultWait = 25 * time.Second
	MaxWait     = 50 * time.Second
)

// MaxPage is the most events one page carries.
const MaxPage = 500
