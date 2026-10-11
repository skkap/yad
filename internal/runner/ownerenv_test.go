package runner

import (
	"context"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/store/db"
	"github.com/skkap/yad/internal/supervise"
)

// ownerEnv sets the owner's [env] layer for one test.
func ownerEnv(t *testing.T, env map[string]string) {
	t.Helper()
	t.Cleanup(func() { supervise.SetOwnerEnv(nil) })
	supervise.SetOwnerEnv(env)
}

// A grant named for a variable the owner's [env] declares is refused at the
// claim, naming that variable and the way round it, and never quoting the
// owner's value or naming what else [env] declares. A grant under any other
// name is taken as before (DEV-188).
func TestRefusesAGrantNamedForAnOwnerVariable(t *testing.T) {
	const ownerValue = "unix:///owner/colima.sock"
	ownerEnv(t, map[string]string{"DOCKER_HOST": ownerValue, "BASH_MAX_TIMEOUT_MS": "1800000"})
	e := newEnv(t)
	clash := testRun("clash", "s-clash")
	clash.Grants = []v1.Grant{{Name: "ZUMINO_TOKEN", Value: "t", As: v1.GrantEnv}, {Name: "DOCKER_HOST", Value: "tcp://hub-chosen:2375", As: v1.GrantEnv}}
	asFile := testRun("as-file", "s-file")
	asFile.Grants = []v1.Grant{{Name: "DOCKER_HOST", Value: "x", As: v1.GrantFile}}
	// Another variable on linux and darwin: the environment is
	// case-sensitive, and this one leaves the owner's alone.
	otherCase := testRun("other-case", "s-case")
	otherCase.Grants = []v1.Grant{{Name: "docker_host", Value: "x", As: v1.GrantEnv}}
	h := &scriptedHub{offer: []v1.Run{clash, asFile, otherCase}}
	doc := drivableDoc("r", 3)
	l := &Loop{Connection: "hub", RunnerID: "r", Hub: h, Store: e.store, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Executor: e.exec, Clock: e.clock}

	mustSync(t, l)
	for _, id := range []string{"clash", "as-file"} {
		r, ok := h.results[id]
		if !ok || r.State != v1.RunFailed || r.Error == nil || r.Error.Class != ClassRefused {
			t.Fatalf("%s: result %+v, want a refusal", id, r)
		}
		msg := r.Error.Message
		if !strings.Contains(msg, "grant DOCKER_HOST") || !strings.Contains(msg, "[env]") || !strings.Contains(msg, "under another name") {
			t.Errorf("%s: refusal %q does not name the variable, [env] and the way round it", id, msg)
		}
		if strings.Contains(msg, ownerValue) || strings.Contains(msg, "BASH_MAX_TIMEOUT_MS") || strings.Contains(msg, "hub-chosen") {
			t.Errorf("%s: refusal %q quotes a value or another declared name", id, msg)
		}
	}
	if _, ok := h.results["other-case"]; ok {
		t.Error("a grant whose name differs from a declared one by case was refused")
	}
}

// A run claimed before the owner declared the variable — parked across a
// restart, picked up by a process whose config.toml now declares it — is
// refused where its grants would become variables, before anything is
// delivered or started.
func TestExecutorRefusesAGrantNamedForAnOwnerVariable(t *testing.T) {
	e := newEnv(t)
	ad := fakeHarness(fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded}})
	x := e.executor(ad)
	ctx := context.Background()
	if err := e.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "s1", Harness: "claude"}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.CreateRun(ctx, db.CreateRunParams{Connection: "hub", ID: "a", SessionID: "s1", Harness: "claude", Spec: "{}"}); err != nil {
		t.Fatal(err)
	}
	ownerEnv(t, map[string]string{"BASH_MAX_TIMEOUT_MS": "1800000"})
	run := testRun("a", "s1")
	run.Grants = []v1.Grant{{Name: "BASH_MAX_TIMEOUT_MS", Value: "60000", As: v1.GrantEnv}}
	released := false
	x.Start(ctx, Claim{Connection: "hub", Run: run, Release: func() { released = true }})
	x.Wait()
	res, ok := outboxResult(t, e, "a")
	if !ok || res.State != v1.RunFailed || res.Error.Class != ClassRefused || !strings.Contains(res.Error.Message, "grant BASH_MAX_TIMEOUT_MS") {
		t.Fatalf("result %+v %+v, %v", res, res.Error, ok)
	}
	if len(ad.Starts) != 0 || !released {
		t.Errorf("starts %d, released %v", len(ad.Starts), released)
	}
}
