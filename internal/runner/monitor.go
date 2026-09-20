package runner

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// Monitor is the runner as `yad status` sees it: each connection's last sync
// and last error, the capacity pool, and what the store holds. Serve fills it;
// the control socket reads it. A nil Monitor records nothing.
type Monitor struct {
	mu       sync.Mutex
	conns    map[string]ConnectionState
	pool     *Pool
	store    *store.Store
	sessions *Collector
	ready    bool
	// stops ends one connection's loop and, with it, its reporter. Serve
	// fills them; `yad disconnect` is what uses them.
	stops map[string]context.CancelFunc
}

// Connection states.
const (
	ConnStarting = "starting" // no sync has finished yet
	ConnSyncing  = "syncing"  // the last sync succeeded
	ConnRetrying = "retrying" // the last sync failed, and the loop backs off
	ConnStopped  = "stopped"  // the loop gave up; the owner has to act
	// ConnGone is a connection `yad disconnect` retired while the daemon was
	// running. It is not in config.toml any more, so `yad status` leaves it
	// out rather than showing the config this process started with.
	ConnGone = "disconnected"
)

// ConnectionState is one connection's recent history, and its standing in the
// capacity pool.
type ConnectionState struct {
	State       string
	LastSync    time.Time // zero until a sync succeeds
	LastError   string
	LastErrorAt time.Time
	// Held is how many runs this connection has of the pool, and Cap the
	// owner's bound on it — 0 when only the runner's capacity bounds it. A
	// cap nobody can see is a cap nobody can trust, so `yad status` shows it.
	Held, Cap int
}

// NewMonitor returns an empty monitor.
func NewMonitor() *Monitor {
	return &Monitor{conns: map[string]ConnectionState{}, stops: map[string]context.CancelFunc{}}
}

// register records how to stop one connection's loop.
func (m *Monitor) register(conn string, stop context.CancelFunc) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stops[conn] = stop
}

func (m *Monitor) attach(p *Pool, st *store.Store, sessions *Collector) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pool, m.store, m.sessions = p, st, sessions
}

// ErrNoSessions is a close asked of a runner with no store open: no
// connection configured, or not yet started.
var ErrNoSessions = errors.New("the runner holds no sessions yet — it has no hub connected, or is still starting; try again in a moment")

// CloseSession is the owner's `yad sessions close`: the session closes now,
// or once the run held in it ends, and its hub hears it was closed by the
// owner.
func (m *Monitor) CloseSession(ctx context.Context, connection, id string) (CloseResult, error) {
	m.mu.Lock()
	sessions := m.sessions
	m.mu.Unlock()
	if sessions == nil {
		return CloseResult{}, ErrNoSessions
	}
	return sessions.Close(ctx, connection, id, v1.SessionClosedByOwner)
}

// Disconnected is the runner's side of `yad disconnect`, which has already
// retired this runner's registration with that hub.
type Disconnected struct {
	// Running is whether a loop for that connection was still syncing.
	Running bool
	// Runs is how many of that hub's runs are still held here.
	Runs int
	// Closed and Closing are its sessions: closed now, or closing once the
	// run held in them ends.
	Closed, Closing int
}

// Disconnect stops syncing one connection and closes every session it holds.
// The hub is gone, so its sessions can never take another run; closing them
// is what gets their workdirs reclaimed, since the collector reclaims from
// session rows and nothing else would ever close these (decisions 0011, 0035).
// The runs held are left to finish where they are — the hub has marked them
// lost, and killing a run the owner did not ask to kill would throw the work
// away.
func (m *Monitor) Disconnect(ctx context.Context, connection string) (Disconnected, error) {
	if m == nil {
		return Disconnected{}, nil
	}
	m.mu.Lock()
	stop, running := m.stops[connection]
	delete(m.stops, connection)
	sessions, st := m.sessions, m.store
	m.mu.Unlock()
	out := Disconnected{Running: running}
	m.update(connection, func(c *ConnectionState) { c.State = ConnGone })
	// Sessions first, while the loop still holds the store open: stopping the
	// last connection ends Serve, and Serve closes the store on its way out.
	if st != nil && sessions != nil {
		if err := m.letGo(ctx, connection, st, sessions, &out); err != nil {
			// Stop syncing anyway: the hub has already retired this runner,
			// so every sync from here is a refused credential. What was not
			// reclaimed is swept at the next start.
			if running {
				stop()
			}
			return out, err
		}
	}
	if running {
		stop()
	}
	return out, nil
}

