package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/acp/acptest"
)

// The runner's contract for Interrupt (internal/runner, control): an error
// means the interrupt did not reach the harness, and the hub's repeat is
// tried again; nil means it was delivered and is not tried again. So a
// cancel the agent's full input refused must be sent by a repeat, the turn
// must end cancelled, and once one is delivered no repeat sends another.
func TestAnInterruptThatFailedIsSentByItsRepeat(t *testing.T) {
	fixture, _ := filepath.Abs(filepath.Join("testdata", "agent", "turn.jsonl"))
	dir := t.TempDir()
	gate, log := filepath.Join(dir, "gate"), filepath.Join(dir, "log.jsonl")
	spec := adapter.Spec{
		RunID: "r1", Binary: os.Args[0], Workdir: t.TempDir(), Model: "a",
		Env: []string{
			acptest.EnvFixture + "=" + fixture, acptest.EnvLog + "=" + log,
			acptest.EnvMode + "=stalled", acptest.EnvGate + "=" + gate,
			"GORACE=atexit_sleep_ms=0",
		},
		Brief: v1.Brief{Instruction: "do it"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tr, err := Start(ctx, testAgent, spec)
	if err != nil {
		t.Fatal(err)
	}
	declined := 0
	var first, repeat error
	for e := range tr.Events() {
		if e.Kind != v1.EventStatus || !strings.HasPrefix(e.Status, "permission declined") {
			continue
		}
		if declined++; declined != acptest.StalledRequests {
			continue
		}
		// Every answer is queued or refused by now: the input is full.
		first = tr.Interrupt()
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		// The hub repeats it at every sync until the runner marks it.
		repeat = tr.Interrupt()
		for deadline := time.Now().Add(10 * time.Second); repeat != nil && time.Now().Before(deadline); repeat = tr.Interrupt() {
			time.Sleep(20 * time.Millisecond)
		}
		if err := tr.Interrupt(); err != nil {
			t.Errorf("an interrupt after one was delivered: %v", err)
		}
	}
	o := tr.Wait()
	if first == nil {
		t.Fatal("the first interrupt reported delivered, but the agent's input was full")
	}
	if repeat != nil {
		t.Fatalf("the hub's repeats never got the interrupt through: %v", repeat)
	}
	if o.State != v1.RunCancelled {
		t.Errorf("outcome %s %+v after an interrupt the adapter reported delivered", o.State, o.Error)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "session/cancel"); n != 1 {
		t.Errorf("the agent read session/cancel %d times, want once", n)
	}
}
