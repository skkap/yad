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
// Capacity goes to connections in turn (0005), and so does each harness's cap:
// the units of each are dealt one at a time around the ring of connections,
// starting from that one's cursor, which gives every connection a share. What
// is free is then dealt the same way to the connections short of their share,
// and that is what a sync may take — so a freed unit goes to the connection
// whose turn it is, not to whichever syncs first, and one that ends a run and
// syncs again at once cannot take it back from one still waiting. A cursor
// stays where it is until its units fill and then moves on once, so every sync
// of one fill is judged against the same deal — one hub's takes cannot re-deal
// the turns of a hub that has not synced yet — while the extra unit of an
// uneven division still goes round the ring from one fill to the next.
//
// A connection that leaves units unused has no work for them and passes — it
// is out of the dealing, keeping what it holds, until its own next sync asks
// again — so one busy hub still fills a pool the others have no queue for,
// while a hub that is quiet now finds its turn waiting for it when its work
// arrives. Units left over because a harness's cap gave it none are not that:
// the hub may have had nothing else to offer, so the connection still waits
// for that harness and keeps its turn at it (DEV-106). One that was given room
// for a harness and used the sync on other work passes on that harness until
// its cap next fills. A unit a turn holds back is not reserved, so it waits at
// most until that connection's next sync, an interval away.
type Pool struct {
	mu        sync.Mutex
	total     int
	caps      map[string]int
	used      int // held runs plus units reserved by syncs in flight
	byHarness map[string]int
	// reservedBy is the units of each capped harness that syncs in flight
	// have advertised and not yet taken. They are out of every other sync's
	// deal for the same reason used is: two syncs must not both promise one
	// unit of a cap, or the hub that answers second is offered a run the
	// runner then cannot take.
	reservedBy map[string]int

	// order is the ring, in the order connections joined it. whole is the
	// turn over the total capacity, harness the turn over each capped
	// harness: the two are dealt the same way, each from its own cursor.
	order   []string
	conns   map[string]*poolConn
	whole   turn
	harness map[string]*turn
}

// turn is where one deal starts. filled is whether the cursor has already
// moved on for the fill in progress: units that fill, have one put back and
// fill again within one sync are one fill, not two. A unit leaving the runs —
// a release, or a put-back unit returned on Close — starts the next one.
type turn struct {
	cursor int
	filled bool
}

// poolConn is one connection's standing in the pool.
type poolConn struct {
	name     string
	idx      int
	cap      int // 0: only the runner's own capacity bounds this connection
	held     int
	reserved int // units its syncs in flight hold and have not yet taken
	heldBy   map[string]int
	// reservedBy is reserved, per capped harness.
	reservedBy map[string]int
	// asking is whether this connection still wants what its turn would give
	// it. A sync that closes its reservation with units left over had no work
	// for them and passes until it asks again, which covers a hub with an
	// empty queue, a sync that failed before it could claim, and — through
	// Pass — a loop that is draining or has stopped. One mark, so no share is
	// ever held for a connection that cannot take it.
	asking bool
	// waits is the capped harnesses a connection that has passed was offered
	// none of, when it passed. Its leftover units prove nothing about those:
	// it stays in their deal, and in the total's for as many units as they
	// have free for it.
	waits map[string]bool
	// passed is the capped harnesses it had room for in a sync that took
	// other work instead. It is out of that harness's deal until the cap
	// next fills, or a hub whose queue starts with other harnesses would keep
	// a turn at a cap it never uses, and a hub that wants only that harness
	// would wait for it indefinitely.
	passed map[string]bool
}

// NewPool builds a pool from the capacity the runner advertises.
func NewPool(c v1.Capacity) *Pool {
	caps, harness := map[string]int{}, map[string]*turn{}
	for id, n := range c.ByHarness {
		caps[id], harness[id] = n, &turn{}
	}
	return &Pool{total: c.Total, caps: caps, byHarness: map[string]int{}, reservedBy: map[string]int{},
		conns: map[string]*poolConn{}, harness: harness}
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
	c := p.conn(conn)
	c.asking, c.waits = false, nil
}

