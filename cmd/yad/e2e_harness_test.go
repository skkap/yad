package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/harness"
)

// The end-to-end tests run once per harness with an adapter. The runner, the
// executor and the hub are meant to know nothing of which harness a run is
// for beyond its adapter; running every path through each one is how that
// stays true (epic E5).

// e2eHarness is one harness as the end-to-end tests drive it: its fake, the
// recorded turn the fake plays, and the names of the knobs that tell the fake
// how to behave. Both fakes answer "hello from a small file" (e2eAnswer).
type e2eHarness struct {
	name, model string
	// instruction is what the recorded turn was asked.
	instruction string
	// tool is how `yad hub watch` shows the recorded tool call.
	tool string
	// The fake's knobs, by environment variable: a gate it waits at before
	// its turn ends and the file it creates on reaching it, a file for its
	// pid, a file it appends a line to per conversation it opens (its
	// working directory, arguments and how it opened it), the variable that
	// makes it deaf to an interrupt at the gate when set to "deaf", and a
	// file in its working directory whose contents replace the answer.
	gate, atGate, pid, starts, deaf, read string
	// login is the variable that, set to "fails", makes the fake's own login
	// exit non-zero having written nothing.
	login string
	// install points the runner at the fake, playing the recorded turn.
	install func(t *testing.T)
	// remember makes the fake keep a conversation per native session id in
	// dir, as the harness keeps them — a resume of one that is not there is
	// refused — and answer with what the session was asked before
	// ("earlier: a | b", or "earlier: nothing").
	remember func(t *testing.T, dir string)
	// fresh is how a start line shows a new conversation; resumed, how it
	// shows the resume of native.
	fresh   string
	resumed func(native string) string
}

var claudeE2E = &e2eHarness{
	name: "claude", model: "haiku",
	instruction: "Use the Read tool to read note.txt, then reply with its contents only.",
	tool:        "→ Read",
	gate:        fakeClaudeGate, atGate: fakeClaudeAtGate, pid: fakeClaudePID,
	starts: fakeClaudeArgs, deaf: fakeClaudeDeaf, read: fakeClaudeRead, login: fakeClaudeLogin,
	install: func(t *testing.T) {
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("YAD_CLAUDE_PATH", self)
		t.Setenv(fakeClaudeFixture, abs(t, e2eFixture))
	},
	remember: func(t *testing.T, dir string) { t.Setenv(fakeClaudeTranscripts, dir) },
	fresh:    "--session-id ",
	resumed:  func(native string) string { return "--resume " + native },
}

var codexE2E = &e2eHarness{
	name: "codex", model: "gpt-5.6-luna",
	instruction: "Run the shell command `cat note.txt` and reply with its output only.",
	tool:        "→ shell",
	gate:        "CODEX_TEST_GATE", atGate: "CODEX_TEST_AT_GATE", pid: "CODEX_TEST_PID",
	starts: "CODEX_TEST_STARTS", deaf: "CODEX_TEST_MODE", read: "CODEX_TEST_READ", login: "CODEX_TEST_LOGIN",
	install: func(t *testing.T) {
		t.Setenv("YAD_CODEX_PATH", fakeCodexBin(t))
		t.Setenv("CODEX_TEST_FIXTURE", abs(t, codexFixtures+"tool.jsonl"))
		t.Setenv("CODEX_TEST_SCHEMA", abs(t, codexSchema))
		// A loaded -race run can take longer than the fake's default
		// between one message of the adapter's and the next.
		t.Setenv("CODEX_TEST_WAIT", "30s")
		// Threads are kept from the start, as codex keeps them, so a run
		// can resume the one before it whatever the test.
		t.Setenv("CODEX_TEST_THREADS", t.TempDir())
	},
	remember: func(t *testing.T, dir string) {
		t.Setenv("CODEX_TEST_THREADS", dir)
		t.Setenv("CODEX_TEST_RECALL", "1")
	},
	fresh:   "thread/start",
	resumed: func(native string) string { return "thread/resume " + native },
}

var e2eHarnesses = []*e2eHarness{claudeE2E, codexE2E}

// eachHarness runs an end-to-end test once per harness.
func eachHarness(t *testing.T, test func(t *testing.T, h *e2eHarness)) {
	t.Helper()
	for _, h := range e2eHarnesses {
		t.Run(h.name, func(t *testing.T) { test(t, h) })
	}
}

