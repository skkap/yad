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
	Session *SessionChoice `json:"session,omitempty" doc:"The session the run belongs to. Absent: a new session with a generated id."`
	Harness string         `json:"harness" minLength:"1" doc:"The harness to run, by its id in a runner's capability document: claude or codex."`
	Model   string         `json:"model" minLength:"1" doc:"The model, in the harness's own terms, such as haiku or gpt-5.1-codex."`
	// Effort is queued like any run; the hub offers it only to a runner
	// advertising the effort feature for the run's harness, so on a fleet
	// without one it waits.
	Effort  string      `json:"effort,omitempty" doc:"How hard the harness thinks, in its own terms, such as low or high: at most 64 letters, digits, - and _, or the run is refused. Offered only to a runner advertising the effort feature for the run's harness. Absent: the harness's default."`
	Brief   v1.Brief    `json:"brief"`
	Sources []v1.Source `json:"sources,omitempty" doc:"What a new session's workdir is built from. A run continuing a session names the same sources or none."`
	// Grants go to the runner with the run and are never returned by this API.
	Grants       []v1.Grant `json:"grants,omitempty" doc:"Secrets for this run alone. Sent to the runner with the run, never returned by this API, and blanked in the hub's store once the run is terminal."`
	StartAt      *time.Time `json:"start_at,omitempty" doc:"A moment the run must not start before."`
	MaxWaitMS    int64      `json:"max_wait_ms,omitempty" minimum:"0" doc:"The most milliseconds the run may wait for a free account, summed over every wait; 0 is no cap."`
	WallClockMS  int64      `json:"wall_clock_ms,omitempty" minimum:"0" doc:"The most milliseconds the harness may run, not counting waits; 0 is no cap."`
	InactivityMS int64      `json:"inactivity_ms,omitempty" minimum:"0" doc:"Stop the run after this many milliseconds with no event; it can only lower the runner owner's own timeout. 0 is the owner's."`
}

// SessionChoice names the session a submitted run belongs to. New true starts
// a session with this id and is refused with 409 if the hub has one; false
// continues one the hub has, and is refused with 404 if it has none. Guessing
// either way would have a runner resume a conversation that does not exist or
// silently start over one that does.
type SessionChoice struct {
	ID  string `json:"id" minLength:"1" doc:"The session id."`
	New bool   `json:"new" doc:"true starts a session with this id, and is refused with 409 if the hub has one. false continues a session the hub has, and is refused with 404 if it has none."`
	// ForkFrom opens the session as a fork of another (decision 0065). It is
	// checked at submit against everything the hub can see then, so a fork
	// that could never be offered is refused rather than left queued.
	ForkFrom string `json:"fork_from,omitempty" doc:"With new true: start the session as a fork of this one — its conversation begins as a copy of that session's, which goes on unchanged. Refused with 404 if the hub has no such session, and with 409 if it is closed or closing, of another harness, not yet claimed by any runner (fork it once one of its runs has started), or on a runner that does not advertise the fork feature for its harness. The fork runs on that session's runner, in a workdir of its own built from this run's sources."`
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
	RunID     string   `json:"run_id" doc:"The run id."`
	SessionID string   `json:"session_id" doc:"The session the run belongs to."`
	Harness   string   `json:"harness" doc:"The harness it runs on."`
	Model     string   `json:"model" doc:"The model it runs."`
	Effort    string   `json:"effort,omitempty" doc:"The effort it was submitted with. Absent: the harness's default."`
	State     RunState `json:"state" enum:"queued,offered,claimed,preparing,running,waiting,succeeded,failed,cancelled,timed_out,lost" doc:"queued: waiting for a runner that can take it. offered: sent to a runner that has not listed it back yet. Then the protocol's states; the last five are terminal."`
	// RunnerID is the runner the run was offered to or is held by.
	RunnerID string `json:"runner_id,omitempty" doc:"The runner the run was offered to or is held by. Absent while queued."`
	// Reason says why a run is waiting or was lost.
	Reason    string     `json:"reason,omitempty" doc:"Why the run is waiting, or why it ended the way it did, for a person."`
	ResumesAt *time.Time `json:"resumes_at,omitempty" doc:"For a waiting run, when it is expected to continue."`
	// CancelRequestedAt is when a cancel was asked for a run a runner holds,
	// until the run ends; the runner stops it at its next sync.
	CancelRequestedAt *time.Time `json:"cancel_requested_at,omitempty" doc:"When a cancel was asked for a run a runner holds; present until the run ends."`
	CreatedAt         time.Time  `json:"created_at" doc:"When the run was submitted."`
	UpdatedAt         time.Time  `json:"updated_at" doc:"When its state last changed."`
	// Result is the runner's terminal report, once the hub has it. A run lost
	// by its runner is terminal with no result.
	Result *v1.Result `json:"result,omitempty" doc:"The runner's terminal report, once the hub has it. A run lost by its runner, or ended on the hub before a runner started it, is terminal with no result."`
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
	Events    []v1.Event `json:"events,omitempty" doc:"The events after the cursor, in seq order, at most 500."`
	NextAfter int64      `json:"next_after" doc:"The after to ask with next."`
	Done      bool       `json:"done" doc:"The run is terminal and every event up to its result's last_seq has been returned. Stop polling."`
	Run       Run        `json:"run" doc:"The run as it is now."`
}

