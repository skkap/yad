package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/control"
)

// The daemon rebuilds its document every interval and asks a failed login
// again every few minutes, so a reason is logged when it appears or changes
// and its clearing once — never on every rebuild (DEV-146). The reason rides
// as an attribute: a hub's health hears the message alone.
func TestModelsFailuresAreLoggedOncePerChange(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	noted := modelsNoted{}
	old := capability.ModelsFailure{Harness: "claude", Account: "work", Reason: "claude refused list_models"}
	timeout := capability.ModelsFailure{Harness: "claude", Account: "work", Reason: "claude did not answer list_models within 15s"}
	own := capability.ModelsFailure{Harness: "codex", Reason: "codex refused model/list"}
	for _, step := range []struct {
		name    string
		failing []capability.ModelsFailure
		// logged is each record's level, harness and account, in order.
		logged []string
	}{
		{"first seen", []capability.ModelsFailure{old}, []string{"WARN claude work"}},
		{"the same again", []capability.ModelsFailure{old}, nil},
		{"another login too", []capability.ModelsFailure{old, own}, []string{"WARN codex "}},
		{"a new reason", []capability.ModelsFailure{timeout, own}, []string{"WARN claude work"}},
		{"one answers", []capability.ModelsFailure{own}, []string{"INFO claude work"}},
		{"all answer", nil, []string{"INFO codex "}},
		{"and stay answered", nil, nil},
		{"fails again", []capability.ModelsFailure{old}, []string{"WARN claude work"}},
	} {
		buf.Reset()
		noted.note(log, step.failing)
		var got []string
		for line := range strings.Lines(buf.String()) {
			var r struct {
				Level, Msg, Harness, Account, Reason string
			}
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				t.Fatal(err)
			}
			got = append(got, r.Level+" "+r.Harness+" "+r.Account)
			if r.Level == "WARN" && (r.Reason == "" || strings.Contains(r.Msg, r.Reason)) {
				t.Errorf("%s: the reason is not an attribute of its own: %s", step.name, line)
			}
		}
		if strings.Join(got, "|") != strings.Join(step.logged, "|") {
			t.Errorf("%s: logged %q, want %q", step.name, got, step.logged)
		}
	}
}

// The whole path on a real daemon: a Claude too old to list its models, the
// daemon's warning, and `yad doctor` — which asks the daemon over its socket
// rather than write or read state — saying why beside the harness, with the
// command that upgrades it. Doctor still exits 0: a harness whose models are
// the catalog's takes runs all the same.
func TestE2EDoctorSaysWhyModelsAreTheCatalogs(t *testing.T) {
	old := capability.ListModelsForTests
	capability.ListModelsForTests = func(context.Context, string, string, string, []string) ([]string, error) {
		return nil, adapter.ModelsError(adapter.ErrModelsRefused, "claude refused list_models at /Users/someone", nil)
	}
	// What earlier tests in this process were answered is kept for minutes,
	// and the stand-in they had failed differently.
	capability.ForgetModels("claude")
	t.Cleanup(func() {
		capability.ListModelsForTests = old
		capability.ForgetModels("claude")
	})
	m := newMachine(t, claudeE2E)
	d := m.daemon()

	var st control.Status
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, out, _ := m.p.yad("", "status", "--json")
		// Ready too, so the daemon is past opening its store before the
		// test ends and stops it.
		if code == 0 && json.Unmarshal([]byte(out), &st) == nil && st.Ready && len(st.ModelsFailures) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never reported a failed ask:\n%s", d.out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	f := st.ModelsFailures[0]
	if f.Harness != "claude" || f.Account != "" || !strings.Contains(f.Reason, "refused list_models") || strings.Contains(f.Reason, "someone") {
		t.Errorf("status's failure = %+v, want claude's own login refusing, in yad's words", f)
	}

	code, out, errs := m.p.yad("", "doctor")
	if code != 0 {
		t.Fatalf("doctor exited %d: %s", code, errs)
	}
	for _, want := range []string{
		"warning: Claude Code — the daemon could not ask it which models it offers",
		"so hubs are sent the last list it gave or the catalog's: claude refused list_models, as a Claude Code older than the request does",
		"`\"$YAD_CLAUDE_PATH\" update`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "someone") {
		t.Errorf("doctor quotes the harness's error:\n%s", out)
	}
	if n := strings.Count(d.out.String(), "a harness did not say which models it offers"); n != 1 {
		t.Errorf("the daemon warned %d times, want once:\n%s", n, d.out.String())
	}
}

// A daemon that holds the lock and does not answer is said to, so a doctor
// with no model warnings is not read as a daemon that had none.
func TestDoctorSaysWhenTheDaemonDidNotAnswer(t *testing.T) {
	l := newLifecycle(t)
	pid := l.spawnWedged(false)
	t.Cleanup(func() {
		syscall.Kill(pid, syscall.SIGKILL)
		eventuallyTrue(func() bool { return !alive(pid) })
	})
	out := l.ok("doctor")
	if !strings.Contains(out, "note: the daemon did not answer, so why a harness's models may be the catalog's is not shown") {
		t.Errorf("doctor with a wedged daemon:\n%s", out)
	}
}
