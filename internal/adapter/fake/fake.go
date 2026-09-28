// Package fake is a scripted harness for tests: it plays a list of events and
// ends with a chosen outcome, with no process and no tokens. The runner, the
// executor and the conformance suite run against it.
package fake

import (
	"context"
	"errors"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

// Script is what the fake harness does in one turn.
type Script struct {
	Events  []v1.Event
	Outcome adapter.Outcome
	// Delay is paused before each event, so tests can steer or interrupt mid-turn.
	Delay time.Duration
	// Hang never ends the turn on its own — for watchdog and cancel tests.
	Hang bool
	// IgnoreInterrupt and IgnoreTerm make the turn deaf to the cancel
	// ladder's first and second rungs, as a wedged harness is; only the
	// context — SIGKILL — ends it then.
	IgnoreInterrupt bool
	IgnoreTerm      bool
	// AwaitInterrupt holds the turn after its events until an interrupt
	// reaches it, then ends it with Outcome. With IgnoreInterrupt it is a
	// harness that hears the interrupt and carries on, with no clock to race
	// the interrupt's delivery (DEV-147).
	AwaitInterrupt bool
	// Stopped is the outcome an interrupt or SIGTERM ends the turn with; nil
	// is cancelled. A harness whose answer was already on its way reports
	// that answer instead.
	Stopped *adapter.Outcome
	// SteerError, when set, is what Steer answers — a harness that no longer
	// takes input.
	SteerError string
	// InterruptFails makes Interrupt answer an error without reaching the
	// harness, as many times as it says.
	InterruptFails int
}

// Adapter plays Scripts. Next picks the script for each Start, so a test can
// vary behaviour per run.
type Adapter struct {
	ID   string
	Next func(adapter.Spec) Script
	// NoEffort is an adapter that cannot hand a run's effort to its harness,
	// as a harness added later might be; the fake applies one by default, as
	// both real adapters do.
	NoEffort bool
	// NoFork is an adapter that cannot open a session as a fork; the fake
	// forks by default, as both real adapters do.
	NoFork bool

	mu     sync.Mutex
	Starts []adapter.Spec
	turns  []adapter.Turn
}

// Turns returns the turns started so far, for assertions on what reached them.
func (a *Adapter) Turns() []adapter.Turn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]adapter.Turn(nil), a.turns...)
}

func (a *Adapter) Harness() string {
	if a.ID == "" {
		return "fake"
	}
	return a.ID
}

func (a *Adapter) AppliesEffort() bool { return !a.NoEffort }

func (a *Adapter) Forks() bool { return !a.NoFork }

func (a *Adapter) Start(ctx context.Context, spec adapter.Spec) (adapter.Turn, error) {
	if a.Next == nil {
		return nil, errors.New("fake adapter has no script")
	}
	a.mu.Lock()
	a.Starts = append(a.Starts, spec)
	a.mu.Unlock()

	s := a.Next(spec)
	t := &turn{events: make(chan v1.Event), done: make(chan struct{}), stop: make(chan struct{}), heard: make(chan struct{}), script: s}
	t.native = s.Outcome.NativeSessionID
	a.mu.Lock()
	a.turns = append(a.turns, t)
	a.mu.Unlock()
	go t.play(ctx)
	return t, nil
}

type turn struct {
	events chan v1.Event
	done   chan struct{}
	stop   chan struct{} // closed by the rung that ends the turn
	heard  chan struct{} // closed by the first interrupt that reaches the turn
	script Script

	mu          sync.Mutex
	steered     []string
	interrupts  int
	terminated  bool
	outcome     adapter.Outcome
	stopOutcome adapter.Outcome
	native      string
}

func (t *turn) play(ctx context.Context) {
	defer close(t.done)
	defer close(t.events)
	s := t.script
	t.outcome = s.Outcome
	end := func() bool {
		select {
		case <-t.stop:
			t.mu.Lock()
			t.outcome = t.stopOutcome
			t.mu.Unlock()
			return true
		case <-ctx.Done():
			t.outcome = adapter.Outcome{State: v1.RunCancelled}
			return true
		default:
			return false
		}
	}
	for _, e := range s.Events {
		select {
		case <-time.After(s.Delay):
		case <-t.stop:
		case <-ctx.Done():
		}
		if end() {
			return
		}
		// The consumer may stop reading once it has cancelled; a bare send would
		// then block forever and Wait would never return.
		select {
		case t.events <- e:
		case <-t.stop:
		case <-ctx.Done():
		}
		if end() {
			return
		}
	}
	if s.AwaitInterrupt {
		select {
		case <-t.heard:
		case <-t.stop:
		case <-ctx.Done():
		}
		if end() {
			return
		}
	}
	if s.Hang {
		select {
		case <-t.stop:
		case <-ctx.Done():
		}
		end()
	}
}

// halt ends the turn with the script's stopped outcome, once.
func (t *turn) halt() {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.stop:
		return
	default:
	}
	t.stopOutcome = adapter.Outcome{State: v1.RunCancelled, NativeSessionID: t.script.Outcome.NativeSessionID}
	if t.script.Stopped != nil {
		t.stopOutcome = *t.script.Stopped
	}
	close(t.stop)
}

func (t *turn) Events() <-chan v1.Event { return t.events }

func (t *turn) NativeSessionID() string { return t.native }

func (t *turn) Steer(text string) error {
	if t.script.SteerError != "" {
		return errors.New(t.script.SteerError)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.steered = append(t.steered, text)
	return nil
}

// Steered returns what was steered into the turn, for assertions.
func Steered(tr adapter.Turn) []string {
	t := tr.(*turn)
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.steered...)
}

func (t *turn) Interrupt() error {
	t.mu.Lock()
	t.interrupts++
	failed := t.interrupts <= t.script.InterruptFails
	first := t.interrupts == t.script.InterruptFails+1
	t.mu.Unlock()
	if failed {
		return errors.New("the harness is not reading its input")
	}
	// Halted before it is heard, so a turn awaiting the interrupt that it
	// also obeys ends stopped rather than with its Outcome.
	if !t.script.IgnoreInterrupt {
		t.halt()
	}
	if first {
		close(t.heard)
	}
	return nil
}

func (t *turn) Terminate() error {
	t.mu.Lock()
	t.terminated = true
	t.mu.Unlock()
	if !t.script.IgnoreTerm {
		t.halt()
	}
	return nil
}

// Rungs returns how many interrupts the turn was sent and whether it was
// sent SIGTERM, for assertions on the cancel ladder.
func Rungs(tr adapter.Turn) (interrupts int, terminated bool) {
	t := tr.(*turn)
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.interrupts, t.terminated
}

func (t *turn) Wait() adapter.Outcome {
	<-t.done
	return t.outcome
}
