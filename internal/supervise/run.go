//go:build unix

package supervise

import (
	"context"
	"io"
	"time"
)

// Capture is what a short-lived child printed and how it ended.
type Capture struct {
	// Stdout is what the child printed, truncated at the caller's limit.
	Stdout []byte
	// Stderr is the tail of the child's standard error, empty when the spec
	// merged it into Stdout.
	Stderr string
	// TimedOut reports that ctx ended before the leader exited. The group was
	// killed; nothing is left behind.
	TimedOut bool
	// Err is the leader's exit error, nil when it exited 0. Exit status is not
	// success: the caller decides that from Stdout.
	Err error
}

// runDrain is how long the reader gets, once the leader has exited or timed
// out, to collect what is already in the pipe before it is closed. What the
// leader printed is buffered by then, so this is short; it only has to outlast
// the reader goroutine's next wakeup, not any process.
const runDrain = 250 * time.Millisecond

// Run starts spec, collects up to limit bytes of its stdout and returns once
// the leader has exited or ctx has ended. The error it returns is a failure to
// start; everything the child did is in the Capture.
//
// The read cannot simply run to EOF. When the leader exits the group dies, but
// a descendant that left it with setsid — a node launcher spawning a detached
// updater with inherited stdio — keeps the pipe open, and EOF never comes. So
// the leader's own fate decides: once it has exited, what it printed is already
// in the pipe and a short drain collects it; if the deadline comes first, the
// child timed out. Closing our end is what ends a read the detached process
// would otherwise hold forever.
func Run(ctx context.Context, spec Spec, limit int) (Capture, error) {
	p, err := Start(ctx, spec)
	if err != nil {
		return Capture{}, err
	}
	read := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(p.Stdout(), int64(limit)))
		read <- b
	}()
	var c Capture
	gotEOF := false
	select {
	case c.Stdout = <-read:
		gotEOF = true
	case <-p.Done():
	case <-ctx.Done():
		select {
		case <-p.Done():
		default:
			c.TimedOut = true
		}
	}
	if !gotEOF {
		drain := time.NewTimer(runDrain)
		select {
		case c.Stdout = <-read:
			gotEOF = true
		case <-drain.C:
		}
		drain.Stop()
	}
	p.Stdout().Close()
	if !gotEOF {
		c.Stdout = <-read // ReadAll returns what it read before the close
	}
	c.Err = p.Wait()
	c.Stderr = p.Stderr()
	return c, nil
}
