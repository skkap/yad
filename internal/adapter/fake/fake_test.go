package fake

import (
	"context"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

var _ adapter.Adapter = (*Adapter)(nil)

func TestPlaysScript(t *testing.T) {
	a := &Adapter{Next: func(adapter.Spec) Script {
		return Script{
			Events:  []v1.Event{{Kind: v1.EventText, Text: "hello"}, {Kind: v1.EventText, Text: "done"}},
			Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "done", NativeSessionID: "n1"},
		}
	}}
	tr, err := a.Start(context.Background(), adapter.Spec{RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for e := range tr.Events() {
		got = append(got, e.Text)
	}
	out := tr.Wait()
	if len(got) != 2 || out.State != v1.RunSucceeded || out.NativeSessionID != "n1" {
		t.Errorf("events %v outcome %+v", got, out)
	}
	if len(a.Starts) != 1 || a.Starts[0].RunID != "r1" {
		t.Errorf("starts = %+v", a.Starts)
	}
}

func TestInterruptEndsHangingTurn(t *testing.T) {
	a := &Adapter{Next: func(adapter.Spec) Script { return Script{Hang: true} }}
	tr, _ := a.Start(context.Background(), adapter.Spec{})
	tr.Steer("more")
	if s := Steered(tr); len(s) != 1 || s[0] != "more" {
		t.Errorf("steered = %v", s)
	}
	tr.Interrupt()
	tr.Interrupt() // twice is harmless: cancel paths race
	done := make(chan adapter.Outcome)
	go func() { done <- tr.Wait() }()
	select {
	case out := <-done:
		if out.State != v1.RunCancelled {
			t.Errorf("state = %s", out.State)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interrupt did not end the turn")
	}
}

// A runner that cancels stops reading events. The turn must still end, or its
// Wait hangs and the executor leaks a goroutine per cancelled run.
func TestInterruptWhileNobodyReads(t *testing.T) {
	a := &Adapter{Next: func(adapter.Spec) Script {
		return Script{Events: []v1.Event{{Kind: v1.EventText}, {Kind: v1.EventText}, {Kind: v1.EventText}}}
	}}
	tr, _ := a.Start(context.Background(), adapter.Spec{})
	time.Sleep(20 * time.Millisecond) // let play reach its first send
	tr.Interrupt()
	done := make(chan adapter.Outcome)
	go func() { done <- tr.Wait() }()
	select {
	case out := <-done:
		if out.State != v1.RunCancelled {
			t.Errorf("state = %s", out.State)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait hung on an event nobody was reading")
	}
}

func TestContextCancelWhileNobodyReads(t *testing.T) {
	a := &Adapter{Next: func(adapter.Spec) Script { return Script{Events: []v1.Event{{Kind: v1.EventText}}} }}
	ctx, cancel := context.WithCancel(context.Background())
	tr, _ := a.Start(ctx, adapter.Spec{})
	time.Sleep(20 * time.Millisecond)
	cancel()
	done := make(chan struct{})
	go func() { tr.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait hung after the context was cancelled")
	}
}
