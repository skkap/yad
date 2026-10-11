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
// (decision 0081). A turn with no process — a test's — has nothing to measure.
func Census(ctx context.Context, p *supervise.Process, helpers ...string) *[]supervise.Leftover {
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
	return &left
}
