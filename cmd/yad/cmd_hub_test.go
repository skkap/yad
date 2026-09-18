package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hub"
	"github.com/skkap/yad/internal/hub/store"
	"github.com/skkap/yad/internal/hub/store/db"
)

// A token saved for the CLI is never shown: it goes to a 0600 file, and
// nothing the commands print contains it.
func TestAdminTokenCreateSavesAndNeverPrints(t *testing.T) {
	p := newProfile(t)
	code, out, errs := p.yad("", "hub", "admin-token", "create")
	if code != 0 {
		t.Fatalf("create: exit %d: %s", code, errs)
	}
	file := filepath.Join(p.config, "hub-admin-token")
	fi, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v", fi.Mode().Perm())
	}
	tok, err := config.ReadSecret(file)
	if err != nil || !strings.HasPrefix(tok, "yadadm_") {
		t.Fatalf("saved %q, %v", tok[:min(len(tok), 7)], err)
	}
	if strings.Contains(out+errs, tok) {
		t.Error("create printed the token it saved")
	}

	// A second create would orphan the saved token, so it is refused before
	// a new one exists.
	if code, _, errs := p.yad("", "hub", "admin-token", "create", "--name", "other"); code == 0 || !strings.Contains(errs, "already holds") {
		t.Errorf("second create: exit %d %q", code, errs)
	}
	code, out, _ = p.yad("", "hub", "admin-token", "list")
	if code != 0 || !strings.Contains(out, "cli") || strings.Contains(out, "other") || strings.Contains(out, tok) {
		t.Errorf("list: exit %d %q", code, out)
	}

	// For a service: printed once, to stdout alone.
	code, out, errs = p.yad("", "hub", "admin-token", "create", "--name", "yashiki", "--out", "-")
	if code != 0 || !strings.HasPrefix(out, "yadadm_") || strings.Count(out, "\n") != 1 || strings.Contains(errs, strings.TrimSpace(out)) {
		t.Errorf("--out -: exit %d out %q err %q", code, out, errs)
	}

	if code, _, errs := p.yad("", "hub", "admin-token", "revoke", "yashiki"); code != 0 {
		t.Errorf("revoke: exit %d %s", code, errs)
	}
	if code, _, errs := p.yad("", "hub", "admin-token", "revoke", "yashiki"); code == 0 || !strings.Contains(errs, "admin-token list") {
		t.Errorf("revoke twice: exit %d %q", code, errs)
	}
}

