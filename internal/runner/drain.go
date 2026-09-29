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
	mu     sync.Mutex
	reason string
	// asked counts the owner's stop requests: signals, and the control
	// socket's stop, which is the first of them.
	asked      int
	draining   chan struct{}
	cancelling chan struct{}
	// update is a drain begun by a self-update and by nobody since (decision
	// 0071): it has no drain wait, so no run it waits on is ever cancelled,
	// and the process re-executes at its end rather than exiting. bounded
	// closes when anyone else asks for a drain or a stop, which makes it an
	// ordinary one from then on.
	update  bool
	bounded chan struct{}
}

// NewDrain returns a runner that is serving.
func NewDrain() *Drain {
	return &Drain{draining: make(chan struct{}), cancelling: make(chan struct{}), bounded: make(chan struct{})}
}

// Begin starts draining, and reports whether this call did. Once draining a
// runner never serves again: the process exits at the end of it. A drain a
// self-update began becomes this one — this call started the exit.
func (d *Drain) Begin(reason string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ordinary(reason) || d.climb(d.draining, reason)
}

// Update starts the drain a self-update ends in, and reports whether this
// call did. It is the one drain with no drain wait: it stops claiming and
// waits for every run held to end however long that takes, because a run is
// never interrupted for an update. A runner already draining for any other
// reason is on its way out, and exits as it was going to.
func (d *Drain) Update(reason string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.climb(d.draining, reason) {
		return false
	}
	d.update = true
	return true
}

// ForUpdate reports whether the drain under way is a self-update's and nobody
// has asked for a drain or a stop since: its end is a re-exec, not an exit.
// A nil Drain never drains.
func (d *Drain) ForUpdate() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.update
}

// Bounded is closed once the drain under way is an ordinary one, with the
// owner's drain wait: a self-update's drain until someone else asks for one
// too. A drain that was ordinary from its start never waits on it.
func (d *Drain) Bounded() <-chan struct{} { return d.bounded }

// ordinary turns a self-update's drain into an ordinary one, and reports
// whether it did. The owner's stop and a hub's drain both mean exit: the
// runner must not come back as the new binary and claim again, and the drain
// wait they expect applies from now.
func (d *Drain) ordinary(reason string) bool {
	if !d.update {
		return false
	}
	d.update = false
	d.reason = reason
	close(d.bounded)
	return true
}

// Cancel moves to the second step — draining first if not already — and
// reports whether this call did.
func (d *Drain) Cancel(reason string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ordinary(reason)
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

// Step is one stop request from the runner's owner, counted: the first drains,
// the second cancels, and from the third it returns 3 or more, which is the
// caller's to act on — exit now. A drain the hub started is not one of them.
func (d *Drain) Step(reason string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.step(reason)
}

func (d *Drain) step(reason string) int {
	d.ordinary(reason)
	d.asked++
	switch d.asked {
	case 1:
		d.climb(d.draining, reason)
	case 2:
		d.climb(d.draining, reason)
		d.climb(d.cancelling, reason)
	}
	return d.asked
}

// Stop is `yad daemon stop`: the owner's first step, taken once however
// often it is asked, and reports whether this call took it. A signal after it
// is the second step.
func (d *Drain) Stop(reason string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.asked == 0 && d.step(reason) == 1
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

// OnSignals climbs the ladder by counting the owner's stop requests (Step):
// the first drains, the second cancels, the third calls exit — which should
// end Serve's context. A service manager sends one SIGTERM and waits; a person
// at a terminal presses Ctrl-C again when they mean it. `yad daemon stop`
// counts as the first. Counting rather than stepping from the current state
// keeps the meaning of a signal fixed: a runner the hub is already draining
// still drains on the first SIGTERM, and does not cancel someone's run because
// a service manager asked politely.
//
// It returns when ctx ends or after the third request.
func OnSignals(ctx context.Context, sigs <-chan os.Signal, d *Drain, exit func(), log *slog.Logger) {
	for {
		var sig os.Signal
		select {
		case <-ctx.Done():
			return
		case sig = <-sigs:
		}
		switch n := d.Step("the runner received " + sig.String()); n {
		case 1:
			log.Warn("draining: no new runs; exiting once the runs held have ended — signal again to cancel them", "signal", sig.String())
		case 2:
			log.Warn("cancelling every run held, then exiting — signal again to exit now", "signal", sig.String())
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
	// loginPoll is how often a self-update's drain looks again at a hub
	// login it is waiting for; a login says nothing when it ends.
	loginPoll = time.Second
	// settledPoll is how often the last delivery checks what is still owed.
	// The reporters say nothing when they finish, and the store is the truth.
	settledPoll = 100 * time.Millisecond
)
