//go:build unix

package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/skkap/yad/internal/config"
)

// askTimeout bounds one request when the caller's context has no deadline. A
// status is a few store reads; a daemon that cannot answer in this is wedged.
const askTimeout = 5 * time.Second

// AskTimeoutForTests replaces askTimeout while it is positive. A test that
// wedges a daemon on purpose is waiting for a certainty, and at the shipped
// timeout it waits five seconds for it every time — sixteen across the stop
// ladder's cases. The shipped value stays where it is: shortening it in the
// binary would turn a daemon merely busy with a sync into a wedged one, which
// `yad status` reports as not answering and `yad daemon stop` signals rather
// than asks. Nothing outside a test may set it:
// TestOnlyTestsReachTheAskTimeout fails on any shipped file of the module that
// assigns it, this one included.
var AskTimeoutForTests time.Duration

// Holder says whether a daemon holds the profile's lock, and its pid. It is
// the answer that survives a daemon too wedged to answer on its socket.
func Holder(p config.Paths) (pid int, running bool, err error) {
	f, err := os.Open(p.Lock())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	// Shared, and let go at once: a daemon starting now retries past it.
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return 0, false, nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return 0, false, err
	}
	return readPID(f), true, nil
}

// UnresponsiveError is a daemon that holds the lock and does not answer.
type UnresponsiveError struct {
	PID int
	Err error
}

func (e *UnresponsiveError) Error() string {
	return fmt.Sprintf("the daemon (pid %d) is running but does not answer on its control socket (%v) — `yad daemon stop` signals it instead", e.PID, e.Err)
}

func (e *UnresponsiveError) Unwrap() error { return e.Err }

// Ask sends one request to the profile's daemon. No daemon is ErrNotRunning;
// one that is running and does not answer is an *UnresponsiveError.
func Ask(ctx context.Context, p config.Paths, op string) (Response, error) {
	return Send(ctx, p, Request{Op: op})
}

// Send is Ask with a whole request.
func Send(ctx context.Context, p config.Paths, req Request) (Response, error) {
	if req.Op == OpStop || req.Op == OpConfirm {
		// A stop sent as one line is held by the daemon and never acted on.
		return Response{}, fmt.Errorf("a stop is two lines on the control socket — call control.Stop, not Send")
	}
	sock := p.Socket()
	if err := CheckPath(sock); err != nil {
		return Response{}, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, askWait())
		defer cancel()
	}
	res, err := ask(ctx, sock, req)
	if err == nil {
		if res.Error != "" {
			return res, errors.New(res.Error)
		}
		return res, nil
	}
	return Response{}, unanswered(p, err)
}

// askWait is how long one answer may take.
func askWait() time.Duration {
	if AskTimeoutForTests > 0 {
		return AskTimeoutForTests
	}
	return askTimeout
}

// unanswered names what a failed exchange means: no daemon, or one that holds
// the lock and did not answer.
func unanswered(p config.Paths, err error) error {
	pid, running, herr := Holder(p)
	switch {
	case herr != nil:
		return errors.Join(err, herr)
	case !running:
		return ErrNotRunning
	}
	return &UnresponsiveError{PID: pid, Err: err}
}

// UnansweredStopError is a stop the daemon acknowledged and the CLI then
// confirmed, whose last answer did not arrive. The stop is delivered: a
// daemon reads the confirm and acts, or has lost its connection and acts on
// nothing, and in neither case may the CLI escalate on its own — a SIGTERM
// after a delivered stop is the runner's second, which cancels its runs.
type UnansweredStopError struct {
	PID int
	Err error
}

func (e *UnansweredStopError) Error() string {
	return fmt.Sprintf("the daemon (pid %d) took the stop request but its answer did not arrive (%v) — `yad status` says whether it is stopping", e.PID, e.Err)
}

func (e *UnansweredStopError) Unwrap() error { return e.Err }

// Stop asks the profile's daemon for a graceful stop, and returns its pid.
// No daemon is ErrNotRunning. A daemon that did not acknowledge the request is
// an *UnresponsiveError, and has not acted on it, so a signal is safe. One that
// acknowledged it, was sent the confirm, and then did not answer is an
// *UnansweredStopError, and must not be signalled for it.
func Stop(ctx context.Context, p config.Paths) (int, error) {
	sock := p.Socket()
	if err := CheckPath(sock); err != nil {
		return 0, err
	}
	res, delivered, err := stop(ctx, sock)
	switch {
	case err == nil && res.Error != "":
		return res.PID, errors.New(res.Error)
	case err == nil:
		return res.PID, nil
	case delivered:
		return res.PID, &UnansweredStopError{PID: res.PID, Err: err}
	}
	return 0, unanswered(p, err)
}

// stop is the CLI's half of the stop exchange (OpStop). delivered is whether
// the confirm was written: from that line on, the stop is the daemon's to act
// on, whatever happens to its answer. Each of the two answers gets askWait of
// its own, so a slow acknowledgement does not leave the last answer no time.
func stop(ctx context.Context, sock string) (res Response, delivered bool, err error) {
	wait := askWait()
	dctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(dctx, "unix", sock)
	if err != nil {
		return Response{}, false, err
	}
	defer conn.Close()
	// A read does not watch the context; an owner's Ctrl-C has to reach it
	// through the deadline.
	past := time.Unix(1, 0)
	defer context.AfterFunc(ctx, func() { conn.SetDeadline(past) })()
	arm := func() {
		dl := time.Now().Add(wait)
		if cdl, ok := ctx.Deadline(); ok && cdl.Before(dl) {
			dl = cdl
		}
		conn.SetDeadline(dl)
		if ctx.Err() != nil {
			conn.SetDeadline(past)
		}
	}
	arm()
	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	if err := enc.Encode(Request{Op: OpStop}); err != nil {
		return Response{}, false, err
	}
	var ack Response
	if err := dec.Decode(&ack); err != nil {
		return Response{}, false, fmt.Errorf("read the daemon's answer: %w", err)
	}
	if ack.Error != "" {
		return ack, false, nil
	}
	if !ack.Confirm {
		// A daemon older than the exchange: it acted on the request alone.
		return ack, true, nil
	}
	arm()
	// A failed write reached nobody, so the daemon has acted on nothing.
	if err := enc.Encode(Request{Op: OpConfirm}); err != nil {
		return Response{}, false, err
	}
	if err := dec.Decode(&res); err != nil {
		return ack, true, fmt.Errorf("read the daemon's answer: %w", err)
	}
	return res, true, nil
}

func ask(ctx context.Context, sock string, req Request) (Response, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", sock)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return Response{}, err
	}
	var res Response
	if err := json.NewDecoder(conn).Decode(&res); err != nil {
		return Response{}, fmt.Errorf("read the daemon's answer: %w", err)
	}
	return res, nil
}
