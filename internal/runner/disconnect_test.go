package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubclient"
)

// connected registers this profile with the env's hub under name.
func connected(t *testing.T, e *env, name string) config.Connection {
	t.Helper()
	noTools(t)
	conn, _, _, err := Connect(context.Background(), e.paths, e.url, e.token(t), name)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func credentialExists(t *testing.T, p config.Paths, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(p.Config, "credentials", name))
	return err == nil
}

func connectionNames(t *testing.T, p config.Paths) []string {
	t.Helper()
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range cfg.Connections {
		out = append(out, c.Name)
	}
	return out
}

// The round trip: the hub retires the registration, the credential and the
// config entry go, and the credential no longer syncs anywhere.
func TestDisconnectRoundTrip(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	connected(t, e, "home")
	cred, err := e.paths.Credential("home")
	if err != nil {
		t.Fatal(err)
	}

	stopped := 0
	out, notes, err := Disconnect(ctx, e.paths, "home", false, func(context.Context) error {
		// Called before anything is removed: the daemon's side of it.
		if !credentialExists(t, e.paths, "home") {
			t.Error("the credential was removed before the daemon was told")
		}
		stopped++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Deregistered || out.Forced != nil || out.Remaining != 0 || stopped != 1 {
		t.Errorf("out %+v, notes %v, stopped %d", out, notes, stopped)
	}
	if len(notes) != 0 {
		t.Errorf("notes on a clean disconnect: %v", notes)
	}
	if credentialExists(t, e.paths, "home") {
		t.Error("the credential is still on disk")
	}
	if names := connectionNames(t, e.paths); len(names) != 0 {
		t.Errorf("config still lists %v", names)
	}
	// The hub has forgotten this runner: its credential opens nothing.
	id, _ := e.paths.RunnerID()
	c, _ := hubclient.New(e.url, cred)
	if _, err := c.Sync(ctx, id, v1.SyncRequest{RunnerID: id}); err == nil {
		t.Error("the deregistered credential still syncs")
	}
}

// Only the named connection goes.
func TestDisconnectLeavesTheOtherConnections(t *testing.T) {
	e := newEnv(t)
	other := newEnv(t)
	// Both hubs register this same profile, as a multi-homed runner does.
	connected(t, e, "one")
	noTools(t)
	if _, _, _, err := Connect(context.Background(), e.paths, other.url, other.token(t), "two"); err != nil {
		t.Fatal(err)
	}
	// The second hub issues its tokens against its own store, so the profile
	// holds a credential per connection.
	if _, err := e.paths.Credential("two"); err != nil {
		t.Fatal(err)
	}
	out, _, err := Disconnect(context.Background(), e.paths, "one", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Remaining != 1 {
		t.Errorf("remaining = %d, want 1", out.Remaining)
	}
	if names := connectionNames(t, e.paths); len(names) != 1 || names[0] != "two" {
		t.Errorf("connections left = %v, want [two]", names)
	}
	if !credentialExists(t, e.paths, "two") {
		t.Error("the other connection's credential went too")
	}
}

// A hub that will not answer keeps both: removing a credential cannot be
// undone from here, and the error says how to go ahead anyway.
func TestDisconnectKeepsEverythingWhenTheHubWillNotAnswer(t *testing.T) {
	e := newEnv(t)
	connected(t, e, "home")
	broken := brokenHub(t, e, http.StatusBadGateway)

	stopped := 0
	_, _, err := Disconnect(context.Background(), broken, "home", false, func(context.Context) error {
		stopped++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), "nothing was removed") {
		t.Fatalf("error = %v, want a refusal naming --force", err)
	}
	if stopped != 0 {
		t.Error("the daemon was told to stop a connection that is still registered")
	}
	if !credentialExists(t, broken, "home") {
		t.Error("the credential was removed although the hub never answered")
	}
	if names := connectionNames(t, broken); len(names) != 1 {
		t.Errorf("connections = %v, want the one that was kept", names)
	}

	// --force is the way through, and says the hub still holds a registration.
	out, _, err := Disconnect(context.Background(), broken, "home", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Deregistered || out.Forced == nil {
		t.Errorf("out = %+v, want a forced removal carrying the hub's refusal", out)
	}
	if credentialExists(t, broken, "home") || len(connectionNames(t, broken)) != 0 {
		t.Error("--force left something behind")
	}
}

// A hub that has already forgotten this runner has no registration to retire,
// so the removal goes ahead without --force.
func TestDisconnectProceedsWhenTheHubHasForgottenTheRunner(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			e := newEnv(t)
			connected(t, e, "home")
			gone := brokenHub(t, e, status)

			out, notes, err := Disconnect(context.Background(), gone, "home", false, nil)
			if err != nil {
				t.Fatal(err)
			}
			if out.Deregistered || out.Forced != nil {
				t.Errorf("out = %+v, want nothing retired and nothing forced", out)
			}
			if len(notes) != 1 || !strings.Contains(notes[0], "already forgotten") {
				t.Errorf("notes = %v", notes)
			}
			if credentialExists(t, gone, "home") || len(connectionNames(t, gone)) != 0 {
				t.Error("a hub that does not know this runner left the connection in place")
			}
		})
	}
}

// A credential that is not there is nothing to retire: the connection still
// goes, and the note says what is left to do at the hub.
func TestDisconnectWithoutACredential(t *testing.T) {
	e := newEnv(t)
	connected(t, e, "home")
	if err := e.paths.DeleteCredential("home"); err != nil {
		t.Fatal(err)
	}
	out, notes, err := Disconnect(context.Background(), e.paths, "home", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Deregistered {
		t.Error("a hub was told with no credential to tell it with")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "retire this runner at the hub itself") {
		t.Errorf("notes = %v", notes)
	}
	if len(connectionNames(t, e.paths)) != 0 {
		t.Error("the connection was kept")
	}
}

// A name nothing is connected to names the ones that are.
func TestDisconnectUnknownConnection(t *testing.T) {
	e := newEnv(t)
	if _, _, err := Disconnect(context.Background(), e.paths, "nowhere", false, nil); err == nil || !strings.Contains(err.Error(), "none at all") {
		t.Errorf("with no connections: %v", err)
	}
	connected(t, e, "home")
	_, _, err := Disconnect(context.Background(), e.paths, "nowhere", false, nil)
	if err == nil || !strings.Contains(err.Error(), "home") {
		t.Errorf("error = %v, want the connections it does have", err)
	}
	if len(connectionNames(t, e.paths)) != 1 {
		t.Error("a disconnect that named nothing removed something")
	}
}

// The daemon failing to let the connection go is a note, never a failure: by
// then the hub has retired the registration, and a credential kept for a hub
// that has forgotten it is a secret nobody would ever clean up.
func TestDisconnectGoesOnWhenTheDaemonCannotBeTold(t *testing.T) {
	e := newEnv(t)
	connected(t, e, "home")
	out, notes, err := Disconnect(context.Background(), e.paths, "home", false, func(context.Context) error {
		return errors.New("the daemon does not answer")
	})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Deregistered {
		t.Error("the hub was not told")
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "yad daemon restart") {
		t.Errorf("notes = %v, want one naming the way to end the connection now", notes)
	}
	if credentialExists(t, e.paths, "home") || len(connectionNames(t, e.paths)) != 0 {
		t.Error("nothing was removed")
	}
}

// brokenHub is the profile with its connection pointed at a hub that answers
// status to every deregistration. The credential and the config entry are the
// ones the real hub issued.
func brokenHub(t *testing.T, e *env, status int) config.Paths {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v1.Error{Code: "unavailable", Message: "no", NextAction: "try later"})
	}))
	t.Cleanup(srv.Close)
	cfg, err := config.Load(e.paths)
	if err != nil {
		t.Fatal(err)
	}
	for i := range cfg.Connections {
		cfg.Connections[i].URL = srv.URL
	}
	if err := config.Save(e.paths, cfg); err != nil {
		t.Fatal(err)
	}
	return e.paths
}

