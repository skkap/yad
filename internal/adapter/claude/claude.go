// Package claude drives Claude Code: `claude -p` with stream-json in both
// directions, a YAD-chosen --session-id, stdin held open for the control
// protocol, and --include-partial-messages so the inactivity watchdog can tell
// a thinking turn from a wedged one (decision 0006, ARCHITECTURE.md §3).
//
// How a stream becomes a run — steering, interrupts, which result decides the
// outcome — is decision 0021.
package claude

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/supervise"
)

// Adapter is the Claude Code adapter.
type Adapter struct {
	// Raw, when set, receives every line Claude writes, unmodified — the local
	// diagnostic copy of the stream that is never sent to a hub (decision 0012),
	// and what the fixture recorder captures. Called once per Start.
	Raw func(spec adapter.Spec) io.Writer
}

func (Adapter) Harness() string { return "claude" }

// Timings a test may shorten.
var (
	// exitGrace is how long Claude gets to exit once its last result is in and
	// stdin is closed. It has nothing left to do but write its transcript.
	exitGrace = 10 * time.Second
	// drainGrace is how long the reader keeps going after Claude has exited, for
	// output already in the pipe. Anything holding the pipe open longer is a
	// descendant that escaped the process group.
	drainGrace = 2 * time.Second
	termGrace  = 5 * time.Second
)

// Settings keys this adapter reads from the owner's configuration.
const settingPermissionMode = "permission_mode"

// defaultPermissionMode applies when the owner has set none. A run is
// unattended and auto-approves (decision 0015): Claude's own default mode would
// deny every tool that needs a prompt, and a stock runner could not edit a
// file. An owner who wants less sets permission_mode in config.toml.
const defaultPermissionMode = "bypassPermissions"

// geteuid is swapped by tests; Claude's root check reads the same thing.
var geteuid = os.Geteuid

var newline = []byte{'\n'}

// requestPrefix marks the control requests YAD sends — interrupts — so their
// acknowledgements can be told from anything else Claude answers.
const requestPrefix = "yad-"

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Start spawns Claude for one run and writes the instruction to it.
func (a Adapter) Start(ctx context.Context, spec adapter.Spec) (adapter.Turn, error) {
	if spec.Binary == "" {
		return nil, errors.New("no claude binary resolved — run `yad doctor` to see where claude was looked for")
	}
	session := spec.NativeSessionID
	if session == "" {
		session = newUUID()
	} else if !uuidPattern.MatchString(session) {
		return nil, fmt.Errorf("claude session id %q is not a UUID — the session store is damaged; close the session and start a new one", session)
	}
	contextFile, err := writeContext(spec.Brief.Context)
	if err != nil {
		return nil, err
	}
	args, err := argv(spec, session, contextFile)
	if err != nil {
		removeFile(contextFile)
		return nil, err
	}
	instruction, err := userFrame(spec.Brief.Instruction)
	if err != nil {
		removeFile(contextFile)
		return nil, err
	}
	p, err := supervise.Start(ctx, supervise.Spec{Path: spec.Binary, Args: args, Dir: spec.Workdir, Env: runEnv(spec.Env), Stdin: true})
	if err != nil {
		removeFile(contextFile)
		return nil, fmt.Errorf("%w — check the claude binary at %s runs", err, spec.Binary)
	}

	t := &turn{
		ctx:       ctx,
		p:         p,
		session:   session,
		frames:    make(chan []byte, 16),
		written:   1,
		out:       make(chan v1.Event),
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
		final:     make(chan struct{}),
		eof:       make(chan struct{}),
		exitGrace: exitGrace, drainGrace: drainGrace, termGrace: termGrace,
	}
	// The instruction goes out from the writer goroutine, never from here: a
	// child that is not reading stdin yet would otherwise block Start.
	t.frames <- instruction
	go t.write(p.Stdin())
	go t.pump()
	var raw io.Writer
	if a.Raw != nil {
		raw = a.Raw(spec)
	}
	go t.read(raw, contextFile)
	go t.reap()
	return t, nil
}

// argv builds the command line. Nothing secret goes here — argv is visible to
// every user on the machine — and nothing from the hub may become a flag.
func argv(spec adapter.Spec, session, contextFile string) ([]string, error) {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		// Echoes each stdin frame as Claude consumes it: the only way to know a
		// steer has been taken, and so which result is the last (0021).
		"--replay-user-messages",
		// Headless, it returns an empty answer and the turn carries on as if the
		// question had been answered.
		"--disallowed-tools", "AskUserQuestion",
	}
	mode := permissionMode(spec)
	if strings.HasPrefix(mode, "-") {
		return nil, fmt.Errorf("permission_mode %q in config.toml is not a mode — see `claude --help` for the choices", mode)
	}
	if mode == "bypassPermissions" && geteuid() == 0 && os.Getenv("IS_SANDBOX") != "1" {
		// Claude refuses this itself — it prints to stderr and exits before
		// writing a stream — and a run that fails every time for a reason the
		// runner already knows is better refused here, with the way out.
		return nil, errors.New("claude refuses bypassPermissions as root, and the runner is root — run yad as an ordinary user (ARCHITECTURE.md §8); inside a disposable container, start the runner with IS_SANDBOX=1 in its environment; or set a narrower permission_mode under [harness.claude] in config.toml")
	}
	args = append(args, "--permission-mode", mode)
	if spec.NativeSessionID != "" {
		args = append(args, "--resume", session)
	} else {
		args = append(args, "--session-id", session)
	}
	if spec.Model != "" {
		// The model comes from the hub. It is one argv element, so it cannot
		// smuggle in a second flag, but one that is itself a flag could.
		if strings.HasPrefix(spec.Model, "-") {
			return nil, fmt.Errorf("model %q is not a model name — ask the hub to send an alias such as sonnet", spec.Model)
		}
		args = append(args, "--model", spec.Model)
	}
	if contextFile != "" {
		args = append(args, "--append-system-prompt-file", contextFile)
	}
	return args, nil
}