// Runner is a runner as a service sees it: enough to know whether it is taking
// work.
type Runner struct {
	RunnerID string `json:"runner_id" doc:"The runner's id."`
	Name     string `json:"name" doc:"The runner's name, for display."`
	// LastSyncAt is absent for a runner that registered and never synced.
	LastSyncAt *time.Time `json:"last_sync_at,omitempty" doc:"When the runner last synced. Absent for one that registered and never synced."`
	// Draining is what the runner said at its last sync: it takes no new
	// runs, and exits once the ones it holds have ended. A runner that
	// drained and exited says it until its next process syncs.
	Draining bool `json:"draining" doc:"What the runner said at its last sync: it takes no new runs and exits once its runs have ended."`
	// DrainRequestedAt is when a drain was asked for and no sync since has
	// said the runner is draining; the runner hears it at its next sync.
	DrainRequestedAt *time.Time `json:"drain_requested_at,omitempty" doc:"When a drain was asked for that no sync has yet answered with draining."`
	// Health is what the runner's last sync said about itself: its load, its
	// free capacity, the disk under its workdirs, each harness's readiness
	// with its accounts, the depth of what it owes this hub, and its recent
	// errors. Absent for a runner that registered and never synced.
	//
	// As the runner sent it, and as old as LastSyncAt: a runner whose process
	// has gone still shows the health of its last word. Nothing here is
	// derived by the hub.
	Health *Health `json:"health,omitempty" doc:"The runner's own health at its last sync, unchanged by the hub. Absent for a runner that has never synced, and as old as last_sync_at."`
}

