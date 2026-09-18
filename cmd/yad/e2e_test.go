package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/hub"
	hubstore "github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hubapiclient"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// The end-to-end tests: one machine that is both hub and runner, driven only
// through the yad commands an operator types — token create, connect,
// admin-token create, daemon start, hub submit, hub watch. The hub is yad
// hub's own handler on loopback, as `yad hub serve` serves it; the harness is
// the real Claude adapter driving a fake claude, which is this test binary
// replaying a recorded stream. Nothing spends a token or leaves the machine.

func TestMain(m *testing.M) {
	if os.Getenv(fakeClaudeFixture) != "" {
		fakeClaude()
		return
	}
	os.Exit(m.Run())
}

// The fake claude's settings travel in its environment, which supervise passes
// through: its scrub removes YAD_* and CLAUDE_CODE_*, not these.
const (
	fakeClaudeFixture = "E2E_CLAUDE_FIXTURE"
	// fakeClaudeGate, when set, is a file the fake waits for before its
	// result: the run is mid-flight, with events out, until the test says.
	fakeClaudeGate = "E2E_CLAUDE_GATE"
	// fakeClaudeAtGate is a file the fake creates when it reaches the gate.
	fakeClaudeAtGate = "E2E_CLAUDE_AT_GATE"
)

// e2eFixture is a recorded haiku turn: one Read of a small file, then its
// contents as the answer.
const (
	e2eFixture = "../../internal/adapter/claude/testdata/claude-2.1.276/tool.jsonl"
	e2eAnswer  = "hello from a small file"
)

// fakeClaude answers --version as claude does, and otherwise plays the
// fixture the way claude writes it: nothing past the echo of the instruction
// until the instruction has arrived, the session id rewritten to the one YAD
// chose, and an exit only once stdin closes after the result.
func fakeClaude() {
	args := os.Args[1:]
	if len(args) == 1 && args[0] == "--version" {
		os.Stdout.WriteString("2.1.276 (Claude Code)\n")
		return
	}
	session := ""
	for i, a := range args {
		if (a == "--session-id" || a == "--resume") && i+1 < len(args) {
			session = args[i+1]
		}
	}
	users, eof := make(chan struct{}, 16), make(chan struct{})
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(nil, 64<<20)
		for sc.Scan() {
			var f struct{ Type string }
			if json.Unmarshal(sc.Bytes(), &f) == nil && f.Type == "user" {
				users <- struct{}{}
			}
		}
		close(eof)
	}()
	raw, err := os.ReadFile(os.Getenv(fakeClaudeFixture))
	if err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(2)
	}
	body := string(raw)
	const recorded = "6d684e55-cf7f-4a32-860a-8c92bde94cb0"
	if session != "" {
		body = strings.ReplaceAll(body, recorded, session)
	}
	out := bufio.NewWriter(os.Stdout)
	for _, line := range strings.SplitAfter(body, "\n") {
		var f struct {
			Type     string `json:"type"`
			IsReplay bool   `json:"isReplay"`
		}
		_ = json.Unmarshal([]byte(line), &f)
		switch {
		case f.Type == "user" && f.IsReplay:
			out.Flush()
			select {
			case <-users:
			case <-time.After(30 * time.Second):
				os.Exit(1)
			}
		case f.Type == "result":
			out.Flush()
			awaitGate()
		}
		out.WriteString(line)
	}
	out.Flush()
	select {
	case <-eof:
	case <-time.After(30 * time.Second): // never outlive a broken test by much
	}
}

func awaitGate() {
	gate := os.Getenv(fakeClaudeGate)
	if gate == "" {
		return
	}
	if at := os.Getenv(fakeClaudeAtGate); at != "" {
		os.WriteFile(at, nil, 0o600)
	}
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(gate); err == nil {
			return
		}
	}
	os.Exit(1)
}

// machine is one profile holding both sides: the hub's database and admin
// token, and the runner's config, credential and state.
type machine struct {
	t       *testing.T
	p       *profile
	hub     *hub.Hub
	hubDB   *hubstore.Store
	service string // the service API's hub URL, always reachable
	gate    string
	atGate  string
	// cut, while set, picks the runner's requests that are dropped without an
	// answer, as a lost network drops them.
	cut     atomic.Pointer[func(*http.Request) bool]
	dropped atomic.Int64
	// skew moves the hub's clock ahead, so a lease lapses without a sleep.
	skew atomic.Int64
}

