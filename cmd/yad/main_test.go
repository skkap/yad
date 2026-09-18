package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

func yad(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("YAD_CONFIG_DIR", t.TempDir())
	t.Setenv("YAD_DATA_DIR", t.TempDir())
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
	bin := dir + "/claude"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '2.1.276 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAD_CLAUDE_PATH", bin)
	var o, e bytes.Buffer
	run(context.Background(), []string{"doctor"}, &o, &e)
	if !strings.Contains(o.String(), "no adapter in this yad yet") {
		t.Errorf("claude installed, no adapter:\n%s", o.String())
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o.Reset()
	run(context.Background(), []string{"doctor"}, &o, &e)
	if !strings.Contains(o.String(), "failed its version probe") {
		t.Errorf("claude broken:\n%s", o.String())
	}
}