// Health is a runner's v1.Health as this API shows it. Its own type rather
// than v1's because the two documents promise different things: v1 lets a
// runner leave out the dashboard fields (decision 0047), while this API has
// always answered every one of them, and a service generated from it would
// break on a field it was told is always there. Where the runner left one out
// it reads 0 here, which is what v1 says the absence means.
type Health struct {
	Load          float64            `json:"load" doc:"The machine's one-minute load average, as uptime(1) prints it: the whole machine's, not divided by CPU count. 0 where the runner cannot read one."`
	FreeCapacity  v1.Capacity        `json:"free_capacity" doc:"What this sync may be offered, already net of every run the runner holds: total bounds the whole response, and each by_harness figure bounds that harness independently. Offer up to it as sent; do not subtract the runs listed beside it."`
	DiskFreeBytes int64              `json:"disk_free_bytes" doc:"Free bytes on the disk under the runner's workdirs."`
	Harnesses     []v1.HarnessHealth `json:"harnesses,omitempty" doc:"Readiness of each harness the runner can drive, with its accounts. A harness with ready false is not claimed for: an offer for it comes back unlisted."`
	SpoolDepth    int                `json:"spool_depth" doc:"Events the runner holds that this hub has not acknowledged."`
	OutboxDepth   int                `json:"outbox_depth" doc:"Results the runner holds that this hub has not acknowledged."`
	RecentErrors  []string           `json:"recent_errors,omitempty" doc:"The runner's recent warnings and errors, newest first, each '<RFC3339 time> <LEVEL> <message>'. The runner's own words: never what a harness printed, never a path on the machine, never a credential. Capped in number, age and length."`
	Draining      bool               `json:"draining,omitempty" doc:"The runner has stopped claiming and exits once the runs it holds have ended. It keeps syncing until then; offer it nothing. The answer to a drain control, which stops repeating once this is true."`
}

// HealthOf is h as this API shows it.
func HealthOf(h v1.Health) Health {
	return Health{
		Load: h.Load, FreeCapacity: h.FreeCapacity, DiskFreeBytes: h.DiskFreeBytes, Harnesses: h.Harnesses,
		SpoolDepth: h.SpoolDepth, OutboxDepth: h.OutboxDepth, RecentErrors: h.RecentErrors, Draining: h.Draining,
	}
}

// RunnerList is every runner a hub knows, the ones still syncing first.
type RunnerList struct {
	Runners []Runner `json:"runners" doc:"Every runner the hub knows, most recently synced first."`
}

// Session is a session as a service sees it.
type Session struct {
	SessionID string `json:"session_id" doc:"The session id."`
	Harness   string `json:"harness" doc:"The harness every run in the session uses."`
	// RunnerID is the runner the session is bound to — the one that claimed
	// its first run, and the only one that can continue it. Absent until then.
	RunnerID string `json:"runner_id,omitempty" doc:"The runner that claimed the session's first run, the only one that can continue it. Absent until then."`
	// ForkFrom is the session this one was opened as a fork of.
	ForkFrom string       `json:"fork_from,omitempty" doc:"The session this one was started as a fork of. Absent for a session that started its own conversation."`
	State    SessionState `json:"state" enum:"open,closing,closed" doc:"open takes runs. closing: a close was asked for and its runner has not yet done it. closed takes no new run."`
	// CloseRequestedAt is when a close was asked for, until the runner says
	// it is done; the runner hears it at its next sync.
	CloseRequestedAt *time.Time `json:"close_requested_at,omitempty" doc:"When a close was asked for, until the runner reports it done."`
	ClosedAt         *time.Time `json:"closed_at,omitempty" doc:"When the session closed."`
	// CloseReason is why it closed: closed (asked for here), closed_by_owner
	// (on the runner's machine), expired (idle past the owner's TTL) or
	// disk_pressure — protocol/v1's SessionCloseReason.
	CloseReason string    `json:"close_reason,omitempty" doc:"Why it closed: closed (asked for through this API), closed_by_owner (on the runner's machine, or its runner deregistered), expired (idle past the owner's TTL) or disk_pressure."`
	CreatedAt   time.Time `json:"created_at" doc:"When the session's first run was submitted."`
}

// SessionState is where a session is: open takes runs; closing has a close
// asked for that its runner has not yet done; closed takes no new run, and
// its workdir is gone or going on its runner.
type SessionState string

const (
	SessionOpen    SessionState = "open"
	SessionClosing SessionState = "closing"
	SessionClosed  SessionState = "closed"
)

// SteerRequest is input for a running turn.
type SteerRequest struct {
	Text string `json:"text" minLength:"1" maxLength:"65536" doc:"What to tell the harness, as a user message."`
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