// A forced disconnect leaves the registration live at the hub, so an error
// after that point must not tell the owner the credential is dead — that is
// how a working credential is left at a hub nobody will ever retire it at.
func TestAForcedDisconnectSaysTheRunnerIsStillRegistered(t *testing.T) {
	e := newEnv(t)
	connected(t, e, "home")
	broken := brokenHub(t, e, http.StatusBadGateway)
	// The credential cannot be removed: its directory is not writable.
	dir := filepath.Join(broken.Config, "credentials")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	_, _, err := Disconnect(context.Background(), broken, "home", true, nil)
	if err == nil {
		t.Fatal("the credential could not be removed and no error said so")
	}
	if !strings.Contains(err.Error(), "still registered") {
		t.Errorf("error = %v, want it to say the hub still holds the registration", err)
	}
	if !strings.Contains(err.Error(), "credential still works") {
		t.Errorf("error = %v, want it to say the credential is live and must be retired", err)
	}
}

// The same failure after a real deregistration says the opposite, because
// there the credential really is dead.
func TestADeregisteredCredentialThatWillNotDeleteSaysItIsDead(t *testing.T) {
	e := newEnv(t)
	connected(t, e, "home")
	dir := filepath.Join(e.paths.Config, "credentials")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	_, _, err := Disconnect(context.Background(), e.paths, "home", false, nil)
	if err == nil || !strings.Contains(err.Error(), "no longer accepted anywhere") {
		t.Errorf("error = %v, want it to say the credential is dead", err)
	}
	if strings.Contains(err.Error(), "still registered") {
		t.Errorf("error = %v, want it to say this runner is deregistered", err)
	}
}
