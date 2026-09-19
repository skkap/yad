package v1

import (
	"errors"
	"fmt"
	"time"
)

// Run is one turn to execute against one session. It names its session,
// harness and model explicitly; none is inferred from runner configuration.
//
// There is deliberately no permission, sandbox or tool-policy field: those are
// the runner owner's configuration, and a hub must not be able to widen them.
type Run struct {
	RunID   string     `json:"run_id"`
	Session SessionRef `json:"session"`
	Harness string     `json:"harness"`
	// Model is required: a run names its model, and a harness left to its own
	// default runs something different, and differently priced, from what the
	// hub asked for.
	Model   string   `json:"model"`
	Brief   Brief    `json:"brief"`
	Sources []Source `json:"sources,omitempty"`
	Grants  []Grant  `json:"grants,omitempty"`
	// StartAt is a one-shot moment the run must not start before, like an email
	// API's send_at. There is no recurrence anywhere in the protocol. A hub
	// offers a run carrying one only to a runner advertising the "start_at"
	// feature, until the moment has passed: any other runner would start it on
	// arrival.
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

// Validate checks what the schema cannot express, and what both sides must
// check anyway: a hub before offering a run, a runner before claiming one.
func (r Run) Validate() error {
	var errs []error
	for _, f := range []struct{ name, v string }{
		{"run_id", r.RunID}, {"session.id", r.Session.ID}, {"harness", r.Harness},
		{"model", r.Model}, {"brief.instruction", r.Brief.Instruction},
	} {
		if f.v == "" {
			errs = append(errs, fmt.Errorf("%s is required", f.name))
		}
	}
	switch r.Session.Mode {
	case "", SessionPerRun, SessionLive:
	default:
		errs = append(errs, fmt.Errorf("session.mode %q is not per_run or live", r.Session.Mode))
	}
	for i, src := range r.Sources {
		if err := src.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("sources[%d]: %w", i, err))
		}
	}
	seen := map[string]bool{}
	for i, g := range r.Grants {
		if err := g.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("grants[%d]: %w", i, err))
		}
		// Two grants by one name: one would silently replace the other.
		if seen[g.Name] {
			errs = append(errs, fmt.Errorf("grants[%d]: grant name %s is given twice", i, g.Name))
		}
		seen[g.Name] = true
	}
	return errors.Join(errs...)
}

// Validate enforces exactly one of git or path. The generated schema cannot
// say "exactly one", so this is where the rule lives.
func (s Source) Validate() error {
	switch {
	case s.Git != nil && s.Path != "":
		return errors.New("set git or path, not both")
	case s.Git != nil && s.Git.URL == "":
		return errors.New("git.url is required")
	case s.Git == nil && s.Path == "":
		return errors.New("set git or path")
	}
	return nil
}