func permissionMode(spec adapter.Spec) string {
	if mode := spec.Settings[settingPermissionMode]; mode != "" {
		return mode
	}
	return defaultPermissionMode
}

// runEnv is the run's environment without IS_SANDBOX. That variable switches
// off Claude's refusal to bypass permissions as root, so only the owner may set
// it, in the runner's own environment. A run's environment carries grants from
// the hub, and a hub must never widen what a harness may do (0015).
func runEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); name != "IS_SANDBOX" {
			out = append(out, kv)
		}
	}
	return out
}

// writeContext puts the brief's context in a file rather than argv: it can be
// long, and argv is readable by everyone on the machine.
func writeContext(ctx string) (string, error) {
	if ctx == "" {
		return "", nil
	}
	f, err := os.CreateTemp("", "yad-claude-context-*.md")
	if err != nil {
		return "", fmt.Errorf("write the run's context for claude: %w — check that the temp directory is writable", err)
	}
	if _, err := f.WriteString(ctx); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("write the run's context for claude: %w — check free disk space", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func removeFile(path string) {
	if path != "" {
		os.Remove(path)
	}
}

func userFrame(text string) ([]byte, error) {
	b, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": text},
	})
	return append(b, '\n'), err
}

func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

type turn struct {
	ctx     context.Context
	p       *supervise.Process
	session string

	// Every write to stdin goes through frames, so the instruction, steers,
	// interrupts and permission answers never interleave.
	mu          sync.Mutex
	frames      chan []byte
	closing     bool // frames is closed; the turn takes no more input
	written     int  // user frames sent, the instruction included
	replayed    int  // user frames Claude has taken
	interrupted bool
	requests    int
	settled     bool // the result in hand answered every frame we sent

	// Events are queued without bound between the reader and the consumer. The
	// reader must never block on a consumer that has stopped reading, or Claude
	// blocks on a full pipe and Wait never returns. The queue is bounded by the
	// stream itself, which ends.
	qmu    sync.Mutex
	queue  []v1.Event
	closed bool
	wake   chan struct{}
	out    chan v1.Event

	final   chan struct{} // closed once the last result is in
	eof     chan struct{} // closed once Claude's output has ended
	done    chan struct{}
	outcome adapter.Outcome

	exitGrace, drainGrace, termGrace time.Duration
}

func (t *turn) Events() <-chan v1.Event { return t.out }

func (t *turn) NativeSessionID() string { return t.session }

func (t *turn) Wait() adapter.Outcome {
	<-t.done
	return t.outcome
}

// Steer sends more input into the running turn. Claude reads it at the next
// tool boundary; a turn with none left finishes first and then answers it as a
// follow-up in the same run (0021).
func (t *turn) Steer(text string) error {
	frame, err := userFrame(text)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing || t.interrupted {
		return errors.New("the run has already finished — send this as a new run in the same session")
	}
	select {
	case t.frames <- frame:
		t.written++
		return nil
	default:
		return errors.New("claude is not reading its input — too many steers are waiting; try again once the run has made progress")
	}
}

// Interrupt asks Claude to stop the turn. The session stays resumable; the run
// ends with the result Claude sends back. A turn already over is not an error.
func (t *turn) Interrupt() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing || t.interrupted {
		return nil
	}
	t.requests++
	frame, _ := json.Marshal(map[string]any{
		"type":       "control_request",
		"request_id": fmt.Sprintf("%s%d", requestPrefix, t.requests),
		"request":    map[string]any{"subtype": "interrupt"},
	})
	select {
	case t.frames <- append(frame, '\n'):
		t.interrupted = true
		return nil
	default:
		return errors.New("claude is not reading its input — the cancel ladder's signals will stop it")
	}
}

// Terminate is the ladder's SIGTERM, for a Claude that did not stop when
// interrupted.
func (t *turn) Terminate() error {
	t.p.Terminate()
	return nil
}

// deny refuses a permission prompt. The prompt only exists because the owner's
// permission mode did not allow the tool; the answer is theirs, already given.
func (t *turn) deny(requestID string) {
	frame, _ := json.Marshal(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response": map[string]any{
				"behavior": "deny",
				"message":  "This run is unattended and its permission mode does not allow this tool. The runner's owner sets the mode in config.toml.",
			},
		},
	})
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return
	}
	select {
	case t.frames <- append(frame, '\n'):
	default:
	}
}

