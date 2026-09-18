package v1

import "time"

// Run is one turn to execute against one session. It names its session,
// harness and model explicitly; none is inferred from runner configuration.
//
// There is deliberately no permission, sandbox or tool-policy field: those are
// the runner owner's configuration, and a hub must not be able to widen them.
type Run struct {
	RunID   string     `json:"run_id"`
	Session SessionRef `json:"session"`
	Harness string     `json:"harness"`
	Model   string     `json:"model,omitempty"`
	Brief   Brief      `json:"brief"`
	Sources []Source   `json:"sources,omitempty"`
	Grants  []Grant    `json:"grants,omitempty"`
	// StartAt is a one-shot moment the run must not start before, like an email
	// API's send_at. There is no recurrence anywhere in the protocol.
	StartAt      *time.Time `json:"start_at,omitempty"`
	MaxWaitMS    int64      `json:"max_wait_ms,omitempty"`
	WallClockMS  int64      `json:"wall_clock_ms,omitempty"`
	InactivityMS int64      `json:"inactivity_ms,omitempty"`
}

// SessionMode is how the runner holds the harness process for a session.
type SessionMode string

const (
	SessionPerRun SessionMode = "per_run"
	// SessionLive is reserved: refused until a runner advertises the
	// "live_sessions" feature.
	SessionLive SessionMode = "live"
)

// SessionRef names the session a run belongs to.
type SessionRef struct {
	ID   string      `json:"id"`
	New  bool        `json:"new"`
	Mode SessionMode `json:"mode,omitempty" enum:"per_run,live"`
}

// Brief is what the run is told: context goes into the harness's system prompt
// so it survives compaction; the instruction is the one user turn.
type Brief struct {
	Context     string `json:"context,omitempty"`
	Instruction string `json:"instruction"`
}

// Source is one input the workdir is built from: a git repository or an
// existing local path. Exactly one field is set.
type Source struct {
	Git  *GitSource `json:"git,omitempty"`
	Path string     `json:"path,omitempty"`
}

// GitSource is a repository to check out as a worktree on Branch, cut from Base.
type GitSource struct {
	URL    string `json:"url"`
	Base   string `json:"base,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// GrantDelivery is how a grant reaches the harness. Never argv.
type GrantDelivery string

const (
	GrantEnv  GrantDelivery = "env"
	GrantFile GrantDelivery = "file"
)

// Grant is a short-lived secret scoped to one run.
type Grant struct {
	Name  string        `json:"name"`
	Value string        `json:"value"`
	As    GrantDelivery `json:"as" enum:"env,file"`
}

// RunState is where a run is. The set is closed and mirrored by DOMAIN.md's
// **Run state** entry; a parity test keeps them equal.
type RunState string

const (
	RunClaimed   RunState = "claimed"
	RunPreparing RunState = "preparing"
	RunRunning   RunState = "running"
	// RunWaiting holds no process: the run is parked on a usage limit with a
	// resume time, and survives a runner restart.
	RunWaiting   RunState = "waiting"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	RunCancelled RunState = "cancelled"
	RunTimedOut  RunState = "timed_out"
	RunLost      RunState = "lost"
)

// RunStates lists the closed set in lifecycle order.
func RunStates() []RunState {
	return []RunState{RunClaimed, RunPreparing, RunRunning, RunWaiting, RunSucceeded, RunFailed, RunCancelled, RunTimedOut, RunLost}
}

// IsTerminal reports whether a run in this state is finished for good. A run
// reaches exactly one terminal state and never leaves it.
func (s RunState) IsTerminal() bool {
	switch s {
	case RunSucceeded, RunFailed, RunCancelled, RunTimedOut, RunLost:
		return true
	}
	return false
}

// Valid reports whether s is in the closed set.
func (s RunState) Valid() bool {
	for _, v := range RunStates() {
		if s == v {
			return true
		}
	}
	return false
}
