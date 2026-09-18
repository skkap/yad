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
	sock := p.Socket()
	if err := CheckPath(sock); err != nil {
		return Response{}, err
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, askTimeout)
		defer cancel()
	}
	res, err := ask(ctx, sock, op)
	if err == nil {
		if res.Error != "" {
			return res, errors.New(res.Error)
		}
		return res, nil
	}
	pid, running, herr := Holder(p)
	switch {
	case herr != nil:
		return Response{}, errors.Join(err, herr)
	case !running:
		return Response{}, ErrNotRunning
	}
	return Response{}, &UnresponsiveError{PID: pid, Err: err}
}

func ask(ctx context.Context, sock, op string) (Response, error) {
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", sock)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if err := json.NewEncoder(conn).Encode(Request{Op: op}); err != nil {
		return Response{}, err
	}
	var res Response
	if err := json.NewDecoder(conn).Decode(&res); err != nil {
		return Response{}, fmt.Errorf("read the daemon's answer: %w", err)
	}
	return res, nil
}
