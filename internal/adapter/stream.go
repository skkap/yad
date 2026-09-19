package adapter

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	v1 "github.com/skkap/yad/protocol/v1"
)

// Queue holds a turn's events between the adapter's reader and the runner.
//
// It is unbounded on purpose. The reader must never block on a consumer that
// has stopped reading, or the harness blocks on a full pipe and the turn never
// ends. The queue is bounded by the stream itself, which ends.
type Queue struct {
	mu     sync.Mutex
	queue  []v1.Event
	closed bool
	wake   chan struct{}
	out    chan v1.Event
}

// NewQueue returns an empty queue; Pump must run for Out to yield anything.
func NewQueue() *Queue {
	return &Queue{wake: make(chan struct{}, 1), out: make(chan v1.Event)}
}

// Out is Turn.Events.
func (q *Queue) Out() <-chan v1.Event { return q.out }

// Push queues an event. It never blocks.
func (q *Queue) Push(e v1.Event) {
	q.mu.Lock()
	q.queue = append(q.queue, e)
	q.mu.Unlock()
	q.notify()
}

// Close ends the stream once what is queued has been handed over.
func (q *Queue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.notify()
}

func (q *Queue) notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Pump hands queued events to the consumer in order and closes Out after the
// last. It gives up when ctx ends, so a consumer that cancelled the run and
// stopped reading does not leave it blocked for ever.
func (q *Queue) Pump(ctx context.Context) {
	defer close(q.out)
	for {
		q.mu.Lock()
		batch := q.queue
		q.queue = nil
		closed := q.closed
		q.mu.Unlock()
		for _, e := range batch {
			select {
			case q.out <- e:
			case <-ctx.Done():
				return
			}
		}
		if len(batch) > 0 {
			continue
		}
		if closed {
			return
		}
		select {
		case <-q.wake:
		case <-ctx.Done():
			return
		}
	}
}

// TextFlush bounds how long streamed text waits before it becomes an event.
// Harnesses send a delta every few tokens; one event each would be thousands
// per answer, and one per finished message would leave a long answer
// invisible — and the inactivity watchdog blind — until it ends.
const TextFlush = time.Second

// TextFlushBytes flushes a fast stream sooner, keeping each event small.
const TextFlushBytes = 4 << 10

// Text gathers streamed deltas into text and thinking events.
type Text struct {
	Emit func(v1.Event)
	Now  func() time.Time

	pending strings.Builder
	kind    v1.EventKind
	since   time.Time
}

// Add appends a delta, flushing first if it is of the other kind.
func (t *Text) Add(kind v1.EventKind, s string) {
	if s == "" {
		return
	}
	if t.pending.Len() > 0 && t.kind != kind {
		t.Flush()
	}
	if t.pending.Len() == 0 {
		t.kind, t.since = kind, t.Now()
	}
	t.pending.WriteString(s)
	if t.pending.Len() >= TextFlushBytes {
		t.Flush()
	}
}

// Tick flushes text that has waited TextFlush. It runs on a timer, not on the
// next delta: a harness that stalls mid-sentence must not hold back what it
// has already said.
func (t *Text) Tick() {
	if t.pending.Len() > 0 && t.Now().Sub(t.since) >= TextFlush {
		t.Flush()
	}
}

// Flush emits whatever is pending.
func (t *Text) Flush() {
	if t.pending.Len() == 0 {
		return
	}
	t.Emit(v1.Event{At: t.Now(), Kind: t.kind, Text: t.pending.String()})
	t.pending.Reset()
}

// CapTool holds a tool payload to MaxToolOutputBytes without splitting a rune.
func CapTool(s string) (string, bool) {
	if len(s) <= v1.MaxToolOutputBytes {
		return s, false
	}
	cut := v1.MaxToolOutputBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
