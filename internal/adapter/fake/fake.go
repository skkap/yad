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
}

// Adapter plays Scripts. Next picks the script for each Start, so a test can
// vary behaviour per run.
type Adapter struct {
	ID   string
	Next func(adapter.Spec) Script

	mu     sync.Mutex
	Starts []adapter.Spec
}

func (a *Adapter) Harness() string {
	if a.ID == "" {
		return "fake"
	}
	return a.ID
}

func (a *Adapter) Start(ctx context.Context, spec adapter.Spec) (adapter.Turn, error) {
	if a.Next == nil {
		return nil, errors.New("fake adapter has no script")
	}
	a.mu.Lock()
	a.Starts = append(a.Starts, spec)
	a.mu.Unlock()

	t := &turn{events: make(chan v1.Event), done: make(chan struct{}), interrupt: make(chan struct{})}
	s := a.Next(spec)
	go t.play(ctx, s)
	return t, nil
}

type turn struct {
	events    chan v1.Event
	done      chan struct{}
	interrupt chan struct{}
	once      sync.Once
	mu        sync.Mutex
	steered   []string
	outcome   adapter.Outcome
}

func (t *turn) play(ctx context.Context, s Script) {
	defer close(t.done)
	defer close(t.events)
	t.outcome = s.Outcome
	for _, e := range s.Events {
		select {
		case <-time.After(s.Delay):
		case <-t.interrupt:
			t.outcome = adapter.Outcome{State: v1.RunCancelled, NativeSessionID: s.Outcome.NativeSessionID}
			return
		case <-ctx.Done():
			t.outcome = adapter.Outcome{State: v1.RunCancelled}
			return
		}
		t.events <- e
	}
	if s.Hang {
		select {
		case <-t.interrupt:
			t.outcome = adapter.Outcome{State: v1.RunCancelled}
		case <-ctx.Done():
			t.outcome = adapter.Outcome{State: v1.RunCancelled}
		}
	}
}

func (t *turn) Events() <-chan v1.Event { return t.events }

func (t *turn) Steer(text string) error {
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
	t.once.Do(func() { close(t.interrupt) })
	return nil
}

func (t *turn) Wait() adapter.Outcome {
	<-t.done
	return t.outcome
}
