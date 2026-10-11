//go:build unix

package supervise

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// psTimeout bounds one ps; psOutputCap what it may print. A census runs once
// per turn end, and a process table is a few hundred KiB at most.
const (
	psTimeout   = 5 * time.Second
	psOutputCap = 8 << 20
)

// psPath is the ps the process table is read with: the system's, where it is,
// so a ps earlier on PATH is not what decides.
func psPath() (string, error) {
	for _, p := range []string{"/bin/ps", "/usr/bin/ps"} {
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	return exec.LookPath("ps")
}

// PS runs the system's ps with args, through Run like any other child, and
// returns what it printed. env is added to its environment. An empty answer
// is the failure: ps -p exits 1 when one of the pids has gone, and still
// prints the rest.
func PS(ctx context.Context, env []string, args ...string) ([]byte, error) {
	path, err := psPath()
	if err != nil {
		return nil, errors.New("there is no ps on this machine to list its processes with")
	}
	ctx, cancel := context.WithTimeout(ctx, psTimeout)
	defer cancel()
	c, err := Run(ctx, Spec{Path: path, Args: args, Env: env, NoTTY: true}, psOutputCap)
	if err != nil {
		return nil, fmt.Errorf("ps would not start: %w", err)
	}
	if c.TimedOut {
		return nil, fmt.Errorf("ps did not answer within %s", psTimeout)
	}
	if len(bytes.TrimSpace(c.Stdout)) == 0 && c.Err != nil {
		return nil, fmt.Errorf("ps failed: %w", c.Err)
	}
	return c.Stdout, nil
}

// Proc is one row of the process table as a census reads it.
type Proc struct {
	PID, PPID, PGID int
	// Zombie is a process that has exited and not been reaped: listed, and
	// running nothing.
	Zombie bool
	// Age is how long it has been running.
	Age time.Duration
	// Name is the kernel's name for its command — the executable's file
	// name, cut to 15 or 16 bytes — and never anything from its argv, which
	// a command is free to fill with a token.
	Name string
}

// nameColumn is the ps column that holds the kernel's name for a command.
// macOS's comm is argv[0], which is the command line's and may be anything;
// its ucomm is the executable's name. Linux's comm is the kernel's task name.
func nameColumn() string {
	if runtime.GOOS == "darwin" {
		return "ucomm="
	}
	return "comm="
}

// procs reads the whole process table. A variable, so a test can hand the
// census a table of its own.
var procs = func(ctx context.Context) ([]Proc, error) {
	out, err := PS(ctx, []string{"LC_ALL=C"}, "-A", "-o", "pid=,ppid=,pgid=,stat=,etime=,"+nameColumn())
	if err != nil {
		return nil, err
	}
	return parseProcTable(out)
}

// parseProcTable reads ps's pid, ppid, pgid, stat, etime and name columns.
// The name is last, since it may hold spaces.
func parseProcTable(out []byte) ([]Proc, error) {
	var table []Proc
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 6 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		pgid, err3 := strconv.Atoi(f[2])
		age, ok := parseEtime(f[4])
		if err1 != nil || err2 != nil || err3 != nil || !ok {
			continue
		}
		table = append(table, Proc{PID: pid, PPID: ppid, PGID: pgid, Zombie: strings.HasPrefix(f[3], "Z"),
			Age: age, Name: strings.Join(f[5:], " ")})
	}
	if len(table) == 0 {
		return nil, errors.New("ps listed no processes")
	}
	return table, nil
}

// parseEtime reads ps's elapsed time, [[dd-]hh:]mm:ss.
func parseEtime(s string) (time.Duration, bool) {
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil || n < 0 {
			return 0, false
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var secs int
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, false
		}
		secs = secs*60 + n
	}
	return time.Duration(days*86400+secs) * time.Second, true
}

// Leftover is one process a harness's tool work still had running when the
// census was taken.
type Leftover struct {
	// PID is for the runner's own log and for tests. It never leaves the
	// machine: a run's event names a leftover by Name alone.
	PID  int
	Name string
	Age  time.Duration
}

