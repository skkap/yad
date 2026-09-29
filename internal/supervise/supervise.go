//go:build unix

// Package supervise is the one place YAD starts a child process.
//
// Every harness, git invocation, setup hook and version probe goes through
// Start, so every one gets the same treatment: its own process group, a
// scrubbed environment, pipes YAD owns, a bounded stderr tail, and a stop that
// escalates and then kills whatever the child left behind. Multica found only 8
// of 27 spawn sites using a process group before it centralised this; here there
// is one site from the start.
package supervise

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"
)

// Spec is what to run.
type Spec struct {
	Path string
	Args []string
	Dir  string
	// Env is added to the scrubbed parent environment; later entries win.
	Env []string
	// KeepEnv names variables Scrub would otherwise remove. Nothing outside
	// this package's tests sets it, and TestKeepEnvIsNotWired fails the day
	// something does, because two rules bind whoever wires it (DEV-62).
	//
	// Wiring it to the owner's configuration must refuse, or at least warn in
	// `yad doctor`, when a variable it keeps is one v1.AccountVariable names
	// and the harness has accounts. An owner who kept ANTHROPIC_API_KEY for
	// API-key billing would have every run spend the key while its events,
	// limits, failover and health all described the account the runner
	// picked, none of whose limits would ever fire: decision 0040's lie,
	// told by the owner's configuration instead of a hub's grant.
	//
	// And a check that decides whether an account can take a run
	// (account.LoggedIn) runs in the same environment as the run it answers
	// for. `claude auth status` in an environment holding a key the run is
	// denied says "logged in" about a credential the run never sees — DEV-26
	// shipped exactly that, and the check goes through Start for this reason.
	// So a variable kept for a run is kept for that harness's login check
	// too, or for neither. account.OwnLogin differs on purpose and only by
	// the token account's stored token, which it leaves out to judge a login
	// made beside it (decision 0054); it answers about that login, not about
	// what the account's runs will use.
	KeepEnv []string
	// Stdin, when true, gives the caller a writer to the child's stdin. Adapters
	// that speak a protocol over stdin need it held open until the turn ends.
	Stdin bool
	// NoTTY starts the child in a session of its own, with no controlling
	// terminal. git and ssh prompt through /dev/tty whatever their environment
	// says, and a runner started from a shell has one; with none to open, a
	// prompt fails at once instead of waiting for a person who is not there.
	// The child still leads its own process group, so the group signals below
	// reach it the same way.
	NoTTY bool
	// MergeStderr sends the child's stderr into the Stdout pipe, in the order it
	// was written, for a caller that reports one transcript — a setup hook's.
	// Stderr then returns nothing.
	MergeStderr bool
}

// StderrTail is how much of a child's stderr is kept. Enough for the last error
// message; bounded because a chatty child must not grow the runner's memory.
const StderrTail = 2 << 10

// stderrSettle is how long Wait gives the stderr copy to finish once the
// leader has exited. Only a descendant that escaped the group makes it wait
// this long.
const stderrSettle = 250 * time.Millisecond

// Process is a running child and its process group.
type Process struct {
	cmd    *exec.Cmd
	stdout *os.File
	stdin  *os.File
	tail   *tailBuffer
	done   chan struct{}
	err    error
	// ctxKilled is set before done closes, from endedByContext. Run reports
	// it as TimedOut.
	ctxKilled bool
}

