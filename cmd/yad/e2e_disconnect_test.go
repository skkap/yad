package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/hub"
	hubstore "github.com/skkap/yad/internal/hub/store"
)

// alsoConnect registers the machine's runner with a second hub of its own, as
// a multi-homed runner is (decision 0003), and returns that hub's store.
func alsoConnect(t *testing.T, m *machine, name string) *hubstore.Store {
	t.Helper()
	ctx := context.Background()
	s, err := hubstore.Open(ctx, filepath.Join(t.TempDir(), name+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	srv := httptest.NewServer(hub.New(hub.Options{Store: s, SyncInterval: testSyncInterval}))
	t.Cleanup(srv.Close)
	tok, _, err := hub.IssueRegistrationToken(ctx, s, time.Hour, time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	m.ok("connect", srv.URL+hub.BasePath, "--token", tok, "--name", name)
	return s
}

// Disconnecting, end to end through the commands an operator types. The runner
// is on two hubs; a run has been and gone on the first, leaving a session and
// its workdir. `yad disconnect` retires the runner at that hub, the daemon
// lets the connection go and closes its sessions, the workdir is reclaimed,
// the credential and the connection leave the machine — and the other hub
// carries on. Nothing here turns on which harness ran, so it runs against one.
func TestE2EDisconnect(t *testing.T) {
	h := e2eHarnesses[0]
	m := newMachine(t, h)
	spare := alsoConnect(t, m, "spare")
	d := m.daemon()
	ctx := context.Background()

	m.ok(m.submitArgs("--run-id", "e2e-bye", "--new-session", "e2e-bye-session", h.instruction)...)
	if code, out, errs := m.watch("e2e-bye"); code != 0 {
		t.Fatalf("watch exit %d: %s\n%s\ndaemon:\n%s", code, errs, out, d.out.String())
	}
	var list []session
	if err := json.Unmarshal([]byte(m.ok("sessions", "--json")), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Workdir == "" {
		t.Fatalf("sessions = %+v", list)
	}
	workdir := list[0].Workdir
	if _, err := os.Stat(workdir); err != nil {
		t.Fatalf("the session has no workdir: %v", err)
	}

	out := m.ok("disconnect", "home")
	if !strings.Contains(out, "credential removed") || !strings.Contains(out, "1 session(s)") {
		t.Errorf("disconnect said %q", out)
	}
	if strings.Contains(out, "no connections left") {
		t.Errorf("disconnect said the runner has no hub, with one still connected: %q", out)
	}
	// The workdir goes with the session: that hub is gone, so nothing will
	// ever continue it, and nothing else would close it (0011, 0035).
	eventually(t, "the workdir "+workdir+" is reclaimed", func() bool {
		_, err := os.Stat(workdir)
		return os.IsNotExist(err)
	})
	// Nothing of that hub is left on the machine, and the other is untouched.
	if _, err := (config.Paths{Config: m.p.config}).Credential("home"); err == nil {
		t.Error("the credential is still readable")
	}
	cfg, err := config.Load(config.Paths{Config: m.p.config})
	if err != nil || len(cfg.Connections) != 1 || cfg.Connections[0].Name != "spare" {
		t.Fatalf("connections = %+v, %v", cfg.Connections, err)
	}
	if _, err := (config.Paths{Config: m.p.config}).Credential("spare"); err != nil {
		t.Errorf("the other connection's credential went too: %v", err)
	}
	// The hub let the runner go without forgetting it: the row stays, so its
	// sessions and history keep their reference and `yad connect` with a token
	// for that id is the way back. The credential is what died.
	id, err := (config.Paths{Config: m.p.config}).RunnerID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.hubDB.GetRunner(ctx, id); err != nil {
		t.Errorf("the hub dropped the runner row: %v", err)
	}

	// The runner is still up and still syncing the hub it kept.
	eventually(t, "the spare hub is still being synced", func() bool {
		r, err := spare.GetRunner(ctx, id)
		return err == nil && r.LastSyncAt.Valid
	})
	var st control.Status
	if err := json.Unmarshal([]byte(m.ok("status", "--json")), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Connections) != 1 || st.Connections[0].Name != "spare" {
		t.Errorf("status shows %+v", st.Connections)
	}
}

// A run held when its hub is disconnected is lost on the hub's side, and left
// running here: the owner is told, rather than having the work killed under
// them.
func TestE2EDisconnectWithARunHeld(t *testing.T) {
	h := e2eHarnesses[0]
	m := newMachine(t, h)
	m.gated()
	d := m.daemon()
	ctx := context.Background()

	m.submit("e2e-held")
	m.waitAtGate()

	out := m.ok("disconnect", "home")
	if !strings.Contains(out, "1 run(s)") || !strings.Contains(out, "nowhere to go") {
		t.Errorf("disconnect said %q, want it to say the run held has nowhere to report", out)
	}
	eventually(t, "the hub has the run it held as lost", func() bool {
		r, err := m.hubDB.GetRun(ctx, "e2e-held")
		return err == nil && r.State == string(v1.RunLost)
	})
	// Let the harness finish; the daemon ends with it, having no hub left.
	m.open()
	select {
	case code := <-d.done:
		if code != 0 {
			t.Errorf("daemon exited %d:\n%s", code, d.out.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the daemon kept running with no connection left:\n%s", d.out.String())
	}
	// Its exit is taken here, so the cleanup does not wait on a process that
	// has already stopped.
	d.once.Do(func() {})
}
