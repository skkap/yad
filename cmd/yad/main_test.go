package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// shortDir is a private temporary directory short enough to hold the
// control socket: t.TempDir() under macOS's $TMPDIR, with a long test name,
// passes the 103-byte limit on its own.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "yad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func yad(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("YAD_CONFIG_DIR", t.TempDir())
	t.Setenv("YAD_DATA_DIR", shortDir(t))
	t.Setenv("PATH", t.TempDir())
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestHarnessesPrintsTheCapabilityDocument(t *testing.T) {
	code, out, errs := yad(t, "harnesses")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var doc v1.Capabilities
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not a capability document: %v\n%s", err, out)
	}
	if doc.RunnerID == "" || len(doc.Harnesses) == 0 || doc.Capacity.Total < 1 {
		t.Errorf("document = %+v", doc)
	}
}

// A command that is not built yet must say which epic brings it.
func TestUnbuiltCommandsNameTheirEpic(t *testing.T) {
	for cmd := range arrivesIn {
		code, _, errs := yad(t, cmd)
		if code != 1 || !strings.Contains(errs, "arrives in epic E") {
			t.Errorf("yad %s: exit %d, %q", cmd, code, errs)
		}
	}
}

func TestProfileIsValidated(t *testing.T) {
	if code, _, _ := yad(t, "--profile", "../x", "version"); code != 2 {
		t.Errorf("a path-shaped profile exited %d", code)
	}
}

func TestDoctorRunsOnAnEmptyMachine(t *testing.T) {
	code, out, _ := yad(t, "doctor")
	if code != 0 || !strings.Contains(out, "No drivable harness") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// time.NewTicker panics on a non-positive duration; the flag must be refused
// with a next action instead of a stack trace.
func TestDaemonRefusesNonPositiveInterval(t *testing.T) {
	for _, v := range []string{"0", "-1s"} {
		code, _, errs := yad(t, "daemon", "start", "--foreground", "--interval", v)
		if code == 0 || !strings.Contains(errs, "--interval must be positive") {
			t.Errorf("--interval %s: exit %d, %q", v, code, errs)
		}
	}
}

// Installed but without an adapter is a different fact from not installed, and
// needs a different next action.
func TestDoctorSaysWhyNothingIsDrivable(t *testing.T) {
	code, out, _ := yad(t, "doctor") // yad() empties PATH
	if code != 0 || !strings.Contains(out, "Install Claude Code or Codex") {
		t.Errorf("empty machine: exit %d:\n%s", code, out)
	}
	dir := t.TempDir()
	gemini := dir + "/gemini"
	if err := os.WriteFile(gemini, []byte("#!/bin/sh\necho '0.9.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAD_GEMINI_PATH", gemini)
	var o, e bytes.Buffer
	run(context.Background(), []string{"doctor"}, &o, &e)
	if !strings.Contains(o.String(), "no adapter in this yad yet") || !strings.Contains(o.String(), "install Claude Code or Codex") {
		t.Errorf("gemini installed, no adapter:\n%s", o.String())
	}

	bin := dir + "/claude"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '2.1.276 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAD_CLAUDE_PATH", bin)
	o.Reset()
	run(context.Background(), []string{"doctor"}, &o, &e)
	if !strings.Contains(o.String(), "1 harness(es) this runner can be given work for") {
		t.Errorf("claude installed, with its adapter:\n%s", o.String())
	}
	if !regexp.MustCompile(`Claude Code +ready`).MatchString(o.String()) {
		t.Errorf("claude not reported ready:\n%s", o.String())
	}

	// A broken Claude beside a recognised Gemini: the fix is the probe
	// error, not an install.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o.Reset()
	run(context.Background(), []string{"doctor"}, &o, &e)
	if !strings.Contains(o.String(), "failed its version probe") || strings.Contains(o.String(), "install Claude Code") {
		t.Errorf("claude broken, gemini present:\n%s", o.String())
	}

	os.Remove(gemini)
	o.Reset()
	run(context.Background(), []string{"doctor"}, &o, &e)
	if !strings.Contains(o.String(), "failed its version probe") {
		t.Errorf("claude broken:\n%s", o.String())
	}
}

// Codex is first-class: an installed one is ready, and one whose app-server
// protocol is not the pinned one is still ready, with the drift said as a
// warning in doctor and in the capability document (decision 0037).
func TestDoctorReportsCodexProtocolDrift(t *testing.T) {
	codex := fakeCodexBin(t)
	t.Setenv("YAD_CODEX_PATH", codex)
	schema, err := filepath.Abs(codexSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_TEST_SCHEMA", schema)
	code, out, errs := yad(t, "doctor")
	if code != 0 || !regexp.MustCompile(`Codex +ready +codex-cli 0.147.0`).MatchString(out) || strings.Contains(out, "warning:") {
		t.Fatalf("pinned codex: exit %d:\n%s%s", code, out, errs)
	}

	drifted := filepath.Join(t.TempDir(), "drifted.json")
	b, err := os.ReadFile(schema)
	if err != nil {
		t.Fatal(err)
	}
	// Any change to a status a turn can end in is drift.
	b = bytes.Replace(b, []byte(`"interrupted",`), []byte(`"interrupted","paused",`), 1)
	if err := os.WriteFile(drifted, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_TEST_SCHEMA", drifted)
	// Another path, so the check the first doctor remembered is not reused.
	t.Setenv("YAD_CODEX_PATH", fakeCodexBin(t))
	code, out, errs = yad(t, "doctor")
	if code != 0 || !regexp.MustCompile(`Codex +ready`).MatchString(out) || !strings.Contains(out, "warning: Codex — the app-server protocol of codex-cli 0.147.0 differs") {
		t.Fatalf("drifted codex: exit %d:\n%s%s", code, out, errs)
	}
	code, out, errs = yad(t, "harnesses")
	if code != 0 || !strings.Contains(out, `"warnings": [`) || !strings.Contains(out, "differs from the one this yad was built against") {
		t.Fatalf("harnesses: exit %d:\n%s%s", code, out, errs)
	}
}

// `yad sessions` on a profile whose runner never ran lists nothing and
// creates nothing; closing one names the task that brings it.
func TestSessionsOnAFreshProfile(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		out  string
		errs string
	}{
		{[]string{"sessions"}, 0, "no sessions", ""},
		{[]string{"sessions", "--json"}, 0, "[]", ""},
		{[]string{"sessions", "close", "s1"}, 1, "", "DEV-18"},
		{[]string{"sessions", "s1"}, 1, "", "unexpected"},
	} {
		code, out, errs := yad(t, tc.args...)
		if code != tc.code || !strings.Contains(out, tc.out) || !strings.Contains(errs, tc.errs) {
			t.Errorf("yad %v: exit %d, %q, %q", tc.args, code, out, errs)
		}
		if _, err := os.Stat(os.Getenv("YAD_DATA_DIR") + "/state.db"); !os.IsNotExist(err) {
			t.Errorf("yad %v created the state database", tc.args)
		}
	}
}
