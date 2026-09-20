package runner

import (
	"sync"

	v1 "github.com/skkap/yad/protocol/v1"
)

// Pool is the runner's capacity: one shared count, with the owner's optional
// cap per harness and per connection. Every run held — claimed and waiting to
// start, or with the executor — holds one unit until its release is called.
//
// Capacity is taken before a sync asks for work, never after an offer arrives
// (decision 0005, Multica's lesson): a run the hub hands over must never wait
// for room, and two connections syncing at once must not both promise the
// same unit. A connection's cap is part of that same taking — it bounds the
// reservation a sync advertises and is consulted nowhere else, so holding more
// than the cap is not a state a claim can discover and then have to undo.
//
// Capacity goes to connections in turn (0005): the runner's units are dealt
// one at a time around the ring of connections, from where the last unit taken
// left off, and a sync may take what is free up to the turns its connection
// has coming beyond the runs it already holds. A connection that leaves units
// unused has no work for them and passes — it is skipped in the dealing until
// its own next sync asks again — so one busy hub still fills a pool the others
// have no queue for, while a hub that is quiet now finds its turn waiting for
// it when its work arrives. A unit a turn holds back is not reserved, so it
// waits at most until that connection's next sync, an interval away.
type Pool struct {
	mu        sync.Mutex
	total     int
	caps      map[string]int
	used      int // held runs plus units reserved by syncs in flight
	byHarness map[string]int

	// order is the ring, in the order connections joined it, and cursor is
	// whose turn the next free unit is.
	order  []string
	conns  map[string]*poolConn
	cursor int
}

// poolConn is one connection's standing in the pool.
type poolConn struct {
	idx  int
	cap  int // 0: only the runner's own capacity bounds this connection
	held int
	// asking is whether this connection still wants what its turn would give
	// it. A sync that closes its reservation with units left over had no work
	// for them and passes until it asks again, which covers a hub with an
	// empty queue, a sync that failed before it could claim, and — through
	// Pass — a loop that is draining or has stopped. One mark, so no share is
	// ever held for a connection that cannot take it.
	asking bool
}

// NewPool builds a pool from the capacity the runner advertises.
func NewPool(c v1.Capacity) *Pool {
	caps := map[string]int{}
	for id, n := range c.ByHarness {
		caps[id] = n
	}
	return &Pool{total: c.Total, caps: caps, byHarness: map[string]int{}, conns: map[string]*poolConn{}}
}

// Join puts a connection in the ring under the owner's cap for it, where 0 is
// no cap of its own. Joining in the order config.toml lists them makes the
// turn order the owner's.
func (p *Pool) Join(conn string, cap int) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conn(conn).cap = cap
}

// Pass gives up this connection's turn until its next Reserve: it is draining,
// still replaying what a previous process owed, or its loop has stopped. What
// it holds stays held; only its claim on free capacity goes.
func (p *Pool) Pass(conn string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.conn(conn).asking = false
}

// conn is a connection's standing, created asking on first sight: a hub
// nothing is yet known about is assumed to want work until a sync says so.
func (p *Pool) conn(name string) *poolConn {
	if c, ok := p.conns[name]; ok {
		return c
	}
	c := &poolConn{idx: len(p.order), asking: true}
	p.conns[name], p.order = c, append(p.order, name)
	return c
}

// headroom is how many more units this connection may hold under its cap.
func (p *Pool) headroom(c *poolConn) int {
	if c.cap <= 0 {
		return p.total
	}
	return max(c.cap-c.held, 0)
}