func newMachine(t *testing.T) *machine {
	t.Helper()
	m := &machine{t: t, p: newProfile(t)}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := filepath.Abs(e2eFixture)
	if err != nil {
		t.Fatal(err)
	}
	gates := t.TempDir()
	m.gate, m.atGate = filepath.Join(gates, "open"), filepath.Join(gates, "reached")
	t.Setenv("YAD_CLAUDE_PATH", self)
	t.Setenv(fakeClaudeFixture, fixture)
	// A race-enabled child otherwise sleeps a second at exit.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	// A fake left waiting at its gate by a failed test is let go.
	t.Cleanup(m.open)

	m.hubDB, err = hubstore.Open(context.Background(), filepath.Join(m.p.data, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.hubDB.Close() })
	m.hub = hub.New(hub.Options{Store: m.hubDB, Now: func() time.Time { return time.Now().Add(time.Duration(m.skew.Load())) }})

	service := httptest.NewServer(m.hub)
	t.Cleanup(service.Close)
	runnerSide := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cut := m.cut.Load(); cut != nil && (*cut)(r) {
			m.dropped.Add(1)
			// No answer at all: the connection is gone, which a runner
			// sees as a network error, not as a status it could act on.
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		m.hub.ServeHTTP(w, r)
	}))
	t.Cleanup(runnerSide.Close)
	m.service = service.URL

	m.ok("hub", "admin-token", "create")
	code, tok, errs := m.p.yad("", "hub", "token", "create")
	if code != 0 {
		t.Fatalf("hub token create: exit %d: %s", code, errs)
	}
	m.ok2(tok, "connect", runnerSide.URL+hub.BasePath, "--token", "-", "--name", "home")
	return m
}

// ok runs a yad command that must succeed and returns its stdout.
func (m *machine) ok(args ...string) string { return m.ok2("", args...) }

func (m *machine) ok2(in string, args ...string) string {
	m.t.Helper()
	code, out, errs := m.p.yad(in, args...)
	if code != 0 {
		m.t.Fatalf("yad %s: exit %d: %s", strings.Join(args, " "), code, errs)
	}
	return out
}

func (m *machine) gated() {
	m.t.Setenv(fakeClaudeGate, m.gate)
	m.t.Setenv(fakeClaudeAtGate, m.atGate)
}

func (m *machine) open() {
	if err := os.WriteFile(m.gate, nil, 0o600); err != nil {
		m.t.Error(err)
	}
}

// submit queues the run the fixture answers, under a chosen id.
func (m *machine) submit(runID string) {
	m.t.Helper()
	out := m.ok("hub", "submit", "--hub", m.service, "--harness", "claude", "--model", "haiku",
		"--run-id", runID, "Use the Read tool to read note.txt, then reply with its contents only.")
	if strings.TrimSpace(out) != runID {
		m.t.Fatalf("submit printed %q, want the run id alone", out)
	}
}

// daemon is `yad daemon start --foreground` until stop.
type daemon struct {
	stop func()
	done chan int
	out  *syncBuffer
	once sync.Once
}

func (m *machine) daemon() *daemon {
	m.t.Helper()
	m.t.Setenv("YAD_CONFIG_DIR", m.p.config)
	m.t.Setenv("YAD_DATA_DIR", m.p.data)
	ctx, cancel := context.WithCancel(context.Background())
	d := &daemon{stop: cancel, done: make(chan int, 1), out: &syncBuffer{}}
	go func() { d.done <- run(ctx, []string{"daemon", "start", "--foreground"}, d.out, d.out) }()
	m.t.Cleanup(func() { d.halt(m.t) })
	return d
}

// halt stops the daemon and waits for it, as Ctrl-C would.
func (d *daemon) halt(t *testing.T) {
	t.Helper()
	d.once.Do(func() {
		d.stop()
		select {
		case code := <-d.done:
			if code != 0 {
				t.Errorf("daemon exited %d:\n%s", code, d.out.String())
			}
		case <-time.After(30 * time.Second):
			t.Errorf("daemon did not stop:\n%s", d.out.String())
		}
	})
}

// watch is `yad hub watch` to the end: its exit code, what it printed and
// what it complained of.
func (m *machine) watch(runID string) (int, string, string) {
	m.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var out, errs syncBuffer
	code := run(ctx, []string{"hub", "watch", "--hub", m.service, runID}, &out, &errs)
	return code, out.String(), errs.String()
}

func (m *machine) client() *hubapiclient.Client {
	m.t.Helper()
	tok, err := os.ReadFile(filepath.Join(m.p.config, "hub-admin-token"))
	if err != nil {
		m.t.Fatal(err)
	}
	c, err := hubapiclient.New(m.service, strings.TrimSpace(string(tok)))
	if err != nil {
		m.t.Fatal(err)
	}
	return c
}

