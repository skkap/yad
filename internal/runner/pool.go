package runner

import (
	"sync"

	v1 "github.com/skkap/yad/protocol/v1"
)

// Pool is the runner's capacity: one shared count, with the owner's optional
// cap per harness. Every run held — claimed and waiting to start, or with the
// executor — holds one unit until its release is called.
//
// Capacity is taken before a sync asks for work, never after an offer arrives
// (decision 0005, Multica's lesson): a run the hub hands over must never wait
// for room, and two connections syncing at once must not both promise the
// same unit.
type Pool struct {
	mu        sync.Mutex
	total     int
	caps      map[string]int
	used      int // held runs plus units reserved by syncs in flight
	byHarness map[string]int
}

// NewPool builds a pool from the capacity the runner advertises.
func NewPool(c v1.Capacity) *Pool {
	caps := map[string]int{}
	for id, n := range c.ByHarness {
		caps[id] = n
	}
	return &Pool{total: c.Total, caps: caps, byHarness: map[string]int{}}
}

// Reserve takes every free unit for one sync. Whatever the sync does not turn
// into claimed runs goes back on Close.
func (p *Pool) Reserve() *Reservation {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := max(p.total-p.used, 0)
	p.used += n
	r := &Reservation{p: p, n: n, by: map[string]int{}}
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

// Reservation is capacity held for one sync.
type Reservation struct {
	p  *Pool
	n  int
	by map[string]int
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
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.used--
			p.byHarness[h]--
		})
	}, true
}

// Take takes one unit outside a sync's reservation, for a run that is already
// this runner's: a parked run coming back holds nothing while it waits, and
// has to take its capacity again before it can run. It fails when the pool is
// full or the harness is at its cap, and the caller leaves the run parked.
func (p *Pool) Take(h string) (release func(), ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used >= p.total {
		return nil, false
	}
	if cap, capped := p.caps[h]; capped && p.byHarness[h] >= cap {
		return nil, false
	}
	p.used++
	p.byHarness[h]++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.used--
			p.byHarness[h]--
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
}

// Close returns every unit the sync did not take.
func (r *Reservation) Close() {
	if r.p == nil {
		return
	}
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	r.p.used -= r.n
	r.n = 0
}
