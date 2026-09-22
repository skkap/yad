package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter/codex/codextest"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/hostool"
	"github.com/skkap/yad/internal/hub"
	hubstore "github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hubapiclient"
	"github.com/skkap/yad/internal/runner"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// The end-to-end tests: one machine that is both hub and runner, driven only
// through the yad commands an operator types — token create, connect,
// admin-token create, daemon start, hub submit, hub watch. The hub is yad
// hub's own handler on loopback, as `yad hub serve` serves it; the harness is
// the real Claude adapter driving a fake claude, which is this test binary
// replaying a recorded stream. Nothing spends a token or leaves the machine.
// Each runs once per harness (e2e_harness_test.go): the same paths drive the
// Codex adapter against the fake codex.

// testSyncInterval is what the hub in these tests names and the runner accepts.
// Both sides clamp a configured interval up to five seconds in production — the
// hub so a lease outlasts the runner's backoff, the runner so no hub can spin
// the machine — and at that floor these tests spend their time asleep: 201 s of
// a 315 s suite (DEV-63). A sync every 50 ms is still one round trip per claim,
// cancel and drain, which is what they are testing.
const testSyncInterval = 50 * time.Millisecond

// probeBudget is how long a harness or host tool probe may take in these
// tests. A bound on a broken build, not a budget for a busy one.
const probeBudget = 30 * time.Second

