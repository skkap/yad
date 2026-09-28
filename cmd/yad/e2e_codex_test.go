package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// The fake codex's side of the end-to-end tests: the test binary started as
// `codex`, playing conversations recorded from codex 0.157.1 (codextest).
// Every end-to-end test runs through it (e2e_harness_test.go); the one here
// plays the recorded resume, missing rollout and interrupt as recorded, where
// the others have the fake keep threads itself.

const (
	codexFixtures = "../../internal/adapter/codex/testdata/codex-0.157.1/"
	codexSchema   = codexFixtures + "codex_app_server_protocol.schemas.json"
)

// fakeCodexBin is this test binary under the name codex, which is how its
// TestMain knows to play Codex rather than Claude.
func fakeCodexBin(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.Symlink(self, bin); err != nil {
		t.Fatal(err)
	}
	return bin
}

// codexPlays makes the next codex started play this recorded conversation.
func codexPlays(t *testing.T, name string) {
	t.Helper()
	p, err := filepath.Abs(codexFixtures + name + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_TEST_FIXTURE", p)
}

// recordedThread is the thread a recorded conversation ran in.
func recordedThread(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(codexFixtures + name + ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(b), `"thread":{"id":"`)
	id, _, _ := strings.Cut(rest, `"`)
	if !ok || id == "" {
		t.Fatalf("%s has no thread", name)
	}
	return id
}

// A Codex session across runs: a run in a new session is driven by the Codex
// adapter and reported, and its thread becomes the session's native id; the
// next run resumes that thread in the same workdir; a thread whose rollout is
// gone fails resume_rejected (decision 0031); and an interrupt from the hub
// ends a run cancelled.
func TestE2ECodexSession(t *testing.T) {
	m := newMachine(t, codexE2E)
	// The thread ids are the recorded ones here.
	t.Setenv("CODEX_TEST_THREADS", "")
	log := filepath.Join(t.TempDir(), "codex.log")
	t.Setenv("CODEX_TEST_LOG", log)

	if out := m.ok("harnesses"); !strings.Contains(out, `"id": "codex"`) || strings.Contains(out, "warnings") {
		t.Fatalf("the capability document:\n%s", out)
	}
	d := m.daemon()
	submit := func(runID string, session []string) {
		t.Helper()
		args := append([]string{"hub", "submit", "--hub", m.service, "--harness", "codex", "--model", "gpt-5.6-luna", "--run-id", runID}, session...)
		if out := m.ok(append(args, "Run the shell command `cat note.txt` and reply with its output only.")...); strings.TrimSpace(out) != runID {
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

	codexPlays(t, "tool")
	submit("cx-first", []string{"--new-session", "cx-talk"})
	code, out, errs := m.watch("cx-first")
	if code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
	for _, want := range []string{"→ shell", "hello from a small file", "── succeeded in"} {
		if !strings.Contains(out, want) {
			t.Errorf("watch output lacks %q:\n%s", want, out)
		}
	}
	res := result("cx-first")
	if res.State != v1.RunSucceeded || res.FinalText != "hello from a small file" || len(res.Usage.ByModel) == 0 || res.Metrics.ToolCalls != 1 {
		t.Fatalf("result = %+v", res)
	}
	evs := m.hubEvents("cx-first")
	contiguous(t, evs)

	var list []session
	if err := json.Unmarshal([]byte(m.ok("sessions", "--json")), &list); err != nil {
		t.Fatal(err)
	}
	// The thread tool.jsonl was recorded in: Codex chose it, and the runner
	// pinned it.
	if len(list) != 1 || list[0].NativeID != recordedThread(t, "tool") {
		t.Fatalf("sessions = %+v", list)
	}
	thread := list[0].NativeID

	codexPlays(t, "resume")
	submit("cx-second", []string{"--session", "cx-talk"})
	if code, out, errs := m.watch("cx-second"); code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
	if res := result("cx-second"); res.State != v1.RunSucceeded || res.FinalText != "plum" {
		t.Errorf("the resumed run: %+v", res)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `\"method\":\"thread/resume\"`) || !strings.Contains(string(b), thread) {
		t.Errorf("the second run did not resume thread %s:\n%s", thread, b)
	}

	codexPlays(t, "resume-missing")
	submit("cx-third", []string{"--session", "cx-talk"})
	if code, _, errs := m.watch("cx-third"); code == 0 || !strings.Contains(errs, "resume_rejected") {
		t.Fatalf("watch exit %d: %s\ndaemon:\n%s", code, errs, d.out.String())
	}
	if res := result("cx-third"); res.State != v1.RunFailed || res.Error == nil || res.Error.Class != "resume_rejected" {
		t.Errorf("result = %+v (%+v)", res, res.Error)
	}

	codexPlays(t, "interrupt")
	submit("cx-fourth", []string{"--new-session", "cx-stop"})
	eventually(t, "the run is streaming text", func() bool {
		for _, ev := range m.hubEvents("cx-fourth") {
			if ev.Kind == v1.EventText {
				return true
			}
		}
		return false
	})
	m.ok("hub", "interrupt", "--hub", m.service, "cx-fourth")
	if code, out, errs := m.watch("cx-fourth"); code == 0 || !strings.Contains(out, "── cancelled") {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
	if res := result("cx-fourth"); res.State != v1.RunCancelled || res.Metrics.CancelLatencyMS == nil {
		t.Errorf("result = %+v", res)
	}
}
