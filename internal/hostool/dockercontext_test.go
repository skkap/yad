package hostool

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// writeContext lays out a docker config directory as the docker CLI does:
// config.json naming the current context, and each context's meta.json
// under the SHA-256 of its name.
func writeContext(t *testing.T, dir, current string, hosts map[string]string) {
	t.Helper()
	if current != "" {
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"auths":{},"currentContext":"`+current+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, host := range hosts {
		sum := sha256.Sum256([]byte(name))
		meta := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
		if err := os.MkdirAll(meta, 0o700); err != nil {
			t.Fatal(err)
		}
		body := `{"Name":"` + name + `","Metadata":{},"Endpoints":{"docker":{"Host":"` + host + `","SkipTLSVerify":false}}}`
		if err := os.WriteFile(filepath.Join(meta, "meta.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOffDefaultDockerSocket(t *testing.T) {
	sockets := t.TempDir()
	colima := filepath.Join(sockets, "colima.sock")
	desktop := filepath.Join(sockets, "desktop.sock")
	if err := os.WriteFile(desktop, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The default socket is a link to Docker Desktop's, as Desktop makes it.
	def := filepath.Join(sockets, "default.sock")
	if err := os.Symlink(desktop, def); err != nil {
		t.Fatal(err)
	}
	old := DefaultDockerSocket
	DefaultDockerSocket = def
	t.Cleanup(func() { DefaultDockerSocket = old })

	hosts := map[string]string{
		"colima":        "unix://" + colima,
		"desktop-linux": "unix://" + desktop,
		"remote":        "tcp://10.0.0.2:2376",
		"plain":         "unix://" + def,
	}
	for _, tc := range []struct {
		name, current, envContext string
		malformed                 bool
		wantContext               string
	}{
		{name: "colima is the current context", current: "colima", wantContext: "colima"},
		{name: "DOCKER_CONTEXT wins over config.json", current: "default", envContext: "colima", wantContext: "colima"},
		{name: "the default context", current: "default"},
		{name: "no current context", current: ""},
		{name: "Docker Desktop, whose socket the default links to", current: "desktop-linux"},
		{name: "a context over TCP", current: "remote"},
		{name: "a context naming the default socket", current: "plain"},
		{name: "a context with no meta.json", current: "gone"},
		{name: "a config.json docker would not read", current: "colima", malformed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeContext(t, dir, tc.current, hosts)
			if tc.malformed {
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := map[string]string{"DOCKER_CONFIG": dir, "DOCKER_CONTEXT": tc.envContext}
			ctx, socket, ok := OffDefaultDockerSocket(func(k string) string { return env[k] })
			if ok != (tc.wantContext != "") || ctx != tc.wantContext {
				t.Fatalf("got %q %q %v, want context %q", ctx, socket, ok, tc.wantContext)
			}
			if ok && socket != colima {
				t.Errorf("socket %q, want %q", socket, colima)
			}
		})
	}
}
