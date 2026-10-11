//go:build unix

package supervise

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The census's children are separate from the supervisor's own, so that the
// two sets of modes do not share a switch: init runs before TestMain.
func init() {
	if mode := os.Getenv("SUPERVISE_CENSUS_CHILD"); mode != "" {
		censusChild(mode)
		os.Exit(0)
	}
}

// censusChild plays a harness at the end of its turn, with the shapes of tool
// work the census must count and the helpers it must not. It prints the pids
// it expects counted on one line and the ones it expects left out on the
// next, then waits as a harness waits for its input to close.
func censusChild(mode string) {
	start := func(child string, setsid bool) *exec.Cmd {
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "SUPERVISE_CENSUS_CHILD="+child)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: setsid}
		out, err := c.StdoutPipe()
		if err != nil {
			os.Exit(9)
		}
		if err := c.Start(); err != nil {
			os.Exit(9)
		}
		// Each child says when it is in place, and what it started.
		line, err := bufio.NewReader(out).ReadString('\n')
		if err != nil {
			os.Exit(9)
		}
		c.Args = append(c.Args, strings.Fields(line)...)
		return c
	}
	switch mode {
	case "harness":
		// A tool command in a session of its own, as Claude Code starts one.
		tool := start("sleep", true)
		// One that started a descendant which left for a session of its own:
		// a setsid'd grandchild of a live leader.
		escaper := start("escaper", true)
		// One whose subshell exited, leaving an orphan in the tool's group.
		orphaner := start("orphaner", true)
		// A helper in the harness's own group, as Claude's MCP servers are.
		mcp := start("sleep", false)
		counted := []string{strconv.Itoa(tool.Process.Pid), strconv.Itoa(escaper.Process.Pid), escaper.Args[1],
			strconv.Itoa(orphaner.Process.Pid), orphaner.Args[1]}
		fmt.Println(strings.Join(counted, " "))
		fmt.Println(mcp.Process.Pid)
		time.Sleep(time.Hour)
	case "escaper":
		gc := start("sleep", true)
		fmt.Println(gc.Process.Pid)
		time.Sleep(time.Hour)
	case "orphaner":
		// The middle process starts a sleeper in this group and exits, as
		// `(sleep 600 &)` does in a shell that goes on running.
		mid := start("leave", false)
		mid.Wait()
		fmt.Println(mid.Args[1])
		time.Sleep(time.Hour)
	case "leave":
		gc := start("sleep", false)
		fmt.Println(gc.Process.Pid)
	case "sleep":
		fmt.Println()
		time.Sleep(time.Hour)
	}
}

