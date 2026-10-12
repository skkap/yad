//go:build unix

package supervise

import (
	"context"
	"syscall"
	"time"
)

// startSlack is how far apart two tables may put one process's start and
// still mean the same process. ps gives elapsed time in whole seconds, read
// at a slightly different moment each time, so the same process's reckoned
// start moves by up to a second either way; a pid reused within two seconds
// of the process it replaces would need the whole pid space to wrap in that
// time.
const startSlack = 2 * time.Second

// stopPoll is how often StopLeftovers rereads the process table while it
// waits for what it signalled to go; killSettle how long it waits after
// SIGKILL, which takes effect at once but is seen only once the process is
// gone from the table or a zombie in it.
const (
	stopPoll   = 100 * time.Millisecond
	killSettle = time.Second
)

// StopLeftovers stops what a census of this process counted, by the ladder's
// signals — SIGTERM to each group, grace to go, then SIGKILL to the groups
// still holding counted work (decision 0086). It is the owner's choice
// ([supervise] stop_left_running); off, nothing calls it.
//
// SIGTERM is sent before it returns, so it reaches the work before the
// caller closes the harness's input and the harness starts stopping its
// tools itself. The rest of the ladder runs in the background, beside the
// harness's own exit rather than after it; Stopping.Wait returns left with
// Stopped set on each one yad signalled that was no longer running after.
//
// It signals a group only when a process table read just before that signal
// shows it holding a live process the census counted: the same pid, started
// when the census reckoned (startSlack). A group with a live member cannot
// have been given away, so its id still names the work's group and not one
// an unrelated process made after the work's had emptied. And never a group
// that is the daemon's own, the leader's, or one holding a named helper —
// Codex's code-mode host sits in a group of its own beside the work — nor
// pgid 0 or 1. What remains is the moment between the table and kill(2), in
// which every member would have to exit and the id go to a new group leader;
// that needs the pid space to wrap within milliseconds.
//
// It is called where the census is taken, the harness still alive and its
// input open, so the work is the harness's descendants still. A table that
// cannot be read stops nothing more: nothing is signalled on a guess.
func (p *Process) StopLeftovers(ctx context.Context, left []Leftover, grace time.Duration, helpers ...string) *Stopping {
	s := &Stopping{done: make(chan struct{}), left: make([]Leftover, len(left))}
	copy(s.left, left)
	if len(left) == 0 {
		close(s.done)
		return s
	}
	// A cleanup bounded by grace and killSettle, which a cancel of the turn
	// must not cut short half-way: the groups it signalled deserve the KILL.
	ctx = context.WithoutCancel(ctx)
	self := syscall.Getpgrp()
	signalled := map[int]bool{} // counted pids whose group was signalled
	signal := func(work []Leftover, table []Proc, read time.Time, sig syscall.Signal) {
		for g, pids := range stopTargets(table, read, work, p.Pid(), self, helpers) {
			if syscall.Kill(-g, sig) == nil {
				for _, pid := range pids {
					signalled[pid] = true
				}
			}
		}
	}
	// Only what was signalled is waited for, and killed: a counted process
	// whose group yad may not signal would hold the stop for the whole grace
	// for nothing, and one that SIGTERM never reached gets no SIGKILL first.
	signalledOnly := func() []Leftover {
		var w []Leftover
		for _, l := range left {
			if signalled[l.PID] {
				w = append(w, l)
			}
		}
		return w
	}
	table, err := procs(ctx)
	read := time.Now()
	if err != nil {
		close(s.done)
		return s
	}
	signal(left, table, read, syscall.SIGTERM)
	go func() {
		defer close(s.done)
		table, read, err := waitGone(ctx, signalledOnly(), table, read, grace)
		if err != nil {
			return
		}
		if len(running(table, read, signalledOnly())) > 0 {
			signal(signalledOnly(), table, read, syscall.SIGKILL)
			if table, read, err = waitGone(ctx, signalledOnly(), table, read, killSettle); err != nil {
				return
			}
		}
		alive := running(table, read, left)
		for i, l := range s.left {
			_, still := alive[l.PID]
			s.left[i].Stopped = signalled[l.PID] && !still
		}
	}()
	return s
}

// Stopping is a StopLeftovers whose SIGTERM has been sent and whose ladder
// may still be running.
type Stopping struct {
	done chan struct{}
	left []Leftover
}

// Wait blocks until the ladder is over — at most the grace, killSettle and
// the reads between — and returns the leftovers, each saying whether yad
// stopped it.
func (s *Stopping) Wait() []Leftover {
	<-s.done
	return s.left
}

// waitGone rereads the table until no counted process is running in it or
// within has passed, and returns the last table read.
func waitGone(ctx context.Context, left []Leftover, table []Proc, read time.Time, within time.Duration) ([]Proc, time.Time, error) {
	deadline := time.Now().Add(within)
	for len(running(table, read, left)) > 0 && time.Now().Before(deadline) {
		time.Sleep(min(stopPoll, time.Until(deadline)))
		var err error
		if table, err = procs(ctx); err != nil {
			return nil, time.Time{}, err
		}
		read = time.Now()
	}
	return table, read, nil
}

// running is the counted processes table, read at read, shows alive: present,
// not a zombie, and started when the census reckoned. A zombie has exited
// and only waits on its parent to be reaped — under a harness that does not
// reap promptly, the work it stopped stays one for a while.
func running(table []Proc, read time.Time, left []Leftover) map[int]Proc {
	counted := make(map[int]Leftover, len(left))
	for _, l := range left {
		counted[l.PID] = l
	}
	alive := map[int]Proc{}
	for _, p := range table {
		l, ok := counted[p.PID]
		if !ok || p.Zombie {
			continue
		}
		if d := read.Add(-p.Age).Sub(l.Start); d > startSlack || d < -startSlack {
			continue
		}
		alive[p.PID] = p
	}
	return alive
}

// stopTargets is the groups StopLeftovers may signal over a table read at
// read, each with the counted processes running in it: every group holding
// one, less pgid 0 and 1, the daemon's own group self, the leader's, and any
// group holding a live process of a named helper.
func stopTargets(table []Proc, read time.Time, left []Leftover, leader, self int, helpers []string) map[int][]int {
	refused := map[int]bool{0: true, 1: true, self: true, leader: true}
	for _, p := range table {
		if !p.Zombie && (p.PID == leader || helper(p.Name, helpers)) {
			refused[p.PGID] = true
		}
	}
	groups := map[int][]int{}
	for pid, p := range running(table, read, left) {
		if !refused[p.PGID] {
			groups[p.PGID] = append(groups[p.PGID], pid)
		}
	}
	return groups
}
