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
// still holding counted work — and returns left with Stopped set on each one
// that was no longer running after (decision 0086). It is the owner's choice
// ([supervise] stop_left_running); off, nothing calls it.
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
func (p *Process) StopLeftovers(ctx context.Context, left []Leftover, grace time.Duration, helpers ...string) []Leftover {
	out := make([]Leftover, len(left))
	copy(out, left)
	if len(left) == 0 {
		return out
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
	table, err := procs(ctx)
	read := time.Now()
	if err != nil {
		return out
	}
	// Only what was signalled is waited for, and killed: a counted process
	// whose group yad may not signal would hold the stop for the whole grace
	// for nothing, and one that SIGTERM never reached gets no SIGKILL first.
	signalledOnly := func() []Leftover {
		var s []Leftover
		for _, l := range left {
			if signalled[l.PID] {
				s = append(s, l)
			}
		}
		return s
	}
	signal(left, table, read, syscall.SIGTERM)
	if table, read, err = waitGone(ctx, signalledOnly(), table, read, grace); err != nil {
		return out
	}
	if len(running(table, read, signalledOnly())) > 0 {
		signal(signalledOnly(), table, read, syscall.SIGKILL)
		if table, read, err = waitGone(ctx, signalledOnly(), table, read, killSettle); err != nil {
			return out
		}
	}
	alive := running(table, read, left)
	for i, l := range out {
		_, still := alive[l.PID]
		out[i].Stopped = signalled[l.PID] && !still
	}
	return out
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
