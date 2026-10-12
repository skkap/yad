package capability

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/supervise"
)

// Another Claude in the home a run uses is a warning, never a reason a
// harness cannot take a run (decision 0077). YAD-170's runs hung before their
// first word in a home where a `claude --bg` daemon ran, and so far that is
// one machine: a proxy for a cause nobody has pinned down is not enough to
// refuse work on, and it is enough to tell the owner where to look.
//
// It is found from what Claude itself writes in its home, never from another
// process's environment or arguments, either of which may hold a token:
//
//   - sessions/<pid>.json, one for every live Claude session in the home —
//     interactive and SDK alike, and background ones by Claude's code — of
//     which only pid, procStart and kind are read;
//   - daemon/roster.json, the `claude --bg` supervisor's, of which only
//     supervisorPid is read.
//
// The .key files beside them are never opened. Each pid is then checked
// against the process table: alive, and either started when the file says
// or running a command named claude, so a file left by a process that
// crashed, its pid since taken by something else, is not counted. A Claude
// that yad started — this process's descendant, or any yad's — is a run, and
// not counted either. Measured on Claude 2.1.296; a version that keeps no
// such files is a home where nothing is found, which is what absence means.

// sharedRecheck is how long what was found in a home is kept: the daemon
// re-probes every 15 seconds, and each look starts ps.
const sharedRecheck = time.Minute

// registryFileCap bounds each file read. A session record is half a KiB.
const registryFileCap = 64 << 10

// Process is one row of the process table: its pid, its parent's, the name
// of its command and, where asked for, when it started as ps prints it.
type Process struct {
	PID, PPID int
	Comm      string
	Start     string
}

// ProcessesForTests, when set, is the whole process table, start times
// included, in place of ps: nothing in the suite looks at the machine's own
// processes, where the owner's Claude sessions are.
var ProcessesForTests func(ctx context.Context) ([]Process, error)

func listProcs(ctx context.Context) ([]Process, error) {
	if ProcessesForTests != nil {
		return ProcessesForTests(ctx)
	}
	return psList(ctx)
}

func procStarts(ctx context.Context, table map[int]Process, pids []int) map[int]string {
	starts := map[int]string{}
	if ProcessesForTests != nil {
		for _, p := range pids {
			starts[p] = table[p].Start
		}
		return starts
	}
	// A start time that cannot be read leaves the command's name to decide.
	if got, err := psStarts(ctx, pids); err == nil {
		starts = got
	}
	return starts
}

// sharedHome is what was found in one home.
type sharedHome struct {
	pids   []int
	daemon bool
}

var (
	sharedMu    sync.Mutex
	sharedAsked = map[string]sharedAnswer{}
	sharedNow   = time.Now
)

type sharedAnswer struct {
	found sharedHome
	err   error
	until time.Time
}

// SharedHomes warns on Claude when another Claude that yad did not start runs
// from a home its runs use: its own default home when the owner gave it no
// accounts, and each account's home otherwise. detail adds the pids, for
// `yad doctor` on the machine; the document reaches every hub and carries a
// count alone. The error is a process table that could not be read, for
// doctor to say so; the document says nothing then.
func SharedHomes(ctx context.Context, found []harness.Detected, cfg config.Config, accounts []account.Account, command func(args ...string) string, detail bool) error {
	var failed error
	for i, d := range found {
		if d.ID != "claude" || !d.Ready() {
			continue
		}
		if len(cfg.Harness[d.ID].Accounts) == 0 {
			home := account.DefaultHome(d.ID)
			if home == "" {
				continue
			}
			s, err := lookShared(ctx, home)
			if err != nil {
				failed = err
				continue
			}
			if len(s.pids) == 0 {
				continue
			}
			found[i].Warnings = append(found[i].Warnings, fmt.Sprintf("%s run%s from the home this runner's Claude runs use%s — runs there have been seen to hang before their first word and end harness_silent (YAD-170). If they do, give yad an account, a home nobody else uses: `%s`",
				others(s, detail), plural(len(s.pids), "s", ""), daemonNote(s), command("account", "add", d.ID, "<label>")))
			continue
		}
		for _, a := range account.For(accounts, d.ID) {
			s, err := lookShared(ctx, a.Home)
			if err != nil {
				failed = err
				continue
			}
			if len(s.pids) == 0 {
				continue
			}
			found[i].Warnings = append(found[i].Warnings, fmt.Sprintf("account %q: %s run%s from its home%s — runs there have been seen to hang before their first word and end harness_silent (YAD-170); an account's home is for yad's runs alone — quit them, or give them a home of their own",
				a.Label, others(s, detail), plural(len(s.pids), "s", ""), daemonNote(s)))
		}
	}
	return failed
}

