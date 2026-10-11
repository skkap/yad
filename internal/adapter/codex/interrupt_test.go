package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/codex/codextest"
)

// The runner's contract for Interrupt (internal/runner, control): an error
// means the interrupt did not reach the harness, and the hub's repeat is
// tried again; nil means it was delivered and is not tried again. So a
// turn/interrupt Codex's full input refused must be sent by a repeat, the
// turn must end cancelled, and once one is delivered no repeat sends another.
func TestAnInterruptThatFailedIsSentByItsRepeat(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	h := &harness{fixture: fixture("plain"), env: map[string]string{"CODEX_TEST_MODE": "stalled", "CODEX_TEST_GATE": gate}}
	declined := 0
	var first, repeat error
	_, out, _ := drive(t, context.Background(), h.spec(t), func(tr adapter.Turn, e v1.Event) bool {
		if e.Kind != v1.EventStatus || !strings.HasPrefix(e.Status, "approval declined") {
			return false
		}
		if declined++; declined != codextest.StalledRequests {
			return false
		}
		// Every answer is queued or refused by now: the input is full.
		first = tr.Interrupt()
		if err := os.WriteFile(gate, nil, 0o600); err != nil {
			t.Error(err)
		}
		// The hub repeats it at every sync until the runner marks it.
		repeat = tr.Interrupt()
		for deadline := time.Now().Add(10 * time.Second); repeat != nil && time.Now().Before(deadline); repeat = tr.Interrupt() {
			time.Sleep(20 * time.Millisecond)
		}
		if err := tr.Interrupt(); err != nil {
			t.Errorf("an interrupt after one was delivered: %v", err)
		}
		return true
	})
	if first == nil {
		t.Fatal("the first interrupt reported delivered, but Codex's input was full")
	}
	if repeat != nil {
		t.Fatalf("the hub's repeats never got the interrupt through: %v", repeat)
	}
	if out.State != v1.RunCancelled {
		t.Errorf("outcome %s %+v after an interrupt the adapter reported delivered", out.State, out.Error)
	}
	if n := len(h.seen(t).sent("turn/interrupt")); n != 1 {
		t.Errorf("Codex read turn/interrupt %d times, want once", n)
	}
}
