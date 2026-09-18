package runner

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/skkap/yad/internal/supervise"
)

// Drain is the runner's way down (DOMAIN.md, decision 0029), in two steps a
// process can only climb. Draining: stop claiming, keep syncing so leases
// renew and results land, and exit once every run held has ended. Cancelling:
// every run still held is cancelled down the cancel ladder, and the runner
// exits once they have ended. The step past that — exit now, whatever is
// running — is not a state here but the end of Serve's context; what it cuts
// short is reported lost at the next start.
//
// A signal, the hub's drain control and — later — the control socket's
// `yad daemon stop` all reach the runner through one Drain, so they climb the
// same ladder and never each other's.
type Drain struct {
	mu         sync.Mutex
	reason     string
	draining   chan struct{}
	cancelling chan struct{}
}

// NewDrain returns a runner that is serving.
func NewDrain() *Drain {
	return &Drain{draining: make(chan struct{}), cancelling: make(chan struct{})}
}

// Begin starts draining, and reports whether this call did. Once draining a
// runner never serves again: the process exits at the end of it.
func (d *Drain) Begin(reason string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.climb(d.draining, reason)
}

// Cancel moves to the second step — draining first if not already — and
// reports whether this call did.
func (d *Drain) Cancel(reason string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.climb(d.draining, reason)
	return d.climb(d.cancelling, reason)
}

func (d *Drain) climb(step chan struct{}, reason string) bool {
	select {
	case <-step:
		return false
	default:
	}
	d.reason = reason
	close(step)
	return true
}

// Draining is closed once the runner stops claiming.
func (d *Drain) Draining() <-chan struct{} { return d.draining }

// Cancelling is closed once the runner cancels what it holds.
func (d *Drain) Cancelling() <-chan struct{} { return d.cancelling }

// IsDraining reports whether the runner has stopped claiming. A nil Drain
// never drains.
func (d *Drain) IsDraining() bool {
	if d == nil {
		return false
	}
	select {
	case <-d.draining:
		return true
	default:
		return false
	}
}

// Reason is why the last step was taken.
func (d *Drain) Reason() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.reason
}

// OnSignals climbs the ladder by counting stop signals: the first drains, the
// second cancels, the third calls exit — which should end Serve's context. A
// service manager sends one SIGTERM and waits; a person at a terminal presses
// Ctrl-C again when they mean it. Counting rather than stepping from the
// current state keeps the meaning of a signal fixed: a runner the hub is
// already draining still drains on the first SIGTERM, and does not cancel
// someone's run because a service manager asked politely.
//
// It returns when ctx ends or after the third signal.
func OnSignals(ctx context.Context, sigs <-chan os.Signal, d *Drain, exit func(), log *slog.Logger) {
	for n := 1; ; n++ {
		var sig os.Signal
		select {
		case <-ctx.Done():
			return
		case sig = <-sigs:
		}
		switch n {
		case 1:
			log.Warn("draining: no new runs; exiting once the runs held have ended — signal again to cancel them", "signal", sig.String())
			d.Begin("the runner received " + sig.String())
		case 2:
			log.Warn("cancelling every run held, then exiting — signal again to exit now", "signal", sig.String())
			d.Cancel("the runner received a second " + sig.String())
		default:
			log.Warn("exiting now; runs cut short are reported lost at the next start", "signal", sig.String())
			exit()
			return
		}
	}
}

// StopBudget is the longest a first stop signal can take to end the runner:
// the drain wait, then the cancel ladder on whatever is still running, then
// the last delivery, and slack for the syncs in between. A service manager's
// stop timeout must be at least this, or it kills the runner mid-drain and the
// runs it holds end lost at the next start.
func StopBudget(drainWait time.Duration) time.Duration {
	return max(drainWait, 0) + supervise.DefaultLadder.InterruptGrace + supervise.DefaultLadder.TermGrace + flushWait + stopSlack
}

// Timings of the way down.
const (
	// stopSlack covers what StopBudget cannot time: a sync or an upload in
	// flight when a step begins, bounded by the hub client's own timeouts.
	stopSlack = 15 * time.Second
	// flushWait bounds the last delivery once every run has ended: long enough
	// for a reachable hub to take what is owed, short enough that a hub which
	// is down does not hold an exit that loses nothing — the spool and the
	// outbox are replayed at the next start.
	flushWait = 30 * time.Second
	// settledPoll is how often the last delivery checks what is still owed.
	// The reporters say nothing when they finish, and the store is the truth.
	settledPoll = 100 * time.Millisecond
)
