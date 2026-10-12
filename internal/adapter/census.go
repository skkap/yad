package adapter

import (
	"context"

	"github.com/skkap/yad/internal/supervise"
)

// Census is what a turn's tool work still has running as the harness gives
// its answer, for Outcome.LeftRunning: Left.Wait gives nil when it could not
// be taken, empty when nothing was. An adapter takes it after the harness's
// own end of the turn and before closing its input, since the harness stops
// its tool processes on the way out, and only for a turn nobody stopped: a
// cancel, an interrupt or a watchdog has already decided what became of the
// work (decision 0083). A turn with no process — a test's — has nothing to
// measure.
//
// With stop, the owner's [supervise] stop_left_running, what it counted is
// stopped too (decision 0086): SIGTERM before Census returns, so it reaches
// the work before the harness's input closes, and the rest of the ladder
// beside the harness's exit.
func Census(ctx context.Context, p *supervise.Process, stop bool, helpers ...string) *Left {
	if p == nil || ctx.Err() != nil {
		return nil
	}
	left, err := p.Census(ctx, helpers...)
	if err != nil {
		return nil
	}
	if left == nil {
		left = []supervise.Leftover{}
	}
	l := &Left{left: left}
	if stop {
		l.stopping = p.StopLeftovers(ctx, left, supervise.DefaultLadder.TermGrace, helpers...)
	}
	return l
}

// Left is a census taken, and the stop over it that may still be running.
type Left struct {
	left     []supervise.Leftover
	stopping *supervise.Stopping
}

// Wait is the census for Outcome.LeftRunning, once any stop over it is over;
// nil for a census not taken. An adapter waits for it after its events have
// ended, so the run's watchdogs, which read the events, are not running
// while a stop finishes: a turn that answered in time is not timed out for
// the seconds yad spent stopping what it left.
func (l *Left) Wait() *[]supervise.Leftover {
	if l == nil {
		return nil
	}
	if l.stopping != nil {
		l.left = l.stopping.Wait()
	}
	return &l.left
}
