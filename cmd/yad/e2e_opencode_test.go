package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter/acp/acptest"
)

// The fake OpenCode's side of the end-to-end tests: the test binary started
// as `opencode`, playing conversations recorded from OpenCode 1.18.33 over
// ACP (acptest). OpenCode is not in eachHarness's matrix: its fake plays one
// recorded conversation per run and keeps no sessions of its own, which the
// matrix's resume and fork tests need. These run the paths OpenCode's turn
// takes through the runner and yad hub that no adapter test reaches: the
// claim, the events, the result, an interrupt from a hub, and a steer a hub
// refuses because OpenCode's own feature list has none (decision 0069).
// Filed as a follow-up: a fake that keeps sessions, and OpenCode in the matrix.

const opencodeFixtures = "../../internal/adapter/opencode/testdata/opencode-1.18.33/"

var opencodeE2E = &e2eHarness{
	name: "opencode", model: "opencode/nemotron-3.5-lightning-free",
	instruction: "Use the bash tool to run `cat note.txt`, then reply with its output only.",
	tool:        "→ bash",
	gate:        acptest.EnvGate, atGate: acptest.EnvAtGate,
	install: func(t *testing.T) {
		t.Setenv("YAD_OPENCODE_PATH", fakeOpenCodeBin(t))
		opencodePlays(t, "tool")
		// A loaded -race run can take longer than the fake's default
		// between one message of the adapter's and the next.
		t.Setenv(acptest.EnvWait, "30s")
	},
}

// fakeOpenCodeBin is this test binary under the name opencode, which is how
// its TestMain knows to play OpenCode.
func fakeOpenCodeBin(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "opencode")
	if err := os.Symlink(self, bin); err != nil {
		t.Fatal(err)
	}
	return bin
}

// opencodePlays makes the next opencode started play this recorded
// conversation.
func opencodePlays(t *testing.T, name string) {
	t.Helper()
	t.Setenv(acptest.EnvFixture, abs(t, opencodeFixtures+name+".jsonl"))
}

// An OpenCode run end to end: the runner advertises it first-class with its
// own features, runs its turn, and the hub gets the tool call, the answer
// and the result — and an interrupt from the hub ends a running turn
// cancelled.
func TestE2EOpenCode(t *testing.T) {
	m := newMachine(t, opencodeE2E)
	var doc v1.Capabilities
	if err := json.Unmarshal([]byte(m.ok("harnesses")), &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range doc.Harnesses {
		if h.ID != "opencode" {
			continue
		}
		found = true
		if h.Kind != "first-class" || !h.Present || h.Error != "" || len(h.Warnings) != 0 {
			t.Errorf("opencode in the document: %+v", h)
		}
		if strings.Join(h.Features, ",") != "interrupt,effort,fork" {
			t.Errorf("opencode's features %v", h.Features)
		}
	}
	if !found {
		t.Fatalf("no opencode in the document: %+v", doc.Harnesses)
	}

	d := m.daemon()
	m.ok(m.submitArgs("--run-id", "oc-first", "--new-session", "oc-talk", opencodeE2E.instruction)...)
	code, out, errs := m.watch("oc-first")
	if code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
	for _, want := range []string{opencodeE2E.tool, e2eAnswer, "── succeeded in"} {
		if !strings.Contains(out, want) {
			t.Errorf("watch output lacks %q:\n%s", want, out)
		}
	}

	// A steer is refused by the hub, never queued: OpenCode takes none, and
	// the runner's list for it says so.
	opencodePlays(t, "plain")
	m.gated()
	m.ok(m.submitArgs("--run-id", "oc-second", "--new-session", "oc-stop", "Reply with exactly: pong")...)
	m.waitAtGate()
	if code, _, errs := m.p.yad("", "hub", "steer", "--hub", m.service, "oc-second", "and more"); code == 0 || !strings.Contains(errs, `"opencode"`) {
		t.Errorf("a steer to an opencode run: exit %d: %s", code, errs)
	}
	m.ok("hub", "interrupt", "--hub", m.service, "oc-second")
	if code, out, errs := m.watch("oc-second"); code == 0 || !strings.Contains(out, "── cancelled") {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
}
