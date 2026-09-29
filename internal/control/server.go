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
	// AccountsChanged re-reads config.toml's account lists and acts on the
	// one account the owner changed.
	AccountsChanged func(ctx context.Context, change AccountChange) (AccountResult, error)
	// ConnectionRemoved lets go of a connection config.toml no longer lists.
	ConnectionRemoved func(ctx context.Context, connection string) (ConnectionRemoval, error)
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
	// paths is the profile served, which the commands an answer offers act on.
	paths config.Paths
}

// lockWait is how long Claim retries a lock it finds taken. A CLI probing
// whether a daemon runs holds a shared lock for microseconds; a daemon
// starting at that instant must not take the probe for a running daemon.
const lockWait = 500 * time.Millisecond

// connDeadline bounds one exchange on the socket, so a client that connects
// and says nothing holds a goroutine for seconds, not forever.
const connDeadline = 10 * time.Second

// AccountsDeadline bounds an OpAccountsChanged, on both ends. It is longer
// than connDeadline because the answer waits on the harness's own login
// check, which account.LoggedIn allows ten seconds, and on the store writes
// after it: a daemon that gave up at connDeadline would answer with a timeout
// for a check that was about to answer.
const AccountsDeadline = 30 * time.Second

// RemovalDeadline bounds an OpConnectionRemoved, on both ends. The loop it
// stops may be in the middle of a sync, which the stop cuts short, and the
// store writes after it are one per session and run of that connection:
// seconds on a busy runner, never the connDeadline's ten on a healthy one.
// The runs it cancels are not waited for.
const RemovalDeadline = 30 * time.Second

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
		return nil, &RunningError{PID: pid, Socket: sock, Paths: p}
	}
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", p.Lock(), err)
	}
	d := &Daemon{sock: sock, lock: f, closed: make(chan struct{}), paths: p}
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
	// One decoder for the connection: a stop's confirm may already sit in
	// its buffer behind the request.
	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	if err := dec.Decode(&req); err != nil {
		res.Error = "a malformed request — the control socket speaks only to this yad binary"
		enc.Encode(res)
		return
	}
	switch req.Op {
	case "status":
		st := h.Status(ctx)
		st.PID = res.PID
		st.Stopping = d.stopping.Load()
		res.Status = &st
	case OpStop:
		d.stop(dec, enc, res, h)
		return
	case "close_session":
		if h.CloseSession == nil {
			res.Error = "this daemon closes no sessions — `" + d.paths.Command("daemon", "restart") + "` after an upgrade"
			break
		}
		closed, err := h.CloseSession(ctx, req.Connection, req.Session)
		if err != nil {
			res.Error = err.Error()
			break
		}
		res.Closed = &closed
	case OpAccountsChanged:
		if h.AccountsChanged == nil || req.Account == nil {
			res.Error = "this daemon takes no account changes — `" + d.paths.Command("daemon", "restart") + "` after an upgrade"
			break
		}
		// The request has been read, so only the answer is left to bound,
		// and connDeadline is too short for it (AccountsDeadline).
		conn.SetDeadline(time.Now().Add(AccountsDeadline))
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), AccountsDeadline)
		result, err := h.AccountsChanged(actx, *req.Account)
		cancel()
		if err != nil {
			res.Error = err.Error()
			break
		}
		res.Account = &result
	case OpConnectionRemoved:
		if h.ConnectionRemoved == nil {
			res.Error = "this daemon cannot let a connection go while it runs — `" + d.paths.Command("daemon", "restart") + "` ends what the removed connection left, now that config.toml no longer lists it"
			break
		}
		conn.SetDeadline(time.Now().Add(RemovalDeadline))
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RemovalDeadline)
		removed, err := h.ConnectionRemoved(rctx, req.Connection)
		cancel()
		if err != nil {
			res.Error = err.Error()
			break
		}
		res.Removed = &removed
	default:
		res.Error = fmt.Sprintf("unknown request %q — the CLI and the daemon are different yad versions; `%s` after an upgrade", strings.TrimSpace(req.Op), d.paths.Command("daemon", "restart"))
	}
	enc.Encode(res)
}

// stop is the daemon's half of the stop exchange (OpStop). It acts only on a
// confirm, which the CLI writes only once it has heard the acknowledgement —
// and from then on the CLI never signals for a stop that goes unanswered. A
// stop the CLI gave up on before the acknowledgement reached it (a daemon slow
// to accept, a reply late in transit) is therefore never acted on here, and the
// SIGTERM the CLI sends instead is the first stop the runner hears rather than
// the second, which would cancel its runs (decision 0029).
func (d *Daemon) stop(dec *json.Decoder, enc *json.Encoder, res Response, h Handler) {
	res.Confirm = true
	if err := enc.Encode(res); err != nil {
		return
	}
	var confirm Request
	if err := dec.Decode(&confirm); err != nil || confirm.Op != OpConfirm {
		return
	}
	first := d.stopping.CompareAndSwap(false, true)
	// Answered before it is acted on (decision 0027), so the CLI hears it
	// even when the stop is quick. Whether this answer lands no longer
	// decides anything: the CLI committed to the stop when it confirmed.
	res.Confirm = false
	enc.Encode(res)
	if first {
		h.Stop()
	}
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