func others(s sharedHome, detail bool) string {
	n := len(s.pids)
	what := fmt.Sprintf("%d other Claude process%s that yad did not start", n, plural(n, "", "es"))
	if detail {
		ids := make([]string, n)
		for i, p := range s.pids {
			ids[i] = strconv.Itoa(p)
		}
		what += " (pid " + strings.Join(ids, ", ") + ")"
	}
	return what
}

func daemonNote(s sharedHome) string {
	if s.daemon {
		return ", `claude --bg` among them"
	}
	return ""
}

// plural is one when n is 1 and many otherwise.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// lookShared is otherClaudes for one home, kept for sharedRecheck.
func lookShared(ctx context.Context, home string) (sharedHome, error) {
	sharedMu.Lock()
	a, ok := sharedAsked[home]
	sharedMu.Unlock()
	if ok && sharedNow().Before(a.until) {
		return a.found, a.err
	}
	s, err := otherClaudes(ctx, home, os.Getpid())
	if ctx.Err() != nil {
		// The caller stopped asking; that says nothing about the home.
		return sharedHome{}, nil
	}
	sharedMu.Lock()
	sharedAsked[home] = sharedAnswer{found: s, err: err, until: sharedNow().Add(sharedRecheck)}
	sharedMu.Unlock()
	return s, err
}

// ForgetSharedHomes drops what is kept, for a test that changes the table.
func ForgetSharedHomes() {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	clear(sharedAsked)
}

// sessionFile is a session record's name: the pid, and nothing else.
var sessionFile = regexp.MustCompile(`^([0-9]+)\.json$`)

// registered is one pid a home's files name.
type registered struct {
	pid    int
	start  string
	daemon bool
}

// otherClaudes is every live Claude the files in home name that is not a
// descendant of self or of a yad. No file, or a directory that cannot be read,
// is nothing found; only a process table that cannot be read is an error, and
// only when there was something to check against it.
func otherClaudes(ctx context.Context, home string, self int) (sharedHome, error) {
	regs := readRegistry(home)
	if len(regs) == 0 {
		return sharedHome{}, nil
	}
	table, err := listProcs(ctx)
	if err != nil {
		return sharedHome{}, err
	}
	byPID := make(map[int]Process, len(table))
	for _, p := range table {
		byPID[p.PID] = p
	}
	var pids []int
	for _, r := range regs {
		if _, ok := byPID[r.pid]; ok {
			pids = append(pids, r.pid)
		}
	}
	// Start times only for the pids that are alive, which are few: asked of
	// the whole table, the time would have to be parsed out of the middle of
	// a line, in words ps may print differently by locale.
	var starts map[int]string
	if len(pids) > 0 {
		starts = procStarts(ctx, byPID, pids)
	}
	var out sharedHome
	seen := map[int]bool{}
	for _, r := range regs {
		p, ok := byPID[r.pid]
		if !ok || seen[r.pid] || yadStarted(byPID, p, self) {
			continue
		}
		sameStart := r.start != "" && sameTime(starts[r.pid], r.start)
		if !sameStart && filepath.Base(p.Comm) != "claude" {
			continue
		}
		seen[r.pid] = true
		out.pids = append(out.pids, r.pid)
		out.daemon = out.daemon || r.daemon
	}
	slices.Sort(out.pids)
	return out, nil
}

