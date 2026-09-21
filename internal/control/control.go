//go:build unix

package control

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword"
)

// A stop is the one request that takes two lines, so that the CLI can tell a
// stop the daemon never acted on from one it did. The CLI sends OpStop; the
// daemon answers with Confirm set and does nothing yet; the CLI sends
// OpConfirm, and only that stops the daemon, which answers once more first.
// Until the CLI has heard the first answer nothing has been acted on, so a
// SIGTERM is a safe fallback: it is the runner's first stop. Once the CLI has
// written the confirm the stop is delivered, and only --force may signal
// (decision 0027) — a SIGTERM then would be the runner's second stop, which
// cancels every run it holds (decision 0029).
const (
	OpStop    = "stop"
	OpConfirm = "confirm"
)

// Request is what the CLI asks.
type Request struct {
	Op string `json:"op"` // "status", OpStop, OpConfirm or "close_session"
	// Connection and Session name the session to close.
	Connection string `json:"connection,omitempty"`
	Session    string `json:"session,omitempty"`
}

// Response is the daemon's answer. Error carries the next action, as every
// error here does.
type Response struct {
	Error  string        `json:"error,omitempty"`
	PID    int           `json:"pid"`
	Status *Status       `json:"status,omitempty"`
	Closed *SessionClose `json:"closed,omitempty"`
	// Confirm is the daemon holding a stop until the CLI confirms it. A stop
	// answered without it came from a daemon older than the exchange, which
	// acted on the request alone.
	Confirm bool `json:"confirm,omitempty"`
}

// SessionClose is what `yad sessions close` did.
type SessionClose struct {
	// Outcome is closed, closing (once LiveRun ends), already_closed or
	// unknown.
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	LiveRun string `json:"live_run,omitempty"`
}

// Status is everything `yad status` shows.
type Status struct {
	PID      int       `json:"pid"`
	Profile  string    `json:"profile"`
	RunnerID string    `json:"runner_id"`
	Name     string    `json:"name"`
	Version  string    `json:"version"`
	Started  time.Time `json:"started"`
	// Stopping is set once a stop was asked for and the process has not yet
	// exited: what a graceful stop is waiting on is in Runs.
	Stopping bool `json:"stopping"`
	// Ready is set once the runner is past its setup — store open, a
	// connection syncing or none configured. A background start reports
	// success only then, so a daemon that is about to exit on a missing
	// credential is not reported as started.
	Ready bool `json:"ready"`

	Capacity    Capacity     `json:"capacity"`
	Connections []Connection `json:"connections"`
	Runs        []Run        `json:"runs"`
	Sessions    int          `json:"sessions"`     // open sessions, across connections
	SpoolDepth  int          `json:"spool_depth"`  // events not yet acknowledged by their hub
	OutboxDepth int          `json:"outbox_depth"` // results not yet acknowledged
	Errors      []LogRecord  `json:"errors"`       // the latest warnings and errors, oldest first
}

// Capacity is the pool: Free is what no run and no sync in flight holds.
type Capacity struct {
	Total int `json:"total"`
	Free  int `json:"free"`
}

// Connection is one hub as the daemon sees it now.
type Connection struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Held is the runs this connection has of the capacity pool, and Cap the
	// owner's bound on it: 0 is no cap of its own.
	Held        int        `json:"held"`
	Cap         int        `json:"cap,omitempty"`
	State       string     `json:"state"` // starting | syncing | retrying | stopped
	LastSync    *time.Time `json:"last_sync,omitempty"`
	LastError   string     `json:"last_error,omitempty"`
	LastErrorAt *time.Time `json:"last_error_at,omitempty"`
}

// Run is one run the runner holds.
type Run struct {
	Connection string     `json:"connection"`
	ID         string     `json:"id"`
	Session    string     `json:"session"`
	Harness    string     `json:"harness"`
	Model      string     `json:"model"`
	State      string     `json:"state"`
	Reason     string     `json:"reason,omitempty"`
	ResumesAt  *time.Time `json:"resumes_at,omitempty"`
	Since      time.Time  `json:"since"`
}

// LogRecord is one warning or error from the daemon's log.
type LogRecord struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Attrs   string    `json:"attrs,omitempty"`
}

// maxSocketPath is the longest path bind(2) takes: sun_path is 104 bytes on
// macOS and the BSDs and 108 on Linux, each with a terminating NUL. Past it
// the kernel refuses with a bare EINVAL that names nothing.
func maxSocketPath() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// CheckPath refuses a socket path the kernel cannot bind. It never falls back
// to a shorter directory elsewhere: a socket outside the data directory is one
// the CLI of the same profile would not find, and one whose directory nobody
// checked.
func CheckPath(sock string) error {
	if n := len(sock); n > maxSocketPath() {
		return fmt.Errorf("the control socket %s is %d bytes, and %s allows at most %d — set YAD_DATA_DIR to a shorter directory, or use a shorter --profile", sock, n, runtime.GOOS, maxSocketPath())
	}
	return nil
}

// checkDir refuses a data directory another user could reach into. The
// socket's own 0600 is set after bind, so the directory is what keeps it
// private in between, and what stops anyone replacing it.
func checkDir(dir string) error {
	fi, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory — point YAD_DATA_DIR at a directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("the data directory %s is %v, and the control socket needs it private — `%s`", dir, fi.Mode().Perm(), shellword.Command("chmod", "700", dir))
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("the data directory %s belongs to uid %d, not to you (%d) — run yad as its owner, or set YAD_DATA_DIR to a directory of your own", dir, st.Uid, os.Getuid())
	}
	return nil
}

// ErrNotRunning is a profile with no daemon.
var ErrNotRunning = errors.New("no daemon is running for this profile")

// RunningError is a profile whose daemon is already up.
type RunningError struct {
	PID    int
	Socket string
	// Paths is the profile the daemon runs for, which the commands the error
	// offers act on.
	Paths config.Paths
}

func (e *RunningError) Error() string {
	return fmt.Sprintf("a daemon is already running for this profile (pid %d, socket %s) — `%s` first, or `%s`", e.PID, e.Socket, e.Paths.Command("daemon", "stop"), e.Paths.Command("daemon", "restart"))
}