// letGo counts what the connection still holds and closes every session of
// it, so the collector reclaims their workdirs.
func (m *Monitor) letGo(ctx context.Context, connection string, st *store.Store, sessions *Collector, out *Disconnected) error {
	held, err := st.ListHeldRuns(ctx, connection)
	if err != nil {
		return err
	}
	out.Runs = len(held)
	rows, err := st.ListSessions(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rows {
		if r.Connection != connection || r.State != "open" {
			continue
		}
		res, err := sessions.Close(ctx, connection, r.ID, v1.SessionClosedByOwner)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("close session %s: %w", r.ID, err))
		case res.Outcome == CloseDone:
			out.Closed++
		case res.Outcome == CloseWaiting:
			out.Closing++
		}
	}
	return errors.Join(errs...)
}

func (m *Monitor) markReady() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ready = true
}

// Ready is whether Serve got past its setup: the store is open and at least
// one connection's loop is running, or there is no connection to run. What a
// hub answers to the first sync comes later, and shows in Connections.
func (m *Monitor) Ready() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ready
}

func (m *Monitor) update(conn string, fn func(*ConnectionState)) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conns[conn]
	if !ok {
		c.State = ConnStarting
	}
	fn(&c)
	m.conns[conn] = c
}

func (m *Monitor) starting(conn string) {
	m.update(conn, func(*ConnectionState) {})
}

func (m *Monitor) synced(conn string, at time.Time) {
	m.update(conn, func(c *ConnectionState) { c.State, c.LastSync = ConnSyncing, at })
}

func (m *Monitor) failed(conn string, err error, at time.Time, state string) {
	m.update(conn, func(c *ConnectionState) {
		c.State, c.LastError, c.LastErrorAt = state, err.Error(), at
	})
}

// Snapshot is the runner at one moment.
type Snapshot struct {
	Connections map[string]ConnectionState
	// Capacity is nil while no pool exists: a runner with no connection
	// holds none, and claims nothing.
	Capacity *struct{ Total, Free int }
	Held     []db.Run
	Sessions int
	Spool    int
	Outbox   int
}

// Snapshot reads the monitor and the store. A store read that fails is in the
// error beside a snapshot filled as far as it got.
func (m *Monitor) Snapshot(ctx context.Context) (Snapshot, error) {
	m.mu.Lock()
	s := Snapshot{Connections: make(map[string]ConnectionState, len(m.conns))}
	for k, v := range m.conns {
		s.Connections[k] = v
	}
	pool, st := m.pool, m.store
	m.mu.Unlock()
	if pool != nil {
		pool.mu.Lock()
		s.Capacity = &struct{ Total, Free int }{pool.total, pool.total - pool.used}
		for name, c := range s.Connections {
			if pc, ok := pool.conns[name]; ok {
				c.Held, c.Cap = pc.held, pc.cap
				s.Connections[name] = c
			}
		}
		pool.mu.Unlock()
	}
	if st == nil {
		return s, nil
	}
	var errs []error
	var err error
	s.Held, err = st.ListAllHeldRuns(ctx)
	errs = append(errs, err)
	n, err := st.CountOpenSessions(ctx)
	s.Sessions = int(n)
	errs = append(errs, err)
	n, err = st.SpoolDepth(ctx)
	s.Spool = int(n)
	errs = append(errs, err)
	n, err = st.OutboxDepth(ctx)
	s.Outbox = int(n)
	errs = append(errs, err)
	return s, errors.Join(errs...)
}
