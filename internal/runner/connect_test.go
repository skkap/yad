package runner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubclient"

	v1 "github.com/skkap/yad/protocol/v1"
)

// The round trip: a token from the hub, yad connect, and a runner that can
// sync with what connect wrote.
func TestConnectRoundTrip(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	e := newEnv(t)
	ctx := context.Background()
	tok := e.token(t)
	conn, res, _, err := Connect(ctx, e.paths, e.url, tok, "home")
	if err != nil {
		t.Fatal(err)
	}
	if conn.Name != "home" || conn.URL != e.url || res.SyncIntervalMS != 15000 {
		t.Errorf("conn %+v res %+v", conn, res)
	}

	cfg, err := config.Load(e.paths)
	if err != nil || len(cfg.Connections) != 1 || cfg.Connections[0] != conn {
		t.Fatalf("config = %+v, %v", cfg.Connections, err)
	}
	raw, err := os.ReadFile(e.paths.ConfigFile())
	if err != nil || strings.Contains(string(raw), res.RunnerCredential) || strings.Contains(string(raw), tok) {
		t.Error("config.toml holds a secret")
	}
	fi, err := os.Stat(filepath.Join(e.paths.Config, "credentials", "home"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("credential file: %v %v", fi, err)
	}
	cred, err := e.paths.Credential("home")
	if err != nil || cred != res.RunnerCredential {
		t.Fatalf("stored credential differs: %v", err)
	}
	id, _ := e.paths.RunnerID()
	c, _ := hubclient.New(e.url, cred)
	if _, err := c.Sync(ctx, id, v1.SyncRequest{RunnerID: id}); err != nil {
		t.Errorf("sync with the stored credential: %v", err)
	}

	// The token is spent: a second connect with it is refused, with the way on.
	_, _, _, err = Connect(ctx, e.paths, e.url, tok, "home")
	if err == nil || !strings.Contains(err.Error(), "already used") || !strings.Contains(err.Error(), "yad hub token create") {
		t.Errorf("reused token: %v", err)
	}
	// A token for a new runner cannot replace this one's credential; a token
	// issued for it can, under the same name.
	if _, _, _, err := Connect(ctx, e.paths, e.url, e.token(t), "home"); err == nil || !strings.Contains(err.Error(), "--runner "+id) {
		t.Errorf("re-connect with a new-runner token: %v", err)
	}
	if _, res2, _, err := Connect(ctx, e.paths, e.url, e.token(t, id), "home"); err != nil || res2.RunnerCredential == res.RunnerCredential {
		t.Errorf("re-connect: %v", err)
	}
	if cfg, _ := config.Load(e.paths); len(cfg.Connections) != 1 {
		t.Errorf("re-connect duplicated the connection: %+v", cfg.Connections)
	}
}

func TestConnectRefusals(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	e := newEnv(t)
	ctx := context.Background()
	if _, _, _, err := Connect(ctx, e.paths, e.url, e.token(t), "home"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, url, token, conn, want string
	}{
		{"no token", e.url, "  ", "x", "no registration token"},
		{"cleartext remote", "http://hub.example/v1", "t", "x", "cleartext"},
		{"name taken by another hub", "https://other.example/v1", "t", "home", "already points at"},
		{"hub already connected", e.url, "t", "second", "already connected"},
		{"bad name", "https://other.example/v1", "t", "Bad Name", "lowercase"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := Connect(ctx, e.paths, tc.url, tc.token, tc.conn)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// None of the refusals touched the network or the config.
	if cfg, _ := config.Load(e.paths); len(cfg.Connections) != 1 {
		t.Errorf("connections = %+v", cfg.Connections)
	}
}

func TestNameFromURL(t *testing.T) {
	for raw, want := range map[string]string{
		"https://zumino.cc/api/yad/v1":        "zumino-cc",
		"http://127.0.0.1:7878/v1":            "127-0-0-1",
		"https://Ashikaga.tail.ts.net/yad/v1": "ashikaga-tail-ts-net",
		"https://[::1]:8/v1":                  "1",
		"%":                                   "hub",
	} {
		if got := NameFromURL(raw); got != want {
			t.Errorf("NameFromURL(%q) = %q, want %q", raw, got, want)
		}
	}
}