// conn is a connection's standing, created asking on first sight: a hub
// nothing is yet known about is assumed to want work until a sync says so.
func (p *Pool) conn(name string) *poolConn {
	if c, ok := p.conns[name]; ok {
		return c
	}
	c := &poolConn{name: name, idx: len(p.order), asking: true, heldBy: map[string]int{},
		reservedBy: map[string]int{}, passed: map[string]bool{}}
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

// deal hands units out one at a time around the ring from cursor, each
// connection up to the limit stake gives it; one stake leaves out is dealt
// nothing. It returns each share, and the position after the first connection
// dealt a unit, which is where the next fill's deal starts.
func (p *Pool) deal(units, cursor int, stake func(*poolConn) (limit int, in bool)) (share map[string]int, next int) {
	share = map[string]int{}
	type seat struct {
		name  string
		limit int
	}
	var ring []seat
	for i := range p.order {
		name := p.order[(cursor+i)%len(p.order)]
		if limit, in := stake(p.conns[name]); in {
			ring = append(ring, seat{name, limit})
		}
	}
	next = cursor
	for dealt := true; units > 0 && dealt; {
		dealt = false
		for _, s := range ring {
			if units == 0 || share[s.name] >= s.limit {
				continue
			}
			if len(share) == 0 {
				next = (p.conns[s.name].idx + 1) % len(p.order)
			}
			share[s.name]++
			units--
			dealt = true
		}
	}
	return share, next
}

// split is one deal worked through: each connection's share of the units,
// its room — what of the free units it may take now — and the spare free
// units no connection in the deal is short of.
type split struct {
	share, room map[string]int
	spare       int
	next        int
}

// split deals units from t's cursor. stake says what a connection holds of
// them, the most it may be dealt, and whether it is in the deal at all; one
// that is not keeps what it holds and is dealt nothing more.
//
// The share is reckoned over all the units rather than over what happens to
// be free, so a connection already holding its share is not dealt an equal
// slice of every unit that frees — that is what let the hub that syncs most
// often end up with most of the pool. The room is the free units dealt the
// same way, each connection up to what it is short of its share: two short
// of theirs with one unit free do not both get to take it, the one whose
// turn comes first does.
func (p *Pool) split(units, free int, t turn, stake func(*poolConn) (holding, limit int, in bool)) split {
	for _, c := range p.conns {
		if holding, _, in := stake(c); !in {
			units -= holding
		}
	}
	share, next := p.deal(units, t.cursor, func(c *poolConn) (int, bool) {
		_, limit, in := stake(c)
		return limit, in
	})
	room, _ := p.deal(free, t.cursor, func(c *poolConn) (int, bool) {
		holding, _, in := stake(c)
		short := share[c.name] - holding
		return short, in && short > 0
	})
	spare := free
	for _, n := range room {
		spare -= n
	}
	return split{share: share, room: room, spare: spare, next: next}
}

// harnessSplit deals harness h's cap. A connection is in it while it asks,
// or waits for h, and has not passed on h; it may be dealt no more than its
// own cap leaves room for.
func (p *Pool) harnessSplit(h string) split {
	cap := p.caps[h]
	free := max(cap-p.byHarness[h]-p.reservedBy[h], 0)
	return p.split(cap, free, *p.harness[h], func(c *poolConn) (int, int, bool) {
		holding := c.heldBy[h] + c.reservedBy[h]
		return holding, holding + p.headroom(c), (c.asking || c.waits[h]) && !c.passed[h]
	})
}

// wholeSplit deals the total capacity. A connection that asks is dealt up to
// its cap. One that has passed but waits for a capped harness is dealt only
// what that harness has free for it: its wait must not hold back units it
// could not use, and must not lose the ones it could.
func (p *Pool) wholeSplit() split {
	harnesses := map[string]split{}
	for h := range p.caps {
		harnesses[h] = p.harnessSplit(h)
	}
	return p.split(p.total, max(p.total-p.used, 0), p.whole, func(c *poolConn) (int, int, bool) {
		holding := c.held + c.reserved
		if c.asking {
			if c.cap > 0 {
				return holding, c.cap, true
			}
			return holding, p.total, true
		}
		wanted := 0
		for h := range c.waits {
			wanted += harnesses[h].room[c.name]
		}
		wanted = min(wanted, p.headroom(c))
		return holding, holding + wanted, wanted > 0
	})
}

// Reserve takes the free units this connection's turn gives it, under its cap,
// and bounds each capped harness by this connection's turn at that cap.
// Whatever the sync does not turn into claimed runs goes back on Close.
func (p *Pool) Reserve(conn string) *Reservation {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.conn(conn)
	c.asking, c.waits = true, nil
	// The room is already bounded by what is free, by this connection's
	// share less what it holds, and by its cap, which appears nowhere else:
	// a run over it is not something a claim can discover.
	n := p.wholeSplit().room[conn]
	p.used += n
	c.reserved += n
	r := &Reservation{p: p, conn: conn, n: n, by: map[string]int{}, hold: map[string]int{}}
	for h := range p.caps {
		// Spare units of a cap nobody in its deal is short of go to whoever
		// asks, so a harness one hub alone wants is never held idle.
		s := p.harnessSplit(h)
		r.by[h] = min(s.room[conn]+s.spare, n)
		r.hold[h] = r.by[h]
		p.reservedBy[h] += r.by[h]
		c.reservedBy[h] += r.by[h]
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
	// by is this sync's turn at each capped harness, less what it has taken
	// of it: what Close reads to tell a hub with no work for a harness from
	// one its cap gave none.
	by map[string]int
	// hold is the units of each capped harness reserved for this sync — by,
	// but never more than n, since a unit taken for one harness is a unit no
	// other can have. What it holds is what it advertises and what Take may
	// turn into a run.
	hold map[string]int
	took bool // whether this sync turned any unit into a run
}

// emptyReservation advertises nothing, for a runner that has no executor to
// hand runs to.
func emptyReservation() *Reservation { return &Reservation{p: nil, by: map[string]int{}} }

// Free is what the sync declares: the hub offers no more than this.
func (r *Reservation) Free() v1.Capacity {
	c := v1.Capacity{Total: r.n}
	if len(r.hold) > 0 {
		c.ByHarness = map[string]int{}
		for id, n := range r.hold {
			c.ByHarness[id] = n
		}
	}
	return c
}

// Take turns one reserved unit into a held run of harness h. It fails when the
// reservation is spent, or h is capped and nothing of it is reserved for this
// sync. release returns the unit to the pool and is safe to call more than
// once.
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
	_, capped := p.caps[h]
	if capped && r.hold[h] <= 0 {
		return nil, false
	}
	c := p.conn(r.conn)
	r.n--
	r.took = true
	if capped {
		r.by[h]--
		r.hold[h]--
		p.reservedBy[h]--
		c.reservedBy[h]--
	}
	// One unit fewer to take means one fewer of every other harness too:
	// held past n, they would be kept from other syncs for nothing.
	for id, held := range r.hold {
		if over := held - r.n; over > 0 {
			r.hold[id] -= over
			p.reservedBy[id] -= over
			c.reservedBy[id] -= over
		}
	}
	p.byHarness[h]++
	c.reserved--
	c.held++
	c.heldBy[h]++
	p.rotate()
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.used--
			p.byHarness[h]--
			c.held--
			c.heldBy[h]--
			p.whole.filled = false
			if t, capped := p.harness[h]; capped {
				t.filled = false
			}
		})
	}, true
}

