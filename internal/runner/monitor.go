package runner

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// Monitor is the runner as `yad status` sees it: each connection's last sync
// and last error, the capacity pool, and what the store holds. Serve fills it;
// the control socket reads it. A nil Monitor records nothing.
type Monitor struct {
	mu    sync.Mutex
	conns map[string]ConnectionState
	pool  *Pool
	store *store.Store
}

// Connection states.
const (
	ConnStarting = "starting" // no sync has finished yet
	ConnSyncing  = "syncing"  // the last sync succeeded
	ConnRetrying = "retrying" // the last sync failed, and the loop backs off
	ConnStopped  = "stopped"  // the loop gave up; the owner has to act
)

// ConnectionState is one connection's recent history.
type ConnectionState struct {
	State       string
	LastSync    time.Time // zero until a sync succeeds
	LastError   string
	LastErrorAt time.Time
}

// NewMonitor returns an empty monitor.
func NewMonitor() *Monitor { return &Monitor{conns: map[string]ConnectionState{}} }

func (m *Monitor) attach(p *Pool, st *store.Store) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pool, m.store = p, st
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