// settle closes stdin once the result in hand is the last one Claude will
// send: every frame we sent has been taken and nothing is queued behind it, or
// an interrupt has made Claude drop whatever was. Closing stdin is how Claude
// learns there is no more input and exits. The result is final only in the
// first case — after an interrupt, a steer still waiting was dropped, not
// answered.
func (t *turn) settle(queued int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return
	}
	complete := t.replayed >= t.written && queued == 0
	if !complete && !t.interrupted {
		return
	}
	t.closing = true
	t.settled = complete
	close(t.frames)
	close(t.final)
}

// closeInput ends input without waiting for a result — the stream is over.
func (t *turn) closeInput() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.closing {
		t.closing = true
		close(t.frames)
	}
}

func (t *turn) write(stdin io.WriteCloser) {
	failed := false
	for f := range t.frames {
		if failed {
			continue // drain, so senders holding mu never block
		}
		if _, err := stdin.Write(f); err != nil {
			// Claude has gone; the reader will see the stream end and say why.
			failed = true
		}
	}
	stdin.Close()
}

func (t *turn) read(raw io.Writer, contextFile string) {
	defer removeFile(contextFile)
	tr := newTranslator(t.session, t.enqueue)
	// One channel for lines and skipped lines alike, so a skipped line is
	// reported where it was in the stream.
	type item struct {
		line    []byte
		tooLong *adapter.ErrLineTooLong
	}
	lines := make(chan item)
	go func() {
		defer close(lines)
		lr := adapter.NewLineReader(t.p.Stdout())
		for {
			line, err := lr.Next()
			var tooLong *adapter.ErrLineTooLong
			if errors.As(err, &tooLong) {
				lines <- item{tooLong: tooLong}
				continue
			}
			if err != nil {
				return // EOF, or the pipe closed under us by reap
			}
			if raw != nil {
				raw.Write(line)
				raw.Write(newline)
			}
			// line aliases the reader's buffer, which the next read reuses.
			lines <- item{line: bytes.Clone(line)}
		}
	}()
	tick := time.NewTicker(textFlush / 4)
	defer tick.Stop()
loop:
	for {
		select {
		case <-tick.C:
			tr.tick()
		case it, ok := <-lines:
			switch {
			case !ok:
				break loop
			case it.tooLong != nil:
				tr.flush()
				tr.streamError(it.tooLong.Error())
			default:
				t.handle(tr, it.line)
			}
		}
	}
	t.p.Stdout().Close()
	t.closeInput()
	close(t.eof)
	exitErr := t.p.Wait()

	t.mu.Lock()
	e := ended{
		interrupted: t.interrupted,
		cancelled:   t.ctx.Err() != nil,
		exitErr:     exitErr,
		stderr:      t.p.Stderr(),
		final:       t.settled,
	}
	t.mu.Unlock()
	t.outcome = tr.outcome(e)
	t.qmu.Lock()
	t.closed = true
	t.qmu.Unlock()
	t.notify()
	close(t.done)
}

func (t *turn) handle(tr *translator, line []byte) {
	r := tr.line(line)
	if r.replayed {
		t.mu.Lock()
		t.replayed++
		t.mu.Unlock()
	}
	if r.deny != "" {
		t.deny(r.deny)
	}
	if r.mismatch {
		// Whatever Claude does next, it does without the conversation it was
		// meant to continue.
		t.Interrupt()
	}
	if r.result {
		t.settle(tr.result.QueuedTurnCount)
	}
}

// reap makes sure the reader ends: Claude is stopped if it lingers after its
// last result or after its output has ended, and the pipe is closed if
// something outlives Claude holding it.
func (t *turn) reap() {
	lingering := true
	select {
	case <-t.p.Done():
		lingering = false
	case <-t.final:
	case <-t.eof:
	}
	if lingering {
		select {
		case <-t.p.Done():
		case <-time.After(t.exitGrace):
			t.p.Stop(supervise.Ladder{TermGrace: t.termGrace})
		}
	}
	select {
	case <-t.done:
	case <-time.After(t.drainGrace):
		t.p.Stdout().Close()
	}
}

func (t *turn) enqueue(e v1.Event) {
	t.qmu.Lock()
	t.queue = append(t.queue, e)
	t.qmu.Unlock()
	t.notify()
}

func (t *turn) notify() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// pump hands queued events to the consumer in order and closes the channel
// after the last. It gives up when the run's context ends, so a consumer that
// cancelled and stopped reading does not leave it blocked for ever.
func (t *turn) pump() {
	defer close(t.out)
	for {
		t.qmu.Lock()
		batch := t.queue
		t.queue = nil
		closed := t.closed
		t.qmu.Unlock()
		for _, e := range batch {
			select {
			case t.out <- e:
			case <-t.ctx.Done():
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
		case <-t.wake:
		case <-t.ctx.Done():
			return
		}
	}
}
