// Package adapter is the seam between the runner and a harness.
//
// An adapter knows one harness: how to start a turn, how to translate its stream
// into protocol events, how to resume, steer and interrupt it, and how to read
// its usage. The runner knows none of that — it holds capacity, workdirs,
// accounts and the hub, and hands an adapter a Spec. Adapters never import the
// runner.
package adapter

import (
	"context"
	"errors"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// Spec is everything an adapter needs to execute one run. The runner resolves
// it: the workdir exists, the account's harness home is chosen, grants are
// already in Env or on disk.
type Spec struct {
	RunID   string
	Model   string
	Workdir string
	// SessionID is YAD's; NativeSessionID is the harness's own, empty for a new
	// session. Claude lets YAD choose it up front; Codex assigns its own.
	SessionID       string
	NativeSessionID string
	Brief           v1.Brief
	// Home is the account's harness home (CLAUDE_CONFIG_DIR, CODEX_HOME), for
	// an adapter that needs the path itself. What actually points the child at
	// it is the variable in Env: an adapter that ignores this field still runs
	// in the right home. Empty when the owner configured no accounts for the
	// harness, whose runs use the harness's own default home.
	Home string
	// Env is added to the child's environment: grants delivered as env, and the
	// home variable.
	Env []string
	// Binary is the resolved harness path from detection.
	Binary string
	// Settings is the owner's harness configuration: permission mode, sandbox,
	// approval policy. Never from the hub (decision 0015).
	Settings map[string]string
}

// Adapter drives one first-class harness.
type Adapter interface {
	Harness() string
	Start(ctx context.Context, spec Spec) (Turn, error)
}

// Turn is one run in flight.
type Turn interface {
	// Events yields normalised events in order and is closed when the turn ends.
	// Seq is assigned by the runner, not the adapter.
	Events() <-chan v1.Event
	// Steer adds input to the running turn.
	Steer(text string) error
	// Interrupt ends the turn and keeps the session resumable.
	Interrupt() error
	// Terminate sends SIGTERM to the turn's process group: the cancel
	// ladder's second rung, for a harness that did not end its turn when
	// interrupted. The last rung, SIGKILL, is cancelling Start's context. A
	// turn whose process is gone is not an error.
	Terminate() error
	// NativeSessionID is the harness's own session id, or "" until the harness
	// has one. The runner stores it as soon as it appears rather than from the
	// Outcome, so a crash mid-run does not lose the resume pointer.
	NativeSessionID() string
	// Wait blocks until the turn is over and the process is gone.
	Wait() Outcome
}

// Outcome is how a turn ended, as the harness itself reported it. Exit status
// alone never decides it: `prompt_too_long` arrives as a successful exit.
type Outcome struct {
	State           v1.RunState
	FinalText       string
	Error           *v1.RunError
	Usage           map[string]v1.Usage
	NativeSessionID string
	// Limit is set when the turn stopped on a usage limit. The runner, not the
	// adapter, decides whether to fail over or wait.
	Limit *Limit
	// Windows is every usage window the harness mentioned during the turn,
	// with the latest use and reset it gave for each. Set whether or not the
	// turn hit a limit — both harnesses report their windows as they go, and
	// a runner that only looked at them on a failure would know an account's
	// headroom only once it had run out.
	//
	// A window the turn never heard about is absent rather than zero: zero
	// use is what a fresh window reads, and the two must not be confused.
	Windows []Window
	// APIRetries counts transient rate-limit retries the harness did itself.
	// A rate limit is not a usage limit (DOMAIN.md): it never sets Limit and
	// never costs the account its state.
	APIRetries int
}

// Limit is a usage limit hit by the account a turn ran on.
type Limit struct {
	Window  string // "five_hour", "seven_day", "primary", "secondary" — the harness's name
	ResetAt time.Time
}

// Window is one usage window of the account a turn ran on. The name is the
// harness's own, as DOMAIN.md's definition of a usage limit already is.
type Window struct {
	Name string
	// UsedPercent is 0-100, whatever scale the harness reported: Codex gives
	// a percentage and Claude a 0-1 fraction, and the adapters convert.
	UsedPercent float64
	// ResetAt is zero when the harness gave the window's use without a reset.
	ResetAt time.Time
}

// Error classes an adapter puts in RunError.Class. A hub acts on the class and
// shows the message; a new class is an addition, never a rename.
const (
	// ClassPromptTooLong — the conversation no longer fits the model's context.
	// Retrying the same run cannot succeed.
	ClassPromptTooLong = "prompt_too_long"
	// ClassUsageLimit — the account hit a usage limit; Outcome.Limit says until when.
	ClassUsageLimit = "usage_limit"
	// ClassSessionNotFound — a resume named a session the harness does not have.
	ClassSessionNotFound = "session_not_found"
	// ClassSessionMismatch — the harness ran under a different session id than
	// the one it was given: the resume silently failed and the context is gone.
	ClassSessionMismatch = "session_mismatch"
	// ClassHarness — the harness reported the turn failed, for any other reason.
	ClassHarness = "harness_error"
	// ClassHarnessExited — the process ended without reporting a result.
	ClassHarnessExited = "harness_exited"
	// ClassStream — a line of the harness's output could not be read; the turn
	// carried on without it.
	ClassStream = "stream"
)

// ErrNotFirstClass is returned when a run targets a harness with no adapter.
var ErrNotFirstClass = errors.New("no adapter for this harness — it is recognised, not first-class")