func TestSubmitNeedsAToken(t *testing.T) {
	p := newProfile(t)
	code, _, errs := p.yad("", "hub", "submit", "--harness", "claude", "--model", "opus", "hi")
	if code == 0 || !strings.Contains(errs, "yad hub admin-token create") {
		t.Errorf("exit %d %q", code, errs)
	}
	// A token file others can read is treated as leaked, not used.
	file := filepath.Join(p.config, "hub-admin-token")
	if err := os.WriteFile(file, []byte("yadadm_x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs = p.yad("", "hub", "submit", "--harness", "claude", "--model", "opus", "hi")
	if code == 0 || !strings.Contains(errs, "readable by others") || strings.Contains(errs, "yadadm_x") {
		t.Errorf("exposed file: exit %d %q", code, errs)
	}
}

func TestSubmitUsage(t *testing.T) {
	p := newProfile(t)
	for _, args := range [][]string{
		{"hub", "submit", "hi"},
		{"hub", "submit", "--harness", "claude", "hi"},
		{"hub", "submit", "--harness", "claude", "--model", "opus"},
		{"hub", "submit", "--harness", "claude", "--model", "opus", "--session", "a", "--new-session", "b", "hi"},
		{"hub", "watch"},
	} {
		if code, _, errs := p.yad("", args...); code == 0 || errs == "" {
			t.Errorf("%v: exit %d %q", args, code, errs)
		}
	}
}

// syncBuffer is written by the command under test and read by the test while
// the command runs.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// hubRig is a hub serving a profile's hub.db over loopback, with that
// profile's CLI pointed at it and holding an admin token.
type hubRig struct {
	p   *profile
	hub *hub.Hub
	s   *store.Store
}

func newHubRig(t *testing.T) *hubRig {
	p := newProfile(t)
	if code, _, errs := p.yad("", "hub", "admin-token", "create"); code != 0 {
		t.Fatalf("admin-token create: %s", errs)
	}
	s, err := store.Open(context.Background(), filepath.Join(p.data, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	h := hub.New(hub.Options{Store: s})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("YAD_HUB_URL", srv.URL)
	return &hubRig{p: p, hub: h, s: s}
}

func (r *hubRig) submit(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errs := r.p.yad("", append([]string{"hub", "submit", "--harness", "claude", "--model", "opus"}, args...)...)
	if code != 0 {
		t.Fatalf("submit: exit %d: %s", code, errs)
	}
	return strings.TrimSpace(out)
}

// watch starts `yad hub watch` and returns its output as it grows and a
// channel with its exit code.
func (r *hubRig) watch(t *testing.T, runID string) (*syncBuffer, *syncBuffer, <-chan int) {
	r.p.t.Setenv("YAD_CONFIG_DIR", r.p.config)
	r.p.t.Setenv("YAD_DATA_DIR", r.p.data)
	out, errs := &syncBuffer{}, &syncBuffer{}
	exit := make(chan int, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { exit <- run(ctx, []string{"hub", "watch", runID}, out, errs) }()
	return out, errs, exit
}

func (r *hubRig) event(t *testing.T, runID string, ev v1.Event) {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.AppendEvent(context.Background(), db.AppendEventParams{RunID: runID, Seq: ev.Seq, Body: string(b), ReceivedAt: 1}); err != nil {
		t.Fatal(err)
	}
	r.hub.Changed()
}

// finish writes a result and moves the run to its state, as the runner's
// result upload will once DEV-6 lands the protocol side.
func (r *hubRig) finish(t *testing.T, runID string, res v1.Result) {
	t.Helper()
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := r.s.PutResult(ctx, db.PutResultParams{RunID: runID, State: string(res.State), Body: string(b), ReceivedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.DB.ExecContext(ctx, "UPDATE runs SET state = ? WHERE id = ?", string(res.State), runID); err != nil {
		t.Fatal(err)
	}
	r.hub.Changed()
}

func waitFor(t *testing.T, what string, buf *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(buf.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never showed %q; it has:\n%s", what, want, buf)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The person's path: submit, then watch events appear one at a time while
// the run is live, ending with its result.
func TestSubmitThenWatch(t *testing.T) {
	r := newHubRig(t)
	id := r.submit(t, "--context", "you are in a test", "say hello")
	if !strings.HasPrefix(id, "run_") || strings.Contains(id, "\n") {
		t.Fatalf("submit printed %q, want the run id alone", id)
	}
	out, errs, exit := r.watch(t, id)
	waitFor(t, "watch", out, "── queued")

	at := time.Now()
	r.event(t, id, v1.Event{Seq: 1, At: at, Kind: v1.EventText, Text: "hello \x1b[31mthere"})
	// Shown before the run ends: watch streams, it does not wait for the end.
	waitFor(t, "watch", out, "hello")
	r.event(t, id, v1.Event{Seq: 2, At: at, Kind: v1.EventToolCall, Tool: &v1.ToolEvent{ID: "t1", Name: "Bash", Input: `{"command":"ls"}`}})
	r.event(t, id, v1.Event{Seq: 3, At: at, Kind: v1.EventToolResult, Tool: &v1.ToolEvent{ID: "t1", Output: "README.md"}})
	r.finish(t, id, v1.Result{State: v1.RunSucceeded, FinalText: "all done", LastSeq: 3, Metrics: v1.Metrics{DurationMS: 1500, ToolCalls: 1}})

	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("watch exit %d: %s", code, errs)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("watch did not end; it printed:\n%s", out)
	}
	got := out.String()
	for _, want := range []string{"── queued", "hello", "→ Bash {\"command\":\"ls\"}", "← README.md", "all done", "── succeeded in 1.5s, 1 tool calls"} {
		if !strings.Contains(got, want) {
			t.Errorf("watch output lacks %q:\n%s", want, got)
		}
	}
	// Harness output is data: an escape sequence in it never reaches the
	// terminal as one.
	if strings.Contains(got, "\x1b") {
		t.Errorf("watch passed a terminal escape through:\n%q", got)
	}
}

func TestWatchFailsWithTheRun(t *testing.T) {
	r := newHubRig(t)
	id := r.submit(t, "break")
	r.finish(t, id, v1.Result{State: v1.RunFailed, Error: &v1.RunError{Class: "harness", Message: "it broke"}})
	out, errs, exit := r.watch(t, id)
	select {
	case code := <-exit:
		if code != 1 || !strings.Contains(errs.String(), "failed: harness: it broke") || !strings.Contains(out.String(), "── failed") {
			t.Errorf("exit %d out %q err %q", code, out, errs)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not end")
	}

	if code, _, errs := r.p.yad("", "hub", "watch", "no-such-run"); code == 0 || !strings.Contains(errs, "no run") {
		t.Errorf("unknown run: exit %d %q", code, errs)
	}
}

// --session continues what an earlier submit started; the hub, not the CLI,
// holds the flag to the truth.
func TestSubmitIntoSessions(t *testing.T) {
	r := newHubRig(t)
	r.submit(t, "--new-session", "s1", "--run-id", "a", "first")
	r.submit(t, "--session", "s1", "second")
	if code, _, errs := r.p.yad("", "hub", "submit", "--harness", "claude", "--model", "opus", "--new-session", "s1", "again"); code == 0 || !strings.Contains(errs, "already exists") {
		t.Errorf("new session twice: exit %d %q", code, errs)
	}
	// A retry with the same run id and content is the same run.
	if again := r.submit(t, "--new-session", "s1", "--run-id", "a", "first"); again != "a" {
		t.Errorf("retry answered %q", again)
	}
	// The instruction from stdin, for anything longer than a line.
	code, out, errs := r.p.yad("a long\ninstruction\n", "hub", "submit", "--harness", "claude", "--model", "opus", "-")
	if code != 0 {
		t.Fatalf("stdin: exit %d %s", code, errs)
	}
	run, err := r.s.GetRun(context.Background(), strings.TrimSpace(out))
	if err != nil || !strings.Contains(run.Spec, `a long\ninstruction`) {
		t.Errorf("stored %q, %v", run.Spec, err)
	}
}