func TestMain(m *testing.M) {
	// Before the child branches below: a child re-executed as yad is a runner
	// holding the same floor, and it syncs against this test's hub.
	hub.SyncFloorForTests, runner.SyncFloorForTests = testSyncInterval, testSyncInterval
	// Every harness and host tool these tests set up is meant to answer its
	// probe, and the fake claude and codex are this binary re-executed under
	// -race: at the shipped five seconds a loaded machine can cut one off, and
	// a harness reported as not answering is one the runner refuses work for
	// (DEV-100). A child re-executed as yad probes too, so this comes first.
	harness.VersionTimeoutForTests = probeBudget
	hostool.VersionTimeoutForTests, hostool.StatusTimeoutForTests = probeBudget, probeBudget
	if os.Getenv(childYad) != "" {
		// The test binary as yad itself, for tests that signal a runner as a
		// service manager would; the harness it spawns is still the fake.
		os.Unsetenv(childYad)
		main()
		return
	}
	// Before the fake claude: a test with both fakes set up tells them
	// apart by the name each was started under.
	if codextest.Child() {
		codextest.Main()
		return
	}
	if os.Getenv(fakeClaudeFixture) != "" {
		fakeClaude()
		return
	}
	// The binary a background start re-executes is this one: it has to be
	// yad, or a daemon that only holds its lock, for the lifecycle tests.
	switch os.Getenv(beYad) {
	case "yad":
		main()
		return
	case "wedged":
		wedgedDaemon()
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
	// fakeClaudePID, when set, is a file the fake writes its pid to, so a
	// test can prove no process outlived the run.
	fakeClaudePID = "E2E_CLAUDE_PID"
	// fakeClaudeArgs, when set, is a file the fake appends its working
	// directory and arguments to, one line per start.
	fakeClaudeArgs = "E2E_CLAUDE_ARGS"
	// fakeClaudeDeaf makes the fake ignore interrupts at its gate, as a
	// wedged harness does: only a signal stops it.
	fakeClaudeDeaf = "E2E_CLAUDE_DEAF"
	// fakeClaudeRead, when set, names a file in the fake's working directory
	// whose contents replace the recorded answer, as if the recorded Read had
	// read it there: the harness sees what the workdir holds.
	fakeClaudeRead = "E2E_CLAUDE_READ"
	// fakeClaudeTranscripts, when set, is a directory where the fake keeps a
	// conversation per session id, as claude keeps its transcripts: each
	// instruction is appended, a --resume with no transcript is refused as
	// claude refuses it, and the answer names the instructions before it —
	// which is how a test sees that a run had its session's context.
	fakeClaudeTranscripts = "E2E_CLAUDE_TRANSCRIPTS"
	// fakeClaudeGH, when set, is a file the fake writes what `gh --version`
	// printed to, found the way a harness's own shell command finds gh: by
	// name, on the PATH the runner gave it.
	fakeClaudeGH = "E2E_CLAUDE_GH"
	// childYad makes the test binary run as yad.
	childYad = "E2E_YAD_MAIN"
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
	if len(args) == 2 && args[0] == "auth" {
		fakeClaudeAuth(args[1])
		return
	}
	session, resume := "", false
	for i, a := range args {
		if (a == "--session-id" || a == "--resume") && i+1 < len(args) {
			session, resume = args[i+1], a == "--resume"
		}
	}
	if f := os.Getenv(fakeClaudePID); f != "" {
		os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	if f := os.Getenv(fakeClaudeGH); f != "" {
		out, err := exec.Command("gh", "--version").CombinedOutput()
		if err != nil {
			out = append(out, []byte("\n"+err.Error())...)
		}
		os.WriteFile(f, out, 0o600)
	}
	if f := os.Getenv(fakeClaudeArgs); f != "" {
		wd, _ := os.Getwd()
		if log, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			log.WriteString(wd + " " + strings.Join(args, " ") + "\n")
			log.Close()
		}
	}
	// Checked before any input is read, as claude checks: a resume with no
	// transcript fails at once (testdata/…/resume-missing.jsonl) — a result,
	// and an exit without waiting for input to close.
	var earlier []byte
	if dir := os.Getenv(fakeClaudeTranscripts); dir != "" {
		var err error
		earlier, err = os.ReadFile(filepath.Join(dir, session))
		if resume && err != nil {
			os.Stdout.WriteString(`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["No conversation found with session ID: ` + session + `"],"session_id":` + strconv.Quote(session) + `}` + "\n")
			os.Exit(1)
		}
	}
	users, eof := make(chan struct{}, 16), make(chan struct{})
	interrupts := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(nil, 64<<20)
		for sc.Scan() {
			var f struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
				Message   struct {
					Content string `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(sc.Bytes(), &f) != nil {
				continue
			}
			switch f.Type {
			case "user":
				remember(session, f.Message.Content)
				users <- struct{}{}
			case "control_request":
				interrupts <- f.RequestID
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
	if name := os.Getenv(fakeClaudeRead); name != "" {
		note, err := os.ReadFile(name)
		if err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(2)
		}
		quoted, _ := json.Marshal(strings.TrimSpace(string(note)))
		body = strings.ReplaceAll(body, e2eAnswer, string(quoted[1:len(quoted)-1]))
	}
	const recorded = "6d684e55-cf7f-4a32-860a-8c92bde94cb0"
	if session != "" {
		body = strings.ReplaceAll(body, recorded, session)
	}
	if os.Getenv(fakeClaudeTranscripts) != "" {
		answer := "earlier: nothing"
		if len(earlier) > 0 {
			answer = "earlier: " + strings.Join(strings.Split(strings.TrimSpace(string(earlier)), "\n"), " | ")
		}
		q, _ := json.Marshal(answer)
		body = strings.ReplaceAll(body, e2eAnswer, string(q[1:len(q)-1]))
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
			if id, ok := awaitGate(interrupts); ok {
				// As the recorded interrupt stream has it: the request is
				// acknowledged, then the turn ends in an aborted result, and
				// claude exits 1 once its input closes.
				out.WriteString(`{"type":"control_response","response":{"subtype":"success","request_id":` + strconv.Quote(id) + `,"response":{"still_queued":[]}}}` + "\n")
				out.WriteString(`{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_streaming","session_id":` + strconv.Quote(session) + `}` + "\n")
				out.Flush()
				select {
				case <-eof:
				case <-time.After(30 * time.Second):
				}
				os.Exit(1)
			}
		}
		out.WriteString(line)
	}
	out.Flush()
	select {
	case <-eof:
	case <-time.After(30 * time.Second): // never outlive a broken test by much
	}
}

// remember appends an instruction to the session's transcript, when the fake
// keeps transcripts.
func remember(session, text string) {
	dir := os.Getenv(fakeClaudeTranscripts)
	if dir == "" || session == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, session), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(strings.ReplaceAll(text, "\n", " ") + "\n")
}