func abs(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// One runner with both harnesses installed drives a Claude run and a Codex
// run at once, each through its own adapter, each held mid-run until both
// are: nothing in the runner is shared between them that should not be. Each
// session keeps its harness — the hub refuses a Codex run in a Claude session
// before any runner sees it.
func TestE2EBothHarnessesAtOnce(t *testing.T) {
	m := newMachine(t, claudeE2E)
	codexE2E.install(t)
	if out := m.ok("harnesses"); !strings.Contains(out, `"id": "claude"`) || !strings.Contains(out, `"id": "codex"`) || strings.Contains(out, "warnings") {
		t.Fatalf("the capability document:\n%s", out)
	}
	codexAtGate := filepath.Join(t.TempDir(), "codex-reached")
	m.gated()
	t.Setenv(codexE2E.gate, m.gate)
	t.Setenv(codexE2E.atGate, codexAtGate)

	m.ok(m.submitAs(claudeE2E, "--run-id", "both-claude", "--new-session", "both-claude", claudeE2E.instruction)...)
	m.ok(m.submitAs(codexE2E, "--run-id", "both-codex", "--new-session", "both-codex", codexE2E.instruction)...)
	d := m.daemon()
	m.waitAtGate()
	eventually(t, "the codex run is mid-run too", func() bool {
		_, err := os.Stat(codexAtGate)
		return err == nil
	})
	m.open()

	for _, h := range e2eHarnesses {
		code, out, errs := m.watch("both-" + h.name)
		if code != 0 {
			t.Fatalf("%s: watch exit %d: %s\n%s\ndaemon:\n%s", h.name, code, errs, out, d.out.String())
		}
		for _, want := range []string{h.tool, e2eAnswer, "── succeeded in"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: watch output lacks %q:\n%s", h.name, want, out)
			}
		}
	}
	var list []session
	if err := json.Unmarshal([]byte(m.ok("sessions", "--json")), &list); err != nil {
		t.Fatal(err)
	}
	harnessOf := map[string]string{}
	for _, s := range list {
		harnessOf[s.ID] = s.Harness
	}
	if len(list) != 2 || harnessOf["both-claude"] != "claude" || harnessOf["both-codex"] != "codex" {
		t.Errorf("sessions = %+v", list)
	}

	code, _, errs := m.p.yad("", m.submitAs(codexE2E, "--session", "both-claude", "And now?")...)
	if code == 0 || !strings.Contains(errs, "claude session") {
		t.Errorf("a codex run in a claude session: exit %d: %s", code, errs)
	}
}

// An override naming nothing is not the end of a runner: detection falls back
// to PATH, advertises the harness with a warning rather than an error, and the
// run starts the binary detection found — PATH's, not the one the dead
// override named (DEV-68). Only the fake on PATH can play the turn here, so a
// run that succeeds is one that started it.
func TestE2EAnOverrideNamingNothingRunsPATHs(t *testing.T) {
	eachHarness(t, testE2EAnOverrideNamingNothingRunsPATHs)
}

func testE2EAnOverrideNamingNothingRunsPATHs(t *testing.T, h *e2eHarness) {
	m := newMachine(t, h)
	entry, _ := harness.Lookup(h.name)
	fake := os.Getenv(entry.EnvPath)
	onPATH := t.TempDir()
	if err := os.Symlink(fake, filepath.Join(onPATH, entry.Binary)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", onPATH)
	t.Setenv(entry.EnvPath, filepath.Join(t.TempDir(), "uninstalled", entry.Binary))

	var doc v1.Capabilities
	if err := json.Unmarshal([]byte(m.ok("harnesses")), &doc); err != nil {
		t.Fatal(err)
	}
	for _, r := range doc.Harnesses {
		if r.ID != h.name {
			continue
		}
		if !r.Present || r.Error != "" || len(r.Warnings) == 0 || !strings.Contains(r.Warnings[0], entry.EnvPath) {
			t.Fatalf("%s is reported %+v, want it present and drivable, with a warning naming %s", h.name, r, entry.EnvPath)
		}
	}
	if !capability.Drivable(doc, h.name) {
		t.Fatalf("%s is not drivable after falling back to PATH", h.name)
	}

	m.submit("fallback-1")
	d := m.daemon()
	code, out, errs := m.watch("fallback-1")
	if code != 0 || !strings.Contains(out, e2eAnswer) || !strings.Contains(out, "── succeeded in") {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
}
