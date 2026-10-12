package adapter

import (
	"context"

	"github.com/skkap/yad/internal/supervise"
)

// Census is what a turn's tool work still has running as the harness gives
// its answer, for Outcome.LeftRunning: nil when it could not be taken, empty
// when nothing was. An adapter takes it after the harness's own end of the
// turn and before closing its input, since the harness stops its tool
// processes on the way out, and only for a turn nobody stopped: a cancel, an
// interrupt or a watchdog has already decided what became of the work
// (decision 0083). A turn with no process — a test's — has nothing to measure.
//
// With stop, the owner's [supervise] stop_left_running, what it counted is
// stopped there and then, before the harness's input closes — the ladder's
// SIGTERM, its grace, then SIGKILL — and each leftover says whether it went
// (decision 0086).
func Census(ctx context.Context, p *supervise.Process, stop bool, helpers ...string) *[]supervise.Leftover {
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
	if stop {
		left = p.StopLeftovers(ctx, left, supervise.DefaultLadder.TermGrace, helpers...)
	}
	return &left
}
