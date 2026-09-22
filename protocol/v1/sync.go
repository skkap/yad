package v1

import "time"

// SyncRequest is the periodic call: heartbeat, lease renewal for every run
// listed, health report and the ask for work, in one request.
type SyncRequest struct {
	RunnerID    string `json:"runner_id"`
	Fingerprint string `json:"fingerprint"`
	// Capabilities is sent only when the hub asked for it with
	// ControlReportCapabilities, or on the first sync after a fingerprint move.
	Capabilities *Capabilities `json:"capabilities,omitempty"`
	Health       Health        `json:"health"`
	// Runs lists every run the runner holds. Listing a run is how it is claimed
	// and how its lease is renewed; a run the hub offered and this list omits
	// was never received.
	Runs []HeldRun `json:"runs,omitempty"`
	// ClosedSessions are sessions this runner has closed and whose workdirs
	// it reclaims, each listed in every sync until one carrying it is
	// answered with a 2xx — at least once, so a hub records them by session
	// id and takes a repeat as the same news. A hub stops offering runs in a
	// session listed here: the runner refuses them. Sent by runners that
	// advertise the "close_session" feature, which also act on the
	// close_session control.
	ClosedSessions []ClosedSession `json:"closed_sessions,omitempty"`
}

// ClosedSession is one session a runner has closed, and why.
type ClosedSession struct {
	SessionID string             `json:"session_id"`
	Reason    SessionCloseReason `json:"reason" enum:"closed,closed_by_owner,expired,disk_pressure"`
	ClosedAt  time.Time          `json:"closed_at"`
}

// SessionCloseReason is why a runner closed a session. A hub may tell them
// apart to say what happened; every one of them means the same thing for
// routing — the session is gone, and a new run needs a new session.
type SessionCloseReason string

const (
	// SessionClosed — the hub asked, with the close_session control.
	SessionClosed SessionCloseReason = "closed"
	// SessionClosedByOwner — the runner's owner closed it on the machine
	// (`yad sessions close`).
	SessionClosedByOwner SessionCloseReason = "closed_by_owner"
	// SessionExpired — nothing ran in it for the owner's idle TTL.
	SessionExpired SessionCloseReason = "expired"
	// SessionDiskPressure — the disk under the workdirs fell below the
	// owner's floor, and this was among the longest idle.
	SessionDiskPressure SessionCloseReason = "disk_pressure"
)

// Health is the runner's state as a hub needs it for routing and alerting.
//
// Every field of it is computed on every sync, so each one is a read the
// runner can afford every few seconds, and the lists are capped: a health
// block that grows with the machine's uptime makes every sync slower for ever.
type Health struct {
	// Load is the machine's one-minute load average, as uptime(1) prints it —
	// the whole machine, not this runner's share, and not divided by CPU
	// count, which the capability document does not carry. 0 from a machine
	// this runner cannot read one from.
	Load          float64         `json:"load" doc:"The machine's one-minute load average, as uptime(1) prints it: the whole machine's, not divided by CPU count. 0 where the runner cannot read one."`
	FreeCapacity  Capacity        `json:"free_capacity"`
	DiskFreeBytes int64           `json:"disk_free_bytes"`
	Harnesses     []HarnessHealth `json:"harnesses,omitempty"`
	SpoolDepth    int             `json:"spool_depth"`
	OutboxDepth   int             `json:"outbox_depth"`
	// RecentErrors are the runner's own recent warnings and errors, newest
	// first, each "<RFC3339 time> <LEVEL> <message>" — why this runner is
	// slow or idle, in the words its owner sees in `yad status`.
	//
	// The runner's words and only those: the message of a log record, never
	// the key=value attrs beside it, which is where a wrapped error's text, a
	// path on the machine and anything a harness printed live. Bounded in
	// number, in age and in length by the runner.
	RecentErrors []string `json:"recent_errors,omitempty" doc:"The runner's recent warnings and errors, newest first, each '<RFC3339 time> <LEVEL> <message>'. The runner's own words: never what a harness printed, never a path on the machine, never a credential. Capped in number, age and length."`
	// Draining says the runner has stopped claiming and exits once the runs
	// it holds have ended. It keeps syncing until then, so their leases renew
	// and their results land; its free capacity is zero, and a hub offers it
	// nothing. Sent by runners that advertise the "drain" feature, which also
	// act on the drain control.
	Draining bool `json:"draining,omitempty"`
}