// yadStarted says whether p descends from self or from a process named yad:
// a run, whichever profile's daemon started it.
func yadStarted(byPID map[int]Process, p Process, self int) bool {
	// Bounded: a table read across two moments can hold a cycle.
	for range 64 {
		if p.PPID == self || p.PPID <= 1 {
			return p.PPID == self
		}
		parent, ok := byPID[p.PPID]
		if !ok {
			return false
		}
		if filepath.Base(parent.Comm) == "yad" {
			return true
		}
		p = parent
	}
	return false
}

// sameTime compares two renderings of a start time, ignoring the padding ps
// puts around it.
func sameTime(a, b string) bool {
	return a != "" && strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
}

// readRegistry is every pid home's session records and --bg roster name.
func readRegistry(home string) []registered {
	var out []registered
	entries, _ := os.ReadDir(filepath.Join(home, "sessions"))
	for _, e := range entries {
		m := sessionFile.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		pid, err := strconv.Atoi(m[1])
		if err != nil || pid <= 1 {
			continue
		}
		var rec struct {
			PID       int    `json:"pid"`
			ProcStart string `json:"procStart"`
			Kind      string `json:"kind"`
		}
		if !readJSON(filepath.Join(home, "sessions", e.Name()), &rec) || (rec.PID != 0 && rec.PID != pid) {
			continue
		}
		out = append(out, registered{pid: pid, start: rec.ProcStart, daemon: rec.Kind == "background"})
	}
	var roster struct {
		SupervisorPID int `json:"supervisorPid"`
	}
	if readJSON(filepath.Join(home, "daemon", "roster.json"), &roster) && roster.SupervisorPID > 1 {
		out = append(out, registered{pid: roster.SupervisorPID, daemon: true})
	}
	return out
}

// readJSON decodes at most registryFileCap bytes of a regular file into v.
func readJSON(path string, v any) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(f, registryFileCap))
	return err == nil && json.Unmarshal(b, v) == nil
}

// psList is every process's pid, parent and command name. The name is the
// last column, since on macOS it is a path and may hold spaces.
func psList(ctx context.Context) ([]Process, error) {
	out, err := supervise.PS(ctx, nil, "-A", "-o", "pid=,ppid=,comm=")
	if err != nil {
		return nil, err
	}
	return parseProcs(out)
}

func parseProcs(out []byte) ([]Process, error) {
	var procs []Process
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		procs = append(procs, Process{PID: pid, PPID: ppid, Comm: strings.Join(f[2:], " ")})
	}
	if len(procs) == 0 {
		return nil, errors.New("ps listed no processes")
	}
	return procs, nil
}

// claudeClock is the environment Claude's procStart is printed in: UTC, in
// the C locale's words. Measured on 2.1.296, in Japan: procStart read
// "Sun Oct 11 09:18:10 2026" for a session ps showed as starting at 18:18.
var claudeClock = []string{"TZ=UTC", "LC_ALL=C"}

// psStarts is each pid's start time as ps prints it — the rendering Claude
// records as procStart. The time is the last column, whatever its words.
func psStarts(ctx context.Context, pids []int) (map[int]string, error) {
	ids := make([]string, len(pids))
	for i, p := range pids {
		ids[i] = strconv.Itoa(p)
	}
	out, err := supervise.PS(ctx, claudeClock, "-o", "pid=,lstart=", "-p", strings.Join(ids, ","))
	if err != nil {
		return nil, err
	}
	return parseStarts(out), nil
}

func parseStarts(out []byte) map[int]string {
	starts := map[int]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		pidText, rest, ok := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if !ok {
			continue
		}
		if pid, err := strconv.Atoi(pidText); err == nil {
			starts[pid] = strings.TrimSpace(rest)
		}
	}
	return starts
}