// quota is how many of the runner's units this connection's turn entitles it
// to hold: the whole capacity dealt one at a time around the ring, from where
// the last unit taken left off. It is reckoned over the capacity rather than
// over what happens to be free, so a connection already holding its share is
// not dealt an equal slice of every unit that frees — that is what let the
// hub that syncs most often end up with most of the pool.
//
// A connection that is not asking, or that is at its cap, is skipped rather
// than dealt a unit no run of its could use: one busy hub may fill a pool the
// others have no work for.
func (p *Pool) quota(conn string) int {
	var asking []string
	for i := range p.order {
		name := p.order[(p.cursor+i)%len(p.order)]
		if c := p.conns[name]; c.asking && p.headroom(c) > 0 {
			asking = append(asking, name)
		}
	}
	if len(asking) == 0 {
		return 0
	}
	n := 0
	for i := range p.total {
		if asking[i%len(asking)] == conn {
			n++
		}
	}
	return n
}

// Reserve takes the free units this connection's turn gives it, under its cap.
// Whatever the sync does not turn into claimed runs goes back on Close.
func (p *Pool) Reserve(conn string) *Reservation {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.conn(conn)
	c.asking = true
	// What is free, what this connection's turns entitle it to beyond what it
	// already holds, and what its cap leaves it. The cap appears here and
	// nowhere else: a run over it is not something a claim can discover.
	n := min(max(p.total-p.used, 0), max(p.quota(conn)-c.held, 0), p.headroom(c))
	p.used += n
	r := &Reservation{p: p, conn: conn, n: n, by: map[string]int{}}
	for id, cap := range p.caps {
		r.by[id] = max(min(cap-p.byHarness[id], n), 0)
	}
	return r
}

// Free is how many units no run and no sync holds.
func (p *Pool) Free() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.total - p.used
}

// Held is how many runs this connection holds, and the owner's cap on it.
// `yad status` shows both: a cap nobody can see is one nobody can trust.
func (p *Pool) Held(conn string) (held, cap int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.conns[conn]
	if !ok {
		return 0, 0
	}
	return c.held, c.cap
}

// Reservation is capacity held for one sync.
type Reservation struct {
	p    *Pool
	conn string
	n    int
	by   map[string]int
}

// emptyReservation advertises nothing, for a runner that has no executor to
// hand runs to.
func emptyReservation() *Reservation { return &Reservation{p: nil, by: map[string]int{}} }

// Free is what the sync declares: the hub offers no more than this.
func (r *Reservation) Free() v1.Capacity {
	c := v1.Capacity{Total: r.n}
	if len(r.by) > 0 {
		c.ByHarness = map[string]int{}
		for id, n := range r.by {
			c.ByHarness[id] = n
		}
	}
	return c
}

// Take turns one reserved unit into a held run of harness h. It fails when the
// reservation is spent or h is at its cap. release returns the unit to the
// pool and is safe to call more than once.
func (r *Reservation) Take(h string) (release func(), ok bool) {
	if r.p == nil {
		return nil, false
	}
	p := r.p
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.n == 0 {
		return nil, false
	}
	if cap, capped := p.caps[h]; capped && p.byHarness[h] >= cap {
		return nil, false
	}
	r.n--
	if _, capped := r.by[h]; capped {
		r.by[h]--
	}
	p.byHarness[h]++
	c := p.conn(r.conn)
	c.held++
	// The next free unit is offered past this connection, so a hub that keeps
	// taking does not keep taking first.
	p.cursor = (c.idx + 1) % len(p.order)
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.used--
			p.byHarness[h]--
			c.held--
		})
	}, true
}

// putBack undoes a Take whose claim was never recorded: the unit returns to
// this reservation, so the next offer in the same sync can have it.
func (r *Reservation) putBack(h string) {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.n++
	if _, capped := r.by[h]; capped {
		r.by[h]++
	}
	r.p.byHarness[h]--
	r.p.conn(r.conn).held--
}

// Close returns every unit the sync did not take.
func (r *Reservation) Close() {
	if r.p == nil {
		return
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	if r.n > 0 {
		// It was offered more than it had work for; the rest of its turn goes
		// to the other connections until its next sync asks again.
		r.p.conn(r.conn).asking = false
	}
	r.p.used -= r.n
	r.n = 0
}