// shells are the names of the shells a harness runs a tool's command in. A
// shell waiting on a command is a wrapper around the work, not the work:
// Claude Code's run_in_background runs `zsh -c 'sleep 600'` as zsh with sleep
// beneath it, and that is one thing left running, not two.
var shells = []string{"sh", "bash", "zsh", "dash", "ksh", "mksh", "fish", "tcsh", "csh"}

// ErrNotMeasured is a census that could not be taken: the leader had already
// exited, so its tool work was no longer recognisably its own, or the process
// table could not be read.
var ErrNotMeasured = errors.New("the census could not be taken")

// Census lists what the leader's tool work still has running: every live
// process in a group led by a direct child of the leader, other than the
// leader's own group, and every descendant of one of those wherever it
// went — minus helpers, the processes the harness itself keeps beside its
// tools, named as ps names them, and minus a shell that is only waiting on
// a command beneath it.
//
// It is taken while the leader is alive, before its input is closed, because
// a harness stops its own tool processes on the way out and a leader that
// has exited has handed its children to init, where nothing marks them as
// its. Measured (decision 0080): Claude Code 2.1.296, Codex 0.160.1 and
// OpenCode 1.18.33 each start a tool's command in a process group of its
// own, so yad's group signals never reach one; the leader's own group holds
// its helpers — Claude's stdio MCP servers — and is left out. What a
// command backgrounds and then leaves, its shell exiting, is handed to init
// at once and is beyond any census without a subreaper; it is not counted.
//
// The leader cannot be confused with another process: it is this process's
// child, and its pid is not free for reuse until it is reaped. One that
// exited around the listing — gone before it, a zombie in it, or reaped
// after it — makes the census ErrNotMeasured rather than a count of nothing.
func (p *Process) Census(ctx context.Context, helpers ...string) ([]Leftover, error) {
	if p.exited(0) {
		return nil, ErrNotMeasured
	}
	table, err := procs(ctx)
	if err != nil || p.exited(0) {
		return nil, ErrNotMeasured
	}
	left, ok := census(table, p.Pid(), helpers)
	if !ok {
		return nil, ErrNotMeasured
	}
	return left, nil
}

// census is Census over a table already read. ok is false when the leader
// is not in the table alive.
func census(table []Proc, leader int, helpers []string) ([]Leftover, bool) {
	byPID := make(map[int]Proc, len(table))
	children := map[int][]int{}
	for _, p := range table {
		byPID[p.PID] = p
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	if self, ok := byPID[leader]; !ok || self.Zombie {
		return nil, false
	}
	tools := map[int]bool{}
	for _, p := range table {
		if p.PPID == leader && p.PGID == p.PID && p.PID != leader {
			tools[p.PID] = true
		}
	}
	in := map[int]bool{}
	var queue []int
	for _, p := range table {
		if tools[p.PGID] && p.PID != leader {
			in[p.PID] = true
			queue = append(queue, p.PID)
		}
	}
	// A descendant that left its group with setsid is still the work's. The
	// table is one moment's, so it is a tree, and in bounds the walk anyway.
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if !in[c] && c != leader {
				in[c] = true
				queue = append(queue, c)
			}
		}
	}
	waiting := map[int]bool{} // has a live child in the census
	for pid := range in {
		if p := byPID[pid]; !p.Zombie && in[p.PPID] {
			waiting[p.PPID] = true
		}
	}
	var left []Leftover
	for pid := range in {
		p := byPID[pid]
		if p.Zombie || helper(p.Name, helpers) || waiting[pid] && slices.Contains(shells, p.Name) {
			continue
		}
		left = append(left, Leftover{PID: pid, Name: p.Name, Age: p.Age})
	}
	slices.SortFunc(left, func(a, b Leftover) int { return a.PID - b.PID })
	return left, true
}

// helper says whether name is one of helpers. The kernel cuts a name to 15
// bytes on Linux and 16 on macOS, so a name of that length matches a helper
// it begins: codex-code-mode-host is listed as codex-code-mode-.
func helper(name string, helpers []string) bool {
	for _, h := range helpers {
		if name == h || len(name) >= 15 && strings.HasPrefix(h, name) {
			return true
		}
	}
	return false
}