// Start runs spec in a new process group. The caller owns Stdout: read it, then
// Close it. The group is killed as soon as the leader exits, so a descendant
// that stayed in the group cannot hold the pipe open — but one that left it
// with setsid can, and then EOF never comes. A caller that must finish bounds
// its read with a deadline and closes Stdout to end it. Stdin, when requested,
// is closed by the Process once the leader exits.
func Start(ctx context.Context, spec Spec) (*Process, error) {
	cmd := exec.Command(spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = append(Scrub(os.Environ(), spec.KeepEnv), spec.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if spec.NoTTY {
		// setsid makes the child its group's leader too; asking for setpgid as
		// well would fail, since a session leader cannot change its group.
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}

	// Pipes are made here rather than with StdoutPipe: exec closes its own pipes
	// in Wait, which loses output a reader has not consumed yet, and Wait blocks
	// on a copy goroutine for as long as any grandchild holds the write end.
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(outR, outW)
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	if spec.MergeStderr {
		cmd.Stderr = outW
	}
	var inR, inW *os.File
	if spec.Stdin {
		if inR, inW, err = os.Pipe(); err != nil {
			closeAll(outR, outW, errR, errW)
			return nil, err
		}
		cmd.Stdin = inR
	}
	if err := cmd.Start(); err != nil {
		closeAll(outR, outW, errR, errW, inR, inW)
		return nil, fmt.Errorf("start %s: %w", spec.Path, err)
	}
	closeAll(outW, errW, inR)

	p := &Process{cmd: cmd, stdout: outR, stdin: inW, tail: &tailBuffer{max: StderrTail}, done: make(chan struct{})}
	tailed := make(chan struct{})
	go func() {
		io.Copy(p.tail, errR)
		errR.Close()
		close(tailed)
	}()
	go func() {
		p.err = cmd.Wait()
		// Decided here, at the reap, because every later vantage point sees
		// the deadline and the exit ready together and cannot order them.
		p.ctxKilled = endedByContext(ctx.Err(), cmd.ProcessState)
		// The leader is gone; anything left in its group is a descendant holding
		// pipes or git locks. It dies now, even after a clean exit.
		p.signalGroup(syscall.SIGKILL)
		// A caller reading Stderr after Wait wants the child's last words —
		// git's error is its last line. A descendant that left the group can
		// hold the pipe open for ever, so the wait for them is bounded.
		select {
		case <-tailed:
		case <-time.After(stderrSettle):
		}
		// Nobody is left to read stdin, so its write end is ours to close. Stdout
		// is not: the caller may still be draining it.
		if p.stdin != nil {
			p.stdin.Close()
		}
		close(p.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			p.signalGroup(syscall.SIGKILL)
		case <-p.done:
		}
	}()
	return p, nil
}

// endedByContext says whether ctx's kill, rather than the leader, ended it:
// ctx had ended by the reap, and the leader died of SIGKILL. The leader leads
// the group, so the kill always reaches it; ctx.Err is set before Done closes
// and the kill follows Done, so a kill of ours is never reaped with ctxErr
// still nil. A leader that exited with a status, or died of any other signal —
// its own SIGSEGV or SIGABRT — got there on its own, however close the deadline
// was. A foreign SIGKILL landing in the same instant as the deadline is the one
// case this cannot tell apart, and calling that a timeout is harmless.
func endedByContext(ctxErr error, ps *os.ProcessState) bool {
	if ctxErr == nil {
		return false
	}
	if ps == nil {
		return true // Wait failed without a status; the deadline is the fact left
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

// Pid is the leader's pid, which is also the process group id.
func (p *Process) Pid() int { return p.cmd.Process.Pid }

// Stdout is the child's standard output. The caller reads it and closes it;
// nothing else will. Closing it unblocks a pending read.
func (p *Process) Stdout() io.ReadCloser { return p.stdout }

// Stdin is the child's standard input, or nil if Spec.Stdin was false. The
// caller may close it to signal end of input; the Process closes it anyway once
// the leader has exited.
func (p *Process) Stdin() io.WriteCloser {
	if p.stdin == nil {
		return nil
	}
	return p.stdin
}

// Stderr returns the last StderrTail bytes the child wrote to stderr.
func (p *Process) Stderr() string { return p.tail.String() }

// Done is closed when the leader has exited and its group has been killed.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait blocks until the leader exits and returns its exit error. Exit status is
// not success: callers decide success from the child's own output.
func (p *Process) Wait() error {
	<-p.done
	return p.err
}

// Step is how far a Stop had to escalate.
type Step string

const (
	StepExited     Step = "exited"     // it was already gone, or left on interrupt
	StepTerminated Step = "terminated" // SIGTERM to the group ended it
	StepKilled     Step = "killed"     // SIGKILL was needed
)

// Ladder is how Stop escalates. Interrupt is the harness's own graceful stop —
// Claude's interrupt control request, Codex's turn/interrupt — and may be nil.
type Ladder struct {
	Interrupt      func() error
	InterruptGrace time.Duration
	TermGrace      time.Duration
}

// DefaultLadder gives a harness ten seconds to finish its turn cleanly and five
// more to handle SIGTERM before the group is killed.
var DefaultLadder = Ladder{InterruptGrace: 10 * time.Second, TermGrace: 5 * time.Second}

// Stop ends the process, escalating interrupt → SIGTERM to the group → SIGKILL
// to the group, and returns the step that ended it.
func (p *Process) Stop(l Ladder) Step {
	if p.exited(0) {
		return StepExited
	}
	if l.Interrupt != nil {
		// An interrupt that fails to send is just a step to skip; the signals
		// behind it do not depend on the harness cooperating.
		_ = l.Interrupt()
		if p.exited(l.InterruptGrace) {
			return StepExited
		}
	}
	p.signalGroup(syscall.SIGTERM)
	if p.exited(l.TermGrace) {
		return StepTerminated
	}
	p.signalGroup(syscall.SIGKILL)
	<-p.done
	return StepKilled
}

// Terminate sends SIGTERM to the group, unless the leader has exited: its
// group is killed with it then, and the id may already belong to another.
// For a caller that runs the ladder's steps itself, around a stream it must
// keep reading.
func (p *Process) Terminate() {
	if !p.exited(0) {
		p.signalGroup(syscall.SIGTERM)
	}
}

func (p *Process) exited(within time.Duration) bool {
	if within <= 0 {
		select {
		case <-p.done:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(within)
	defer t.Stop()
	select {
	case <-p.done:
		return true
	case <-t.C:
		return false
	}
}

func (p *Process) signalGroup(sig syscall.Signal) {
	// A negative pid addresses the whole group. ESRCH means it is already empty,
	// which is the outcome we wanted.
	if err := syscall.Kill(-p.cmd.Process.Pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = p.cmd.Process.Signal(sig)
	}
}

// Scrub returns env without the variables a child must not inherit:
//   - CLAUDECODE and CLAUDE_CODE_* make a nested claude believe it is running
//     inside another Claude Code session and change its behaviour;
//   - every variable that chooses whose credential a harness uses —
//     v1.AccountVariable, the list a grant is refused by — since one set on
//     the machine ranks above the account's login and moves the run off it
//     while every report still names the account (DEV-62, decision 0060);
//   - YAD_* is the runner's own configuration, which a harness has no business
//     reading.
//
// From every child, not only a harness: a setup hook or a git hook that
// starts a harness would hand the variable on to it. A project that needs
// such a key for its own use is sent it as a grant under another name, which
// is what the grant refusal tells a hub.
//
// Only env is scrubbed. What a caller appends after it — a run's grants, an
// account's home and a token account's CLAUDE_CODE_OAUTH_TOKEN
// (account.TurnEnv, decision 0054) — reaches the child as given, and wins over
// anything left in env, os/exec keeping the last of a repeated name.
func Scrub(env []string, keep []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if contains(keep, name) || !scrubbed(name) {
			out = append(out, kv)
		}
	}
	return out
}

func scrubbed(name string) bool {
	return name == "CLAUDECODE" || strings.HasPrefix(name, "CLAUDE_CODE_") ||
		strings.HasPrefix(name, "YAD_") || accountVariable(name)
}

// harnessHomes are the account variables Scrub lets through. The owner's
// CLAUDE_CONFIG_DIR or CODEX_HOME is not a credential ranked above an
// account. With no accounts it is the harness's own login, which the default
// login check (decision 0053) and codex.DefaultHome read from this same
// environment; with an account, the account's home is appended after it and
// wins. CLAUDE_SECURESTORAGE_CONFIG_DIR is the same kind of variable — where
// Claude keeps that login, when it is not the home — and every account sets
// it to its own home the same way (decision 0070). internal/account's tests
// hold this set to the variables an account sets, so a harness that gains
// one cannot be missed here.
var harnessHomes = []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CODEX_HOME"}

func accountVariable(name string) bool {
	_, ok := v1.AccountVariable(name)
	return ok && !contains(harnessHomes, name)
}

// AccountVariables names the variables in env that Scrub removes because they
// choose a harness's credential, in env's order, for `yad doctor` and the
// daemon to warn about. Names only: the values are credentials. An empty one
// is left out — Scrub still removes it, but it chooses nothing, and a warning
// about it would teach the owner to skip the warnings.
func AccountVariables(env []string) []string {
	var out []string
	for _, kv := range env {
		if name, v, _ := strings.Cut(kv, "="); v != "" && accountVariable(name) && !contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func closeAll(fs ...*os.File) {
	for _, f := range fs {
		if f != nil {
			f.Close()
		}
	}
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
