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
}

// Health is the runner's state as a hub needs it for routing and alerting.
type Health struct {
	Load          float64         `json:"load"`
	FreeCapacity  Capacity        `json:"free_capacity"`
	DiskFreeBytes int64           `json:"disk_free_bytes"`
	Harnesses     []HarnessHealth `json:"harnesses,omitempty"`
	SpoolDepth    int             `json:"spool_depth"`
	OutboxDepth   int             `json:"outbox_depth"`
	RecentErrors  []string        `json:"recent_errors,omitempty"`
	// Draining says the runner has stopped claiming and exits once the runs
	// it holds have ended. It keeps syncing until then, so their leases renew
	// and their results land; its free capacity is zero, and a hub offers it
	// nothing. Sent by runners that advertise the "drain" feature, which also
	// act on the drain control.
	Draining bool `json:"draining,omitempty"`
}

// HarnessHealth is per-harness readiness, including which accounts are limited.
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
	NextSyncMS int       `json:"next_sync_ms"`
	LeaseMS    int       `json:"lease_ms"`
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
type Control struct {
	Kind      ControlKind `json:"kind" enum:"cancel,interrupt,steer,close_session,drain,report_capabilities,update"`
	RunID     string      `json:"run_id,omitempty"`
	SessionID string      `json:"session_id,omitempty"`
	Text      string      `json:"text,omitempty"`
}
