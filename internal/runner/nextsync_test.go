package runner

import (
	"context"
	"slices"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// Against yad hub: a runner whose capacity is taken by a run it has just
// claimed, with another run queued behind it, is asked back in 3 s and waits
// 3 s — the floor lets the hub's quick answer through rather than clamping it
// to the steady 5 s. With nothing behind it, it waits the hub's 15 s.
func TestARunQueuedBehindTheRunnerBringsItBackIn3s(t *testing.T) {
	for _, tc := range []struct {
		name   string
		queued []v1.Run
		want   time.Duration
	}{
		{"a run queued behind the one it holds", []v1.Run{testRun("a", "s1"), testRun("b", "s2")}, 3 * time.Second},
		{"nothing queued behind it", []v1.Run{testRun("a", "s1")}, 15 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			l := e.loop(t, 1)
			e.enqueue(t, tc.queued...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.clock.stopAfter, e.clock.cancel = 2, cancel
			if err := l.Run(ctx); err != nil {
				t.Fatal(err)
			}
			// The offer is listed back at once; the answer to that claim
			// names the wait.
			if want := []time.Duration{0, tc.want}; !slices.Equal(e.clock.waits, want) {
				t.Errorf("waits %v, want %v", e.clock.waits, want)
			}
			if got := e.exec.ids(); !slices.Equal(got, []string{"a"}) {
				t.Errorf("started %v, want [a]", got)
			}
		})
	}
}
