package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// stalledTurn is a turn whose interrupt, deferred to the turn's id, Codex's
// full input refused when the id came: the writer holds a line Codex has not
// read and the queue behind it is full. Codex reads again from then on;
// drained returns once it has read everything queued before the interrupt,
// methods what it read once input has closed.
func stalledTurn(t *testing.T) (tr *turn, drained func(), methods func() []string) {
	t.Helper()
	pr, pw := io.Pipe()
	w := &first{WriteCloser: pw, writing: make(chan struct{})}
	in := newInput(w)
	tr = &turn{
		thread: "th", conn: NewConn(in), in: in, interrupted: true,
		tr: newTranslator(func(v1.Event) {}), wait: map[int64]string{},
		begun: make(chan struct{}), finished: make(chan struct{}),
	}
	// The writer holds its first line before the queue is filled, so nothing
	// frees a slot until Codex reads.
	tr.conn.Notify("filler", nil)
	<-w.writing
	queued := 1
	for tr.conn.Notify("filler", nil) == nil {
		queued++
	}
	tr.begin("turn-1")
	if !tr.owed {
		t.Fatal("begin queued turn/interrupt into a full input")
	}
	var got []string
	empty, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(pr)
		fillers := 0
		for sc.Scan() {
			var m struct {
				Method string `json:"method"`
			}
			json.Unmarshal(sc.Bytes(), &m)
			got = append(got, m.Method)
			if m.Method == "filler" {
				if fillers++; fillers == queued {
					close(empty)
				}
			}
		}
	}()
	wait := func(ch <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatal(what)
		}
	}
	drained = func() {
		t.Helper()
		wait(empty, "Codex never read what was queued")
	}
	methods = func() []string {
		t.Helper()
		wait(done, "the input writer never finished")
		return got
	}
	return tr, drained, methods
}

// first closes writing when the first write begins.
type first struct {
	io.WriteCloser
	once    sync.Once
	writing chan struct{}
}

func (f *first) Write(b []byte) (int, error) {
	f.once.Do(func() { close(f.writing) })
	return f.WriteCloser.Write(b)
}

// An owed interrupt goes once Codex reads again, once only, and never after
// the turn has ended: completed with input still open for a usage limit's
// question, or with input closed (DEV-185).
func TestAnOwedInterruptIsSentOnceWhileTheTurnRuns(t *testing.T) {
	for _, c := range []struct {
		name string
		end  func(*testing.T, *turn)
		want int
	}{
		{"while the turn runs", nil, 1},
		{"after turn/completed asks for the limits", func(t *testing.T, tr *turn) {
			tr.tr.done = &turnInfo{ID: "turn-1", Status: "failed", Error: &turnError{CodexErrorInfo: json.RawMessage(`"usageLimitExceeded"`)}}
			tr.completed()
			if tr.over {
				t.Fatal("the limits were not asked for, so the window after turn/completed is not tested")
			}
		}, 0},
		{"after input closed", func(_ *testing.T, tr *turn) { tr.stop() }, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr, drained, methods := stalledTurn(t)
			drained()
			select {
			case <-tr.in.room:
			default:
				t.Fatal("no room was signalled once Codex read again")
			}
			if c.end != nil {
				c.end(t, tr)
			}
			for range 3 {
				tr.retryInterrupt()
			}
			if err := tr.Interrupt(); err != nil {
				t.Errorf("a repeat of an owed interrupt: %v", err)
			}
			tr.stop()
			n := 0
			for _, m := range methods() {
				if m == "turn/interrupt" {
					n++
				}
			}
			if n != c.want {
				t.Errorf("Codex read turn/interrupt %d times, want %d", n, c.want)
			}
		})
	}
}

// An interrupt that arrives after turn/start but before the turn has an id is
// deferred to the id, and Interrupt has already said it was delivered: the
// runner will not try it again. So when Codex's input is full as the id
// arrives, the adapter must send it once Codex reads again (DEV-185).
func TestADeferredInterruptRefusedAtTheTurnsIDIsSentOnceCodexReads(t *testing.T) {
	dir := t.TempDir()
	gate, drain := filepath.Join(dir, "gate"), filepath.Join(dir, "drain")
	h := &harness{fixture: fixture("plain"), env: map[string]string{
		"CODEX_TEST_MODE": "stalled-start", "CODEX_TEST_GATE": gate, "CODEX_TEST_DRAIN": drain,
		// Long enough for the backlog of answers to drain first; with the
		// interrupt lost the test fails after it.
		"CODEX_TEST_WAIT": "5s",
	}}
	declined := 0
	var deferred error
	_, out, _ := drive(t, context.Background(), h.spec(t), func(tr adapter.Turn, e v1.Event) bool {
		if e.Kind == v1.EventStatus && strings.HasPrefix(e.Status, "approval declined") {
			if declined++; declined == codextest.StalledRequests {
				// Every answer is queued or refused, and the turn has no id.
				deferred = tr.Interrupt()
				if err := os.WriteFile(gate, nil, 0o600); err != nil {
					t.Error(err)
				}
			}
			return false
		}
		if e.Kind != v1.EventText || declined < codextest.StalledRequests {
			return false
		}
		// The turn's text comes after its id, so begin has met the full
		// input by now. Codex reads again.
		if err := os.WriteFile(drain, nil, 0o600); err != nil {
			t.Error(err)
		}
		return true
	})
	if deferred != nil {
		t.Fatalf("an interrupt before the turn's id: %v", deferred)
	}
	if out.State != v1.RunCancelled {
		t.Errorf("outcome %s %+v after an interrupt the adapter reported delivered", out.State, out.Error)
	}
	if n := len(h.seen(t).sent("turn/interrupt")); n != 1 {
		t.Errorf("Codex read turn/interrupt %d times, want once", n)
	}
}