// HarnessHealth is per-harness readiness with each account's state: limited
// until a reset, needing a login the owner has to finish, or free.
//
// Ready is whether the harness can take a run now — at least one free account,
// or none configured at all, since a harness with no accounts runs on the
// harness's own login. It is not a claim about the binary being installed;
// only harnesses the runner can drive appear here.
type HarnessHealth struct {
	ID       string          `json:"id"`
	Ready    bool            `json:"ready"`
	Accounts []AccountReport `json:"accounts,omitempty"`
}

// HeldRun is one run's non-terminal state. Terminal states travel in a Result.
type HeldRun struct {
	RunID     string     `json:"run_id"`
	State     RunState   `json:"state" enum:"claimed,preparing,running,waiting"`
	ResumesAt *time.Time `json:"resumes_at,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

// SyncResponse is what the hub wants done until the next sync.
type SyncResponse struct {
	NextSyncMS int `json:"next_sync_ms"`
	// LeaseMS covers offers as well as claims (decision 0046): a runner that
	// goes silent holding an offer strands nothing past it.
	LeaseMS    int       `json:"lease_ms" doc:"How long the hub holds each run named in this answer for this runner without hearing from it: every run offered here, and every run the request listed. A listed run the lease lapses on is lost. An offered run the lease lapses on, never claimed, goes back in the queue and may be offered to any runner; a claim listed after that is answered with a cancel. Never shorter than next_sync_ms."`
	Runs       []Run     `json:"runs,omitempty"`
	Controls   []Control `json:"controls,omitempty"`
	MinVersion string    `json:"min_version,omitempty"`
}

// ControlKind is the closed set of instructions a hub can give in a sync.
type ControlKind string

const (
	ControlCancel             ControlKind = "cancel"
	ControlInterrupt          ControlKind = "interrupt"
	ControlSteer              ControlKind = "steer"
	ControlCloseSession       ControlKind = "close_session"
	ControlDrain              ControlKind = "drain"
	ControlReportCapabilities ControlKind = "report_capabilities"
	// ControlUpdate is reserved (decision 0018): a runner that does not
	// implement self-update ignores it and keeps reporting its version.
	ControlUpdate ControlKind = "update"
)

// ControlKinds lists the closed set, for validation and the parity test.
func ControlKinds() []ControlKind {
	return []ControlKind{ControlCancel, ControlInterrupt, ControlSteer, ControlCloseSession, ControlDrain, ControlReportCapabilities, ControlUpdate}
}

// Control is one instruction. Which of the optional fields are set depends on
// the kind: runs for cancel/interrupt/steer, sessions for close_session, none
// for drain — which a hub sends only to a runner advertising the "drain"
// feature, and repeats until the runner's health says it is draining.
//
// close_session goes only to a runner advertising the "close_session"
// feature, and is repeated until the session appears in the runner's
// closed_sessions. A session with a run held closes once that run ends; a
// session the runner does not hold, or already closed, is reported closed.
//
// steer and interrupt are gated the same way, on the "steer" and "interrupt"
// features. Only cancel and report_capabilities go to every v1 runner; update
// is reserved and goes to none. Nothing acknowledges a control, so one sent to
// a runner that does not act on it is indistinguishable, to whoever asked for
// it, from one that was obeyed.
type Control struct {
	Kind      ControlKind `json:"kind" enum:"cancel,interrupt,steer,close_session,drain,report_capabilities,update"`
	RunID     string      `json:"run_id,omitempty"`
	SessionID string      `json:"session_id,omitempty"`
	Text      string      `json:"text,omitempty"`
}
