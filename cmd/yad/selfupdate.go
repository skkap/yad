package main

import (
	"fmt"
	"log/slog"
	"runtime"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/runner"
	"github.com/skkap/yad/internal/selfupdate"
)

// selfUpdateTimings are selfupdate's own constants in the shipped binary. The
// end-to-end tests shorten them for a runner that is a child process, which no
// fake clock reaches.
var selfUpdateTimings struct{ first, every, idleWait, poll time.Duration }

// newUpdater is the self-update config.toml turned on (decision 0071), for the
// binary at exe. It reads releases where `yad upgrade` does, and hands the
// takeover to the drain.
func newUpdater(g global, cfg config.Config, exe string, drain *runner.Drain, monitor *runner.Monitor, log *slog.Logger) *selfupdate.Updater {
	src := releaseSource()
	src.Yad = g.paths.Command
	return selfupdate.New(selfupdate.Options{
		Source: src, Target: exe, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		Installed: buildinfo.Version, Needs: connectionProtocols(cfg),
		Ask: selfupdate.Ask, Idle: monitor.Idle, Swap: drain.Update,
		Repo: src.Repo, Yad: src.Yad,
		First: selfUpdateTimings.first, Every: selfUpdateTimings.every,
		IdleWait: selfUpdateTimings.idleWait, Poll: selfUpdateTimings.poll,
		Log: log,
	})
}

// connectionProtocols is each protocol major the runner's connections sync
// over, with the connections that do. This build speaks v1 alone, so every
// connection it holds registered over v1 and syncs over it; a build that
// speaks two majors has to record, per connection, which one its hub took,
// and this is where that record would be read.
func connectionProtocols(cfg config.Config) map[string][]string {
	if len(cfg.Connections) == 0 {
		return nil
	}
	names := make([]string, len(cfg.Connections))
	for i, c := range cfg.Connections {
		names[i] = c.Name
	}
	return map[string][]string{v1.Version: names}
}

// updateStatus is `yad status`'s view of the self-update; nil is off.
func updateStatus(u *selfupdate.Updater) *control.Update {
	if u == nil {
		return nil
	}
	s := u.Status()
	out := &control.Update{Off: s.Off, LastError: s.LastError, Latest: s.Latest}
	if !s.LastCheck.IsZero() {
		out.LastCheck = &s.LastCheck
	}
	if !s.NextCheck.IsZero() {
		out.NextCheck = &s.NextCheck
	}
	if p := s.Pending; p != nil {
		out.Pending = &control.PendingUpdate{Tag: p.Tag, Since: p.Since, By: p.By, Swapping: p.Swapping, Reason: p.Reason}
	}
	if r := s.Refused; r != nil {
		out.Refused = &control.RefusedUpdate{Tag: r.Tag, Reason: r.Reason, At: r.At}
	}
	return out
}

// reexecError is a runner that drained for a self-update and has closed
// everything it held — its socket and lock, state.db, its log — so the process
// can become the binary at path.
type reexecError struct{ path string }

func (e reexecError) Error() string {
	return fmt.Sprintf("the runner drained for a self-update and re-executes %s", e.path)
}