// rotate moves each cursor on once its units are full, past the connection
// that fill's deal started with, so the extra unit of an uneven division is
// the next connection's in the next fill. Only then: a cursor that moved with
// every unit taken re-dealt the fill under the syncs still to come, and with
// three hubs over four units the third was dealt nothing (DEV-78).
//
// A harness's fill also ends every pass on it: the connections that spent
// their turn on other work are dealt in again, so a hub whose queue changes
// is never shut out of a harness for longer than one fill of it.
func (p *Pool) rotate() {
	// Every next position is read before any cursor moves, so each is the
	// deal of the fill that has just completed.
	held := 0
	for _, c := range p.conns {
		held += c.held
	}
	moveWhole := held >= p.total && !p.whole.filled
	var wholeNext int
	if moveWhole {
		wholeNext = p.wholeSplit().next
	}
	moved := map[string]int{}
	for h, t := range p.harness {
		if !t.filled && p.byHarness[h] >= p.caps[h] && p.caps[h] > 0 {
			moved[h] = p.harnessSplit(h).next
		}
	}
	if moveWhole {
		p.whole.cursor, p.whole.filled = wholeNext, true
	}
	for h, next := range moved {
		p.harness[h].cursor, p.harness[h].filled = next, true
		for _, c := range p.conns {
			delete(c.passed, h)
		}
	}
}

// putBack undoes a Take whose claim was never recorded: the unit returns to
// this reservation, so the next offer in the same sync can have it.
func (r *Reservation) putBack(h string) {
	p := r.p
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.conn(r.conn)
	r.n++
	// The unit goes back to this sync's hold at h, which it came from. The
	// holds Take trimmed from other harnesses stay trimmed: another sync may
	// have been dealt those units since.
	if _, capped := p.caps[h]; capped {
		r.by[h]++
		r.hold[h]++
		p.reservedBy[h]++
		c.reservedBy[h]++
	}
	p.byHarness[h]--
	c.reserved++
	c.held--
	c.heldBy[h]--
}

// Close returns every unit the sync did not take.
func (r *Reservation) Close() {
	if r.p == nil {
		return
	}
	p := r.p
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.conn(r.conn)
	if r.n > 0 {
		// It was offered more than it had work for; the rest of its turn goes
		// to the other connections until its next sync asks again — except
		// at a harness it was given none of, which it may still want.
		c.asking, c.waits = false, nil
		for h := range p.caps {
			if r.by[h] <= 0 {
				if c.waits == nil {
					c.waits = map[string]bool{}
				}
				c.waits[h] = true
			}
		}
	}
	if r.took {
		// Room for a harness it took other work over is room its hub did not
		// want for that harness now. A sync that took nothing is not
		// evidence of that: its hub may have had an empty queue — which the
		// leftover above already covers — or never answered.
		for h := range p.caps {
			if r.by[h] > 0 {
				c.passed[h] = true
			}
		}
	}
	p.used -= r.n
	c.reserved -= r.n
	r.n = 0
	for h, held := range r.hold {
		p.reservedBy[h] -= held
		c.reservedBy[h] -= held
		r.hold[h] = 0
	}
	// A unit put back after its units filled comes free here, and the next
	// time they fill is a fill of their own.
	held := 0
	for _, c := range p.conns {
		held += c.held
	}
	if held < p.total {
		p.whole.filled = false
	}
	for h, t := range p.harness {
		if p.byHarness[h] < p.caps[h] {
			t.filled = false
		}
	}
}