// awaitGate holds the turn at its gate until the test opens it, or until an
// interrupt arrives, whose request id it returns.
func awaitGate(interrupts <-chan string) (string, bool) {
	gate := os.Getenv(fakeClaudeGate)
	if gate == "" {
		return "", false
	}
	if at := os.Getenv(fakeClaudeAtGate); at != "" {
		os.WriteFile(at, nil, 0o600)
	}
	deaf := os.Getenv(fakeClaudeDeaf) != ""
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		select {
		case id := <-interrupts:
			if !deaf {
				return id, true
			}
		default:
		}
		if _, err := os.Stat(gate); err == nil {
			return "", false
		}
	}
	os.Exit(1)
	return "", false
}

// machine is one profile holding both sides: the hub's database and admin
// token, and the runner's config, credential and state.
type machine struct {
	t       *testing.T
	h       *e2eHarness
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

func newMachine(t *testing.T, h *e2eHarness) *machine {
	t.Helper()
	m := &machine{t: t, h: h, p: newProfile(t)}
	gates := t.TempDir()
	m.gate, m.atGate = filepath.Join(gates, "open"), filepath.Join(gates, "reached")
	h.install(t)
	// A race-enabled child otherwise sleeps a second at exit.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	// A fake left waiting at its gate by a failed test is let go.
	t.Cleanup(m.open)

	var err error
	m.hubDB, err = hubstore.Open(context.Background(), filepath.Join(m.p.data, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.hubDB.Close() })
	// The shortest interval this hub may name, so a cancel reaches the runner
	// within a poll rather than within seconds.
	m.hub = hub.New(hub.Options{Store: m.hubDB, SyncInterval: testSyncInterval,
		Now: func() time.Time { return time.Now().Add(time.Duration(m.skew.Load())) }})

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
	m.t.Setenv(m.h.gate, m.gate)
	m.t.Setenv(m.h.atGate, m.atGate)
}

func (m *machine) open() {
	if err := os.WriteFile(m.gate, nil, 0o600); err != nil {
		m.t.Error(err)
	}
}

// submit queues the run the fixture answers, under a chosen id.
func (m *machine) submit(runID string) {
	m.t.Helper()
	out := m.ok(m.submitArgs("--run-id", runID, m.h.instruction)...)
	if strings.TrimSpace(out) != runID {
		m.t.Fatalf("submit printed %q, want the run id alone", out)
	}
}

// submitArgs is `yad hub submit` for the machine's harness and model.
func (m *machine) submitArgs(args ...string) []string { return m.submitAs(m.h, args...) }

func (m *machine) submitAs(h *e2eHarness, args ...string) []string {
	return append([]string{"hub", "submit", "--hub", m.service, "--harness", h.name, "--model", h.model}, args...)
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
// driven through the harness's adapter, streamed, and reported, and the person
// watching sees the tool call, the answer and the result.
func TestE2ERunSucceeds(t *testing.T) { eachHarness(t, testE2ERunSucceeds) }

func testE2ERunSucceeds(t *testing.T, h *e2eHarness) {
	m := newMachine(t, h)
	m.submit("e2e-1")
	d := m.daemon()

	code, out, errs := m.watch("e2e-1")
	if code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
	for _, want := range []string{h.tool, e2eAnswer, "── succeeded in"} {
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
func TestE2ENetworkDropMidRun(t *testing.T) { eachHarness(t, testE2ENetworkDropMidRun) }

func testE2ENetworkDropMidRun(t *testing.T, h *e2eHarness) {
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
			m := newMachine(t, h)
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

// The runner restarts mid-run (decision 0030). The new process reports the run
// lost — never runs it again — and its result goes out only after the events
// streamed before the restart, every one, exactly as the runner held them.
// The session survives: its native id and its workdir are kept, and the next
// run in it resumes the conversation in the same directory.
func TestE2ERunnerRestartMidRun(t *testing.T) { eachHarness(t, testE2ERunnerRestartMidRun) }

func testE2ERunnerRestartMidRun(t *testing.T, h *e2eHarness) {
	m := newMachine(t, h)
	argsFile := filepath.Join(t.TempDir(), "harness.starts")
	t.Setenv(h.starts, argsFile)
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
	r := localRun(t, s, "e2e-restart")
	if v1.RunState(r.State).IsTerminal() {
		t.Fatalf("a run stopped with its runner is %s; it must stay held for the next start", r.State)
	}
	// Pinned mid-run, before the harness had said anything final.
	before, err := s.GetSession(context.Background(), db.GetSessionParams{Connection: "home", ID: r.SessionID})
	if err != nil || !before.NativeID.Valid || before.Workdir == "" {
		t.Fatalf("session at the restart %+v, %v: the native id and workdir must be recorded mid-run", before, err)
	}
	marker := filepath.Join(before.Workdir, "left-by-the-first-run")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	d = m.daemon()
	eventually(t, "the new process reports the run lost", func() bool {
		return localRun(t, s, "e2e-restart").State == string(v1.RunLost)
	})
	// Its result waits behind its events; the run stays listed meanwhile,
	// so the hub neither has a result nor loses it on a lapsed lease.
	time.Sleep(200 * time.Millisecond)
	if run, err := m.client().Run(context.Background(), "e2e-restart"); err != nil || run.State.Terminal() {
		t.Fatalf("hub run %+v, %v: ended before its events were in", run, err)
	}

	m.cut.Store(nil)
	code, out, errs := m.watch("e2e-restart")
	if code == 0 || !strings.Contains(out, "── lost") || !strings.Contains(errs, "runner_restarted") {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
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
	if run.State != hubapi.RunState(v1.RunLost) || run.Result == nil || run.Result.Error == nil ||
		run.Result.Error.Class != "runner_restarted" || run.Result.LastSeq != int64(len(spooled)) {
		t.Errorf("hub run %+v, result %+v", run, run.Result)
	}
	eventually(t, "the runner owes nothing", func() bool {
		o, err1 := s.OutboxDepth(context.Background())
		sp, err2 := s.SpoolDepth(context.Background())
		return err1 == nil && err2 == nil && o == 0 && sp == 0
	})

	// The next run in the session resumes the conversation where it was,
	// in the same workdir.
	m.open()
	out = m.ok(m.submitArgs("--session", run.SessionID, "--run-id", "e2e-resumed", "Carry on.")...)
	if strings.TrimSpace(out) != "e2e-resumed" {
		t.Fatalf("submit printed %q", out)
	}
	if code, out, errs := m.watch("e2e-resumed"); code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
	after, err := s.GetSession(context.Background(), db.GetSessionParams{Connection: "home", ID: r.SessionID})
	if err != nil || after.NativeID != before.NativeID || after.Workdir != before.Workdir {
		t.Errorf("session after the resume %+v, %v; before the restart %+v", after, err, before)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the workdir lost what the first run left: %v", err)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	starts := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(starts) != 2 {
		t.Fatalf("the harness started %d times, want 2:\n%s", len(starts), b)
	}
	resume := h.resumed(before.NativeID.String)
	if wd, _ := filepath.EvalSymlinks(before.Workdir); !strings.Contains(starts[1], resume) ||
		!(strings.HasPrefix(starts[1], before.Workdir+" ") || strings.HasPrefix(starts[1], wd+" ")) {
		t.Errorf("the second start was %q; want %q in %s", starts[1], resume, before.Workdir)
	}
}

// Cancel and interrupt, from the operator's side: a run mid-flight is stopped
// with `yad hub cancel` or `yad hub interrupt`, the runner hears it at its
// next sync, the harness answers the interrupt, and the run ends cancelled —
// with the latency measured, the watcher told, and no process left behind.
func TestE2EStopMidRun(t *testing.T) { eachHarness(t, testE2EStopMidRun) }

func testE2EStopMidRun(t *testing.T, h *e2eHarness) {
	for _, verb := range []string{"cancel", "interrupt"} {
		t.Run(verb, func(t *testing.T) {
			m := newMachine(t, h)
			pidFile := filepath.Join(t.TempDir(), "harness.pid")
			t.Setenv(h.pid, pidFile)
			m.gated()
			runID := "e2e-" + verb
			m.submit(runID)
			d := m.daemon()
			m.waitAtGate()

			m.ok("hub", verb, "--hub", m.service, runID)
			code, out, errs := m.watch(runID)
			if code == 0 || !strings.Contains(out, "── cancelled") {
				t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
			}
			run, err := m.client().Run(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Result == nil || run.Result.State != v1.RunCancelled || run.Result.Metrics.CancelLatencyMS == nil {
				t.Fatalf("result %+v", run.Result)
			}
			evs := m.hubEvents(runID)
			contiguous(t, evs)
			// Which path the runner took: cancel climbs the ladder, interrupt
			// only asks. Each says so in the run's own stream.
			want := map[string]string{"cancel": "cancelling", "interrupt": "interrupting"}[verb]
			var said []string
			for _, ev := range evs {
				if ev.Kind == v1.EventStatus && (ev.Status == "cancelling" || ev.Status == "interrupting") {
					said = append(said, ev.Status)
				}
			}
			if len(said) != 1 || said[0] != want {
				t.Errorf("stop statuses %v, want [%s]", said, want)
			}

			b, err := os.ReadFile(pidFile)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(b))
			if err != nil {
				t.Fatal(err)
			}
			eventually(t, "the harness process is gone", func() bool { return syscall.Kill(pid, 0) != nil })
		})
	}
}

// A conversation across runs, from the operator's side: a second run
// submitted with --session continues the first run's session — the harness
// resumes it and answers with the first run's context, in the same workdir —
// and `yad sessions` shows it. A transcript that is gone makes the next
// resume fail as resume_rejected, and the session is still listed for the
// hub to decide about.
func TestE2ESessionContinues(t *testing.T) { eachHarness(t, testE2ESessionContinues) }

func testE2ESessionContinues(t *testing.T, h *e2eHarness) {
	m := newMachine(t, h)
	transcripts := t.TempDir()
	h.remember(t, transcripts)
	argsFile := filepath.Join(t.TempDir(), "harness.starts")
	t.Setenv(h.starts, argsFile)
	d := m.daemon()
	submit := func(runID string, session []string, instruction string) {
		t.Helper()
		args := m.submitArgs(append(append([]string{"--run-id", runID}, session...), instruction)...)
		if out := m.ok(args...); strings.TrimSpace(out) != runID {
			t.Fatalf("submit printed %q", out)
		}
	}
	result := func(runID string) *v1.Result {
		t.Helper()
		run, err := m.client().Run(context.Background(), runID)
		if err != nil || run.Result == nil {
			t.Fatalf("run %s: %+v, %v", runID, run, err)
		}
		return run.Result
	}

	submit("e2e-first", []string{"--new-session", "e2e-talk"}, "The word is plum.")
	if code, out, errs := m.watch("e2e-first"); code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
	if got := result("e2e-first").FinalText; got != "earlier: nothing" {
		t.Errorf("the first run answered %q; a new session has no earlier turns", got)
	}
	submit("e2e-second", []string{"--session", "e2e-talk"}, "Which word was it?")
	if code, out, errs := m.watch("e2e-second"); code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
	if got := result("e2e-second").FinalText; got != "earlier: The word is plum." {
		t.Errorf("the second run answered %q; it should have had the first run's turn", got)
	}

	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	starts := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(starts) != 2 {
		t.Fatalf("the harness started %d times, want 2:\n%s", len(starts), b)
	}
	dir0, _, _ := strings.Cut(starts[0], " ")
	dir1, _, _ := strings.Cut(starts[1], " ")
	if dir0 != dir1 || !strings.Contains(starts[0], h.fresh) || !strings.Contains(starts[1], h.resumed("")) {
		t.Errorf("starts:\n%s\nwant a new session then a resume, both in one workdir", b)
	}

	out := m.ok("sessions", "--json")
	var list []session
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("sessions --json: %v\n%s", err, out)
	}
	if len(list) != 1 || list[0].ID != "e2e-talk" || list[0].Connection != "home" || list[0].Runs != 2 ||
		list[0].LiveRun != "" || list[0].NativeID == "" || list[0].State != "open" {
		t.Fatalf("sessions = %+v", list)
	}
	if wd, _ := filepath.EvalSymlinks(list[0].Workdir); dir0 != list[0].Workdir && dir0 != wd {
		t.Errorf("the session's workdir is %s; the harness ran in %s", list[0].Workdir, dir0)
	}
	if out := m.ok("sessions"); !strings.Contains(out, "e2e-talk") || !strings.Contains(out, list[0].Workdir) {
		t.Errorf("yad sessions:\n%s", out)
	}

	if err := os.Remove(filepath.Join(transcripts, list[0].NativeID)); err != nil {
		t.Fatal(err)
	}
	submit("e2e-third", []string{"--session", "e2e-talk"}, "And now?")
	if code, _, errs := m.watch("e2e-third"); code == 0 || !strings.Contains(errs, "resume_rejected") {
		t.Fatalf("watch exit %d: %s\ndaemon:\n%s", code, errs, d.out.String())
	}
	if res := result("e2e-third"); res.State != v1.RunFailed || res.Error == nil || res.Error.Class != "resume_rejected" {
		t.Errorf("result = %+v (%+v)", res, res.Error)
	}
}

// Closing, end to end through the commands an operator types: the hub's
// `yad hub close-session` reaches the runner at its next sync, the workdir
// goes, the hub hears it and refuses a continuation; and the owner's
// `yad sessions close` does the same from the runner's side, which the hub
// hears as closed by the owner.
func TestE2ESessionsClose(t *testing.T) { eachHarness(t, testE2ESessionsClose) }

func testE2ESessionsClose(t *testing.T, h *e2eHarness) {
	m := newMachine(t, h)
	d := m.daemon()
	ctx := context.Background()
	start := func(runID, sessionID string) string {
		t.Helper()
		m.ok(m.submitArgs("--run-id", runID, "--new-session", sessionID, h.instruction)...)
		if code, out, errs := m.watch(runID); code != 0 {
			t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
		}
		var list []session
		if err := json.Unmarshal([]byte(m.ok("sessions", "--json")), &list); err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.ID == sessionID {
				if _, err := os.Stat(s.Workdir); err != nil {
					t.Fatalf("session %s has no workdir: %v", sessionID, err)
				}
				return s.Workdir
			}
		}
		t.Fatalf("no session %s in %+v", sessionID, list)
		return ""
	}
	closedOnHub := func(sessionID, reason string) {
		t.Helper()
		eventually(t, "the hub has "+sessionID+" closed", func() bool {
			s, err := m.client().Session(ctx, sessionID)
			return err == nil && s.State == hubapi.SessionClosed && s.CloseReason == reason
		})
	}
	goneFromDisk := func(dir string) {
		t.Helper()
		eventually(t, "the workdir "+dir+" is gone", func() bool {
			_, err := os.Stat(dir)
			return os.IsNotExist(err)
		})
	}

	byHub := start("e2e-close-1", "e2e-by-hub")
	if out := m.ok("hub", "close-session", "--hub", m.service, "e2e-by-hub"); !strings.Contains(out, "closing") {
		t.Errorf("hub close-session printed %q", out)
	}
	goneFromDisk(byHub)
	closedOnHub("e2e-by-hub", "closed")
	code, _, errs := m.p.yad("", m.submitArgs("--session", "e2e-by-hub", "And now?")...)
	if code == 0 || !strings.Contains(errs, "closed") {
		t.Errorf("a continuation of the closed session: exit %d: %s", code, errs)
	}

	byOwner := start("e2e-close-2", "e2e-by-owner")
	if out := m.ok("sessions", "close", "e2e-by-owner"); !strings.Contains(out, "closed") {
		t.Errorf("sessions close printed %q", out)
	}
	goneFromDisk(byOwner)
	closedOnHub("e2e-by-owner", "closed_by_owner")

	var list []session
	eventually(t, "yad sessions shows both closed and heard", func() bool {
		list = nil
		if err := json.Unmarshal([]byte(m.ok("sessions", "--json")), &list); err != nil {
			t.Fatal(err)
		}
		for _, s := range list {
			if s.State != "closed" || !s.Reclaimed || !s.HubTold || s.ClosedAt == nil {
				return false
			}
		}
		return len(list) == 2
	})
	if out := m.ok("sessions"); !strings.Contains(out, "closed (closed_by_owner)") || !strings.Contains(out, "(reclaimed)") {
		t.Errorf("yad sessions:\n%s", out)
	}
}
