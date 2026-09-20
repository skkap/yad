package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hub"
	"github.com/skkap/yad/internal/hub/store"
)

// connectedProfile is a runner profile registered with a hub of its own, and
// the hub's URL.
func connectedProfile(t *testing.T, name string) (*profile, string) {
	t.Helper()
	hubSide, runnerSide := newProfile(t), newProfile(t)
	code, tok, errs := hubSide.yad("", "hub", "token", "create", "--ttl", "10m")
	if code != 0 {
		t.Fatalf("token create: exit %d: %s", code, errs)
	}
	s, err := store.Open(context.Background(), filepath.Join(hubSide.data, "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	srv := httptest.NewServer(hub.New(hub.Options{Store: s}))
	t.Cleanup(srv.Close)
	url := srv.URL + hub.BasePath
	if code, _, errs := runnerSide.yad("", "connect", url, "--token", strings.TrimSpace(tok), "--name", name); code != 0 {
		t.Fatalf("connect: exit %d: %s", code, errs)
	}
	return runnerSide, url
}

// The operator's path: connect, then disconnect, and nothing of that hub is
// left on the machine.
func TestDisconnectRemovesTheCredentialAndTheConnection(t *testing.T) {
	p, url := connectedProfile(t, "home")
	cred, err := config.Paths{Config: p.config}.Credential("home")
	if err != nil {
		t.Fatal(err)
	}

	code, out, errs := p.yad("", "disconnect", "home")
	if code != 0 {
		t.Fatalf("disconnect: exit %d: %s", code, errs)
	}
	if !strings.Contains(out, "credential removed") || !strings.Contains(out, url) {
		t.Errorf("disconnect said %q", out)
	}
	if strings.Contains(out+errs, cred) {
		t.Error("disconnect printed the credential")
	}
	if _, err := os.Stat(filepath.Join(p.config, "credentials", "home")); err == nil {
		t.Error("the credential file is still there")
	}
	cfg, err := config.Load(config.Paths{Config: p.config})
	if err != nil || len(cfg.Connections) != 0 {
		t.Errorf("connections = %+v, %v", cfg.Connections, err)
	}
	// It says what the operator is left with: a runner with no hub.
	if !strings.Contains(out, "no connections left") {
		t.Errorf("disconnect did not say the runner has no hub left: %q", out)
	}
	// And what it did not do. With no daemon running, nothing closed that
	// hub's sessions here, and saying "credential removed" and stopping would
	// leave the operator to find the workdirs themselves.
	if !strings.Contains(out, "no daemon is running") || !strings.Contains(out, "yad sessions") {
		t.Errorf("disconnect did not say the sessions were left behind: %q", out)
	}
	// What the hub did with the work it held is the operator's business too.
	if !strings.Contains(out, "sessions on this runner are closed") {
		t.Errorf("disconnect did not say what became of the work at the hub: %q", out)
	}
	// And again: there is nothing by that name any more.
	if code, _, errs := p.yad("", "disconnect", "home"); code == 0 || !strings.Contains(errs, "none at all") {
		t.Errorf("second disconnect: exit %d %q", code, errs)
	}
}

func TestDisconnectUsage(t *testing.T) {
	p := newProfile(t)
	for _, args := range [][]string{{"disconnect"}, {"disconnect", "one", "two"}} {
		if code, _, errs := p.yad("", args...); code == 0 || !strings.Contains(errs, "usage") {
			t.Errorf("%v: exit %d %q", args, code, errs)
		}
	}
}