// hubEvents is every event the hub holds for a run, in order.
func (m *machine) hubEvents(runID string) []v1.Event {
	m.t.Helper()
	var all []v1.Event
	c, after := m.client(), int64(0)
	for {
		page, err := c.Events(context.Background(), runID, after, 0)
		if err != nil {
			m.t.Fatal(err)
		}
		all = append(all, page.Events...)
		if len(page.Events) == 0 {
			return all
		}
		after = page.NextAfter
	}
}

// contiguous fails unless the events are numbered 1..n with no gap.
func contiguous(t *testing.T, evs []v1.Event) {
	t.Helper()
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d: the hub's stream has a gap", i, ev.Seq)
		}
	}
}

func (m *machine) runnerStore() *store.Store {
	m.t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(m.p.data, "state.db"))
	if err != nil {
		m.t.Fatal(err)
	}
	m.t.Cleanup(func() { s.Close() })
	return s
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting until %s", what)
}

func (m *machine) waitAtGate() {
	m.t.Helper()
	eventually(m.t, "the harness is mid-run", func() bool {
		_, err := os.Stat(m.atGate)
		return err == nil
	})
}

func localRun(t *testing.T, s *store.Store, runID string) db.Run {
	t.Helper()
	r, err := s.GetRun(context.Background(), db.GetRunParams{Connection: "home", ID: runID})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The whole path, once: a run submitted to the hub is claimed by the runner,
// driven through the Claude adapter, streamed, and reported, and the person
// watching sees the tool call, the answer and the result.
func TestE2ERunSucceeds(t *testing.T) {
	m := newMachine(t)
	m.submit("e2e-1")
	d := m.daemon()

	code, out, errs := m.watch("e2e-1")
	if code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
	for _, want := range []string{"→ Read", e2eAnswer, "── succeeded in"} {
		if !strings.Contains(out, want) {
			t.Errorf("watch output lacks %q:\n%s", want, out)
		}
	}

	run, err := m.client().Run(context.Background(), "e2e-1")
	if err != nil {
		t.Fatal(err)
	}
	evs := m.hubEvents("e2e-1")
	contiguous(t, evs)
	if run.Result == nil || run.Result.State != v1.RunSucceeded || run.Result.FinalText != e2eAnswer {
		t.Fatalf("result %+v", run.Result)
	}
	if run.Result.LastSeq != int64(len(evs)) || run.Result.Metrics.ToolCalls != 1 {
		t.Errorf("result says last_seq %d and %d tool calls; the hub holds %d events", run.Result.LastSeq, run.Result.Metrics.ToolCalls, len(evs))
	}
	if len(run.Result.Usage.ByModel) == 0 {
		t.Error("result carries no usage")
	}

	// Nothing is left owed on the runner.
	s := m.runnerStore()
	eventually(t, "the runner owes nothing", func() bool {
		o, err1 := s.OutboxDepth(context.Background())
		sp, err2 := s.SpoolDepth(context.Background())
		return err1 == nil && err2 == nil && o == 0 && sp == 0
	})
	if r := localRun(t, s, "e2e-1"); r.State != string(v1.RunSucceeded) {
		t.Errorf("runner recorded %s", r.State)
	}
}

// The network drops mid-run and comes back: the run finishes while the hub is
// out of reach, and every event and the result still land, in order, once it
// is back — from the runner's spool and outbox, with nothing lost.
func TestE2ENetworkDropMidRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		cut  func(*http.Request) bool
	}{
		// Syncs, events and the result all fail; the result waits behind
		// its events.
		{"everything", func(*http.Request) bool { return true }},
		// Only the result is lost, after every event is in: the outbox
		// carries it.
		{"the result", func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/result") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t)
			m.gated()
			m.submit("e2e-drop")
			d := m.daemon()
			m.waitAtGate()

			c := m.client()
			eventually(t, "the hub has the run's first events", func() bool {
				return len(m.hubEvents("e2e-drop")) > 0
			})
			m.cut.Store(&tc.cut)
			m.open() // the harness finishes, with the hub out of reach

			s := m.runnerStore()
			eventually(t, "the runner has recorded the result", func() bool {
				o, err := s.OutboxDepth(context.Background())
				return err == nil && o == 1 && localRun(t, s, "e2e-drop").State == string(v1.RunSucceeded)
			})
			eventually(t, "the runner has tried the hub and failed", func() bool { return m.dropped.Load() >= 2 })
			if run, err := c.Run(context.Background(), "e2e-drop"); err != nil || run.State.Terminal() {
				t.Fatalf("the hub has the result while it is out of reach: %+v %v", run, err)
			}

			m.cut.Store(nil)
			code, out, errs := m.watch("e2e-drop")
			if code != 0 {
				t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
			}
			if !strings.Contains(out, e2eAnswer) || !strings.Contains(out, "── succeeded in") {
				t.Errorf("watch output:\n%s", out)
			}
			run, err := c.Run(context.Background(), "e2e-drop")
			if err != nil {
				t.Fatal(err)
			}
			evs := m.hubEvents("e2e-drop")
			contiguous(t, evs)
			if run.Result == nil || run.Result.State != v1.RunSucceeded || run.Result.LastSeq != int64(len(evs)) {
				t.Fatalf("result %+v with %d events on the hub", run.Result, len(evs))
			}
			eventually(t, "the runner owes nothing", func() bool {
				o, err1 := s.OutboxDepth(context.Background())
				sp, err2 := s.SpoolDepth(context.Background())
				return err1 == nil && err2 == nil && o == 0 && sp == 0
			})
		})
	}
}