func TestCensusCountsToolWorkAndNotHelpers(t *testing.T) {
	p, err := Start(context.Background(), Spec{Path: os.Args[0],
		Env: []string{"SUPERVISE_CENSUS_CHILD=harness", "GORACE=atexit_sleep_ms=0"}})
	if err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(p.Stdout())
	var lines [2][]int
	for i := range lines {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range strings.Fields(line) {
			n, _ := strconv.Atoi(f)
			lines[i] = append(lines[i], n)
		}
	}
	counted, helpers := lines[0], lines[1]
	// What left the group is beyond the group kill; the test ends it.
	t.Cleanup(func() {
		p.Stop(Ladder{})
		for _, pid := range counted {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	left, err := p.Census(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, l := range left {
		got = append(got, l.PID)
		if l.Name == "" || l.Age < 0 {
			t.Errorf("leftover %+v has no name or a negative age", l)
		}
	}
	slices.Sort(counted)
	if !slices.Equal(got, counted) {
		t.Errorf("census counted pids %v; want %v (tool, escaper and its grandchild, orphaner and its orphan) and not the helper %v", got, counted, helpers)
	}
}

func TestCensusOfAnExitedLeaderIsNotMeasured(t *testing.T) {
	p := spawn(t, "echo")
	p.Wait()
	if left, err := p.Census(context.Background()); !errors.Is(err, ErrNotMeasured) {
		t.Errorf("census of an exited leader = %v, %v; want ErrNotMeasured, not a count of nothing", left, err)
	}
}

func TestCensusOfAnUnreadableTableIsNotMeasured(t *testing.T) {
	old := procs
	t.Cleanup(func() { procs = old })
	procs = func(context.Context) ([]Proc, error) { return nil, errors.New("no ps") }
	p := spawn(t, "polite")
	if left, err := p.Census(context.Background()); !errors.Is(err, ErrNotMeasured) {
		t.Errorf("census without a table = %v, %v; want ErrNotMeasured", left, err)
	}
}

func TestCensusRules(t *testing.T) {
	const h = 100 // the harness
	harness := Proc{PID: h, PPID: 1, PGID: h, Name: "2.1.296"}
	for _, tc := range []struct {
		name    string
		table   []Proc
		helpers []string
		want    []string // names, in pid order
		ok      bool
	}{
		{"nothing left", []Proc{harness}, nil, nil, true},
		{"the leader is gone", []Proc{{PID: 200, PPID: 1, PGID: 200, Name: "sleep"}}, nil, nil, false},
		{"the leader is a zombie", []Proc{{PID: h, PPID: 1, PGID: h, Zombie: true, Name: "claude"}}, nil, nil, false},
		{"a shell waiting on its command is one thing left", []Proc{harness,
			{PID: 200, PPID: h, PGID: 200, Name: "zsh"},
			{PID: 201, PPID: 200, PGID: 200, Name: "sleep"},
		}, nil, []string{"sleep"}, true},
		{"a shell running on its own is the work", []Proc{harness,
			{PID: 200, PPID: h, PGID: 200, Name: "bash"},
		}, nil, []string{"bash"}, true},
		{"a shell whose command is a zombie runs alone", []Proc{harness,
			{PID: 200, PPID: h, PGID: 200, Name: "bash"},
			{PID: 201, PPID: 200, PGID: 200, Zombie: true, Name: "sleep"},
		}, nil, []string{"bash"}, true},
		{"a command exec'd in place of its shell", []Proc{harness,
			{PID: 200, PPID: h, PGID: 200, Name: "sleep"},
		}, nil, []string{"sleep"}, true},
		{"the harness's own group is its helpers'", []Proc{harness,
			{PID: 200, PPID: h, PGID: h, Name: "python3.12"},
			{PID: 201, PPID: h, PGID: h, Name: "caffeinate"},
		}, nil, nil, true},
		{"a named helper in a group of its own, cut to the kernel's width", []Proc{harness,
			{PID: 200, PPID: h, PGID: 200, Name: "codex-code-mode-"},
			{PID: 201, PPID: h, PGID: 201, Name: "codex-code-mode"},
			{PID: 202, PPID: h, PGID: 202, Name: "sleep"},
		}, []string{"codex-code-mode-host"}, []string{"sleep"}, true},
		{"a short name is not a helper it begins", []Proc{harness,
			{PID: 200, PPID: h, PGID: 200, Name: "codex"},
		}, []string{"codex-code-mode-host"}, []string{"codex"}, true},
		{"a group led by a grandchild is not the harness's", []Proc{harness,
			{PID: 200, PPID: h, PGID: h, Name: "node"},
			{PID: 201, PPID: 200, PGID: 201, Name: "esbuild"},
		}, nil, nil, true},
		{"a process elsewhere is not counted", []Proc{harness,
			{PID: 300, PPID: 1, PGID: 300, Name: "sleep"},
		}, nil, nil, true},
		{"an orphan in a tool's group and a descendant that left it", []Proc{harness,
			{PID: 200, PPID: h, PGID: 200, Name: "zsh"},
			{PID: 201, PPID: 1, PGID: 200, Name: "sleep"},
			{PID: 202, PPID: 200, PGID: 202, Name: "node"},
			{PID: 203, PPID: 202, PGID: 202, Name: "esbuild"},
		}, nil, []string{"sleep", "node", "esbuild"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, ok := census(tc.table, h, tc.helpers)
			var names []string
			for _, l := range left {
				names = append(names, l.Name)
			}
			if ok != tc.ok || !slices.Equal(names, tc.want) {
				t.Errorf("census = %v, %v; want %v, %v", names, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestParseProcTable(t *testing.T) {
	// macOS, with ucomm names that hold a space and one cut to 16 bytes, and
	// Linux procps, whose etime pads to the right width.
	for _, tc := range []struct {
		name, out string
		want      []Proc
	}{
		{"darwin", "    1     0     1 Ss   14-00:56:36 launchd\n" +
			"25790 25709 25790 S+        00:18 2.1.296\n" +
			"28033 28028 28028 S         00:02 sleep\n" +
			"  612     1   612 S    02:03:04 Google Chrome He\n" +
			"  700   612   612 Z       01:00 codex-code-mode-\n" +
			"nonsense\n", []Proc{
			{PID: 1, PPID: 0, PGID: 1, Age: 14*24*time.Hour + 56*time.Minute + 36*time.Second, Name: "launchd"},
			{PID: 25790, PPID: 25709, PGID: 25790, Age: 18 * time.Second, Name: "2.1.296"},
			{PID: 28033, PPID: 28028, PGID: 28028, Age: 2 * time.Second, Name: "sleep"},
			{PID: 612, PPID: 1, PGID: 612, Age: 2*time.Hour + 3*time.Minute + 4*time.Second, Name: "Google Chrome He"},
			{PID: 700, PPID: 612, PGID: 612, Zombie: true, Age: time.Minute, Name: "codex-code-mode-"},
		}},
		{"linux", "      1       0       1 Ss       1-02:03:04 systemd\n" +
			"   4242    4200    4242 S             00:07 sleep\n", []Proc{
			{PID: 1, PPID: 0, PGID: 1, Age: 26*time.Hour + 3*time.Minute + 4*time.Second, Name: "systemd"},
			{PID: 4242, PPID: 4200, PGID: 4242, Age: 7 * time.Second, Name: "sleep"},
		}},
	} {
		got, err := parseProcTable([]byte(tc.out))
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("%s: parseProcTable = %+v, %v; want %+v", tc.name, got, err, tc.want)
		}
	}
	if _, err := parseProcTable([]byte("\n")); err == nil {
		t.Error("an empty table parsed")
	}
}

func TestParseEtime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"00:00": 0, "01:02": 62 * time.Second, "1:02:03": time.Hour + 2*time.Minute + 3*time.Second,
		"2-00:00:01": 48*time.Hour + time.Second,
	} {
		if got, ok := parseEtime(in); !ok || got != want {
			t.Errorf("parseEtime(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "5", "a:b", "1:2:3:4", "x-01:02", "-1:00"} {
		if _, ok := parseEtime(bad); ok {
			t.Errorf("parseEtime(%q) parsed", bad)
		}
	}
}
