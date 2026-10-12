//go:build unix

package supervise

import (
	"context"
	"errors"
	"maps"
	"slices"
	"syscall"
	"testing"
	"time"
)

// What the census counted is stopped and said to be, wherever it went —
// a tool's session, a setsid'd grandchild, an orphan in a tool's group —
// while the harness and the helper in its own group go on (decision 0086).
func TestStopLeftoversStopsWhatTheCensusCounted(t *testing.T) {
	p, counted, helpers := startCensusChild(t, "harness")
	left, err := p.Census(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != len(counted) {
		t.Fatalf("census counted %d; the child started %d", len(left), len(counted))
	}
	got := p.StopLeftovers(context.Background(), left, 5*time.Second)
	for _, l := range got {
		if !l.Stopped {
			t.Errorf("%s[%d] is not said to be stopped", l.Name, l.PID)
		}
	}
	table, err := procs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if alive := running(table, time.Now(), left); len(alive) > 0 {
		t.Errorf("still running after the stop: %v", slices.Collect(maps.Keys(alive)))
	}
	if p.exited(0) {
		t.Error("the harness itself was stopped")
	}
	for _, pid := range helpers {
		if syscall.Kill(pid, 0) != nil || zombie(table, pid) {
			t.Errorf("the helper %d in the harness's own group was stopped", pid)
		}
	}
}

// Work that ignores SIGTERM is killed once the grace is over, and not
// before.
func TestStopLeftoversKillsWhatIgnoresTerm(t *testing.T) {
	p, _, _ := startCensusChild(t, "deaf-harness")
	left, err := p.Census(context.Background())
	if err != nil || len(left) != 1 {
		t.Fatalf("census = %+v, %v; want the one deaf tool", left, err)
	}
	const grace = 300 * time.Millisecond
	began := time.Now()
	got := p.StopLeftovers(context.Background(), left, grace)
	if took := time.Since(began); took < grace {
		t.Errorf("stopped in %s, inside the %s grace: SIGTERM cannot have ended a process that ignores it", took, grace)
	}
	if !got[0].Stopped {
		t.Errorf("%+v is not said to be stopped", got[0])
	}
	table, err := procs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(running(table, time.Now(), left)) > 0 {
		t.Error("the deaf tool survived SIGKILL")
	}
}

// A table that cannot be read signals nothing: a group is told from one
// reused by another process only by what a table shows in it.
func TestStopLeftoversWithoutATableSignalsNothing(t *testing.T) {
	harness, stand := spawn(t, "sleep"), spawn(t, "sleep")
	old := procs
	t.Cleanup(func() { procs = old })
	procs = func(context.Context) ([]Proc, error) { return nil, errors.New("no ps") }
	left := []Leftover{{PID: stand.Pid(), PGID: stand.Pid(), Name: "sleep", Start: time.Now()}}
	got := harness.StopLeftovers(context.Background(), left, 50*time.Millisecond)
	if got[0].Stopped {
		t.Error("said to be stopped with no table read")
	}
	if stand.exited(100 * time.Millisecond) {
		t.Error("a group was signalled with no table to say it was the work's")
	}
}

// Work in a group yad may not signal — here the daemon's own — is left
// alone, said not to be stopped, and does not hold the stop for the grace.
func TestStopLeftoversLeavesARefusedGroupAtOnce(t *testing.T) {
	harness := spawn(t, "sleep")
	self := syscall.Getpgrp()
	const pid = 1 << 30 // no process: a signal sent to it would say so
	start := time.Now().Add(-10 * time.Second)
	old := procs
	t.Cleanup(func() { procs = old })
	procs = func(context.Context) ([]Proc, error) {
		return []Proc{{PID: pid, PPID: harness.Pid(), PGID: self, Age: time.Since(start).Truncate(time.Second), Name: "sleep"}}, nil
	}
	began := time.Now()
	got := harness.StopLeftovers(context.Background(), []Leftover{{PID: pid, PGID: self, Name: "sleep", Start: start}}, 5*time.Second)
	if took := time.Since(began); took > time.Second {
		t.Errorf("took %s over work it would not signal", took)
	}
	if got[0].Stopped {
		t.Error("work in the daemon's own group is said to be stopped")
	}
}

func TestStopTargetsRules(t *testing.T) {
	const h, self = 100, 50
	read := time.Now()
	harness := Proc{PID: h, PPID: 1, PGID: h, Age: time.Minute, Name: "2.1.296"}
	sleep := Leftover{PID: 200, PGID: 200, Name: "sleep", Start: read.Add(-10 * time.Second)}
	work := Proc{PID: 200, PPID: h, PGID: 200, Age: 10 * time.Second, Name: "sleep"}
	with := func(p Proc, f func(*Proc)) Proc { f(&p); return p }
	for _, tc := range []struct {
		name    string
		table   []Proc
		left    []Leftover
		helpers []string
		want    []int
	}{
		{"the counted work's group", []Proc{harness, work}, []Leftover{sleep}, nil, []int{200}},
		{"started a second later, as ps rounds it", []Proc{harness, with(work, func(p *Proc) { p.Age = 9 * time.Second })},
			[]Leftover{sleep}, nil, []int{200}},
		{"its pid is now a later process's", []Proc{harness, with(work, func(p *Proc) { p.Age = time.Second })},
			[]Leftover{sleep}, nil, nil},
		{"it has exited, waiting to be reaped", []Proc{harness, with(work, func(p *Proc) { p.Zombie = true })},
			[]Leftover{sleep}, nil, nil},
		{"it is gone", []Proc{harness}, []Leftover{sleep}, nil, nil},
		{"its group is the daemon's", []Proc{harness, with(work, func(p *Proc) { p.PGID = self })},
			[]Leftover{sleep}, nil, nil},
		{"its group is the harness's", []Proc{harness, with(work, func(p *Proc) { p.PGID = h })},
			[]Leftover{sleep}, nil, nil},
		{"its group is init's", []Proc{harness, with(work, func(p *Proc) { p.PGID = 1 })},
			[]Leftover{sleep}, nil, nil},
		{"its group holds a helper", []Proc{harness, work, {PID: 201, PPID: 200, PGID: 200, Age: time.Second, Name: "codex-code-mode-"}},
			[]Leftover{sleep}, []string{"codex-code-mode-host"}, nil},
		{"a helper that has exited holds nothing", []Proc{harness, work, {PID: 201, PPID: 200, PGID: 200, Zombie: true, Name: "codex-code-mode-"}},
			[]Leftover{sleep}, []string{"codex-code-mode-host"}, []int{200}},
		{"it moved to a group of its own", []Proc{harness, with(work, func(p *Proc) { p.PGID = 300 })},
			[]Leftover{sleep}, nil, []int{300}},
		{"two counted in one group", []Proc{harness, work, {PID: 201, PPID: 200, PGID: 200, Age: 10 * time.Second, Name: "node"}},
			[]Leftover{sleep, {PID: 201, PGID: 200, Name: "node", Start: read.Add(-10 * time.Second)}}, nil, []int{200}},
		{"a process nobody counted is no reason", []Proc{harness, {PID: 400, PPID: 1, PGID: 400, Age: 10 * time.Second, Name: "sleep"}},
			[]Leftover{sleep}, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := slices.Sorted(maps.Keys(stopTargets(tc.table, read, tc.left, h, self, tc.helpers)))
			if !slices.Equal(got, tc.want) {
				t.Errorf("groups = %v; want %v", got, tc.want)
			}
		})
	}
}

func zombie(table []Proc, pid int) bool {
	for _, p := range table {
		if p.PID == pid {
			return p.Zombie
		}
	}
	return false
}