// The runner restarts mid-run. Nothing resumes a run yet (epic E3), so the new
// process gives it up: it stops listing it, the hub marks it lost when its
// lease lapses, and the events it streamed before the restart are still
// delivered — a run ends reported lost, never silently rerun or left hanging.
func TestE2ERunnerRestartMidRun(t *testing.T) {
	m := newMachine(t)
	// Uploads are cut from the start, so every event the run streams is
	// still in the spool when the runner stops: the delivery after the
	// restart is the only way any of them reaches the hub.
	events := func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/events") }
	m.cut.Store(&events)
	m.gated()
	m.submit("e2e-restart")
	d := m.daemon()
	m.waitAtGate()
	eventually(t, "the runner has tried to upload events", func() bool { return m.dropped.Load() >= 1 })
	d.halt(t)

	s := m.runnerStore()
	spooled, err := s.UnackedEvents(context.Background(), db.UnackedEventsParams{Connection: "home", RunID: "e2e-restart", Limit: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(spooled) == 0 || len(m.hubEvents("e2e-restart")) != 0 {
		t.Fatalf("%d events spooled at the restart and %d on the hub; the test needs them all held back", len(spooled), len(m.hubEvents("e2e-restart")))
	}
	if r := localRun(t, s, "e2e-restart"); v1.RunState(r.State).IsTerminal() {
		t.Fatalf("a run stopped with its runner is %s; it must stay held for the next start", r.State)
	}

	d = m.daemon()
	eventually(t, "the new process gives the run up", func() bool {
		return localRun(t, s, "e2e-restart").State == string(v1.RunLost)
	})
	// The lease lapses; the hub's sweep decides.
	m.skew.Store(int64(10 * time.Minute))
	if err := m.hub.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	code, out, errs := m.watch("e2e-restart")
	if code == 0 || !strings.Contains(out, "── lost") || !strings.Contains(errs, "ended lost") {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}

	// Only now, with the run already lost, do the events get through: the
	// hub takes them from the run's holder after it ends (decision 0023),
	// every one, exactly as the runner held them. A refusal would empty
	// the spool too, and leave the hub short.
	m.cut.Store(nil)
	eventually(t, "the events streamed before the restart are on the hub", func() bool {
		return len(m.hubEvents("e2e-restart")) >= len(spooled)
	})
	evs := m.hubEvents("e2e-restart")
	if len(evs) != len(spooled) {
		t.Fatalf("the hub holds %d events; the runner spooled %d", len(evs), len(spooled))
	}
	for i, row := range spooled {
		var want v1.Event
		if err := json.Unmarshal([]byte(row.Body), &want); err != nil {
			t.Fatal(err)
		}
		got, _ := json.Marshal(evs[i])
		exp, _ := json.Marshal(want)
		if string(got) != string(exp) {
			t.Errorf("event %d on the hub:\n%s\nspooled:\n%s", row.Seq, got, exp)
		}
	}
	run, err := m.client().Run(context.Background(), "e2e-restart")
	if err != nil {
		t.Fatal(err)
	}
	if run.State != hubapi.RunState(v1.RunLost) || run.Result != nil {
		t.Errorf("hub run %+v", run)
	}
	// Lost is the hub's verdict; the runner owes it no result.
	if o, err := s.OutboxDepth(context.Background()); err != nil || o != 0 {
		t.Errorf("outbox %d %v: a lost run owes no result", o, err)
	}
}
