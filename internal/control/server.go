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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/skkap/yad/internal/config"
)

// Handler is what the daemon answers with.
type Handler struct {
	Status func(ctx context.Context) Status
	// Stop asks the daemon for a graceful stop and returns at once; the
	// socket stays up until the process closes it, so `yad status` can
	// watch the stop happen. What graceful means is the daemon's.
	Stop func()
	// CloseSession is the owner's close of one session. It records the
	// close and returns; the workdir goes afterwards.
	CloseSession func(ctx context.Context, connection, session string) (SessionClose, error)
}

// Daemon is one process's hold on its profile: the lock, and the socket.
type Daemon struct {
	sock     string
	lock     *os.File
	ln       *net.UnixListener
	stopping atomic.Bool
	wg       sync.WaitGroup
	once     sync.Once
	closed   chan struct{}
}

// lockWait is how long Claim retries a lock it finds taken. A CLI probing
// whether a daemon runs holds a shared lock for microseconds; a daemon
// starting at that instant must not take the probe for a running daemon.
const lockWait = 500 * time.Millisecond

// connDeadline bounds one exchange on the socket, so a client that connects
// and says nothing holds a goroutine for seconds, not forever.
const connDeadline = 10 * time.Second

// Claim makes this process the profile's daemon: it takes the lock, removes a
// socket a dead daemon left behind, and listens. A profile whose daemon is
// alive is a *RunningError.
func Claim(p config.Paths) (*Daemon, error) {
	sock := p.Socket()
	if err := CheckPath(sock); err != nil {
		return nil, err
	}
	if err := checkDir(p.Data); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p.Lock(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the daemon lock: %w", err)
	}
	deadline := time.Now().Add(lockWait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		pid := readPID(f)
		f.Close()
		return nil, &RunningError{PID: pid, Socket: sock}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", p.Lock(), err)
	}
	d := &Daemon{sock: sock, lock: f, closed: make(chan struct{})}
	if err := d.claim(); err != nil {
		d.release()
		return nil, err
	}
	return d, nil
}

func (d *Daemon) claim() error {
	if err := d.lock.Truncate(0); err != nil {
		return err
	}
	if _, err := d.lock.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0); err != nil {
		return err
	}
	// Holding the lock proves no daemon owns a socket file found here: it is
	// what a crash left, and it goes.
	if err := os.Remove(d.sock); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the stale control socket: %w — delete %s by hand", err, d.sock)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: d.sock, Net: "unix"})
	if err != nil {
		return fmt.Errorf("listen on the control socket: %w", err)
	}
	d.ln = ln
	// bind(2) creates the file under the umask; the directory, checked
	// private, is what covers the moment before this.
	if err := os.Chmod(d.sock, 0o600); err != nil {
		ln.Close()
		return err
	}
	return nil
}

// Serve answers on the socket until ctx ends.
func (d *Daemon) Serve(ctx context.Context, h Handler) {
	go func() {
		select {
		case <-ctx.Done():
			d.ln.Close()
		case <-d.closed:
		}
	}()
	for {
		conn, err := d.ln.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			// A transient accept error (EMFILE) must not end the only way
			// to reach the daemon.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		d.wg.Go(func() { d.answer(ctx, conn, h) })
	}
	d.wg.Wait()
}

func (d *Daemon) answer(ctx context.Context, conn *net.UnixConn, h Handler) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(connDeadline))
	ctx, cancel := context.WithTimeout(ctx, connDeadline)
	defer cancel()
	var req Request
	res := Response{PID: os.Getpid()}
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		res.Error = "a malformed request — the control socket speaks only to this yad binary"
		json.NewEncoder(conn).Encode(res)
		return
	}
	switch req.Op {
	case "status":
		st := h.Status(ctx)
		st.PID = res.PID
		st.Stopping = d.stopping.Load()
		res.Status = &st
	case "stop":
		first := d.stopping.CompareAndSwap(false, true)
		// The answer goes before the stop starts, so the CLI hears it even
		// when the stop is quick.
		json.NewEncoder(conn).Encode(res)
		if first {
			h.Stop()
		}
		return
	case "close_session":
		if h.CloseSession == nil {
			res.Error = "this daemon closes no sessions — `yad daemon restart` after an upgrade"
			break
		}
		closed, err := h.CloseSession(ctx, req.Connection, req.Session)
		if err != nil {
			res.Error = err.Error()
			break
		}
		res.Closed = &closed
	default:
		res.Error = fmt.Sprintf("unknown request %q — the CLI and the daemon are different yad versions; `yad daemon restart` after an upgrade", strings.TrimSpace(req.Op))
	}
	json.NewEncoder(conn).Encode(res)
}

// Close removes the socket and releases the lock, in that order: the next
// daemon may start the moment the lock is free.
func (d *Daemon) Close() error {
	var err error
	d.once.Do(func() {
		close(d.closed)
		if d.ln != nil {
			// Closing a listener Go created removes its file.
			err = d.ln.Close()
			if errors.Is(err, net.ErrClosed) {
				err = nil
			}
		}
		d.release()
	})
	return err
}

func (d *Daemon) release() {
	syscall.Flock(int(d.lock.Fd()), syscall.LOCK_UN)
	d.lock.Close()
}

func readPID(f *os.File) int {
	b := make([]byte, 32)
	n, _ := f.ReadAt(b, 0)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b[:n])))
	return pid
}
