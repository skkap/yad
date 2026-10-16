//go:build unix

package workdir

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/supervise"
)

// A probe answers with a class for each way a repository cannot be reached,
// and refuses unprobed what a run's git would refuse or a hub should not send.
func TestProbe(t *testing.T) {
	f := newFixture(t)
	newOrigin(t, f.root, "acme", map[string]string{"README": "v1\n"})
	repo := filepath.Join(f.root, "acme.git")
	outside := t.TempDir()
	newOrigin(t, outside, "elsewhere", map[string]string{"README": "v1\n"})

	// A port nothing listens on: refused at once, with no DNS to wait for.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().String()
	l.Close()

	for _, tc := range []struct {
		name, url, ref string
		reachable      bool
		class          v1.SourceReachError
	}{
		{"a repository in the roots, at HEAD", repo, "", true, ""},
		{"a branch that exists", "file://" + repo, "main", true, ""},
		{"a ref the repository lacks", repo, "no-such-branch", false, v1.ReachRefNotFound},
		{"a directory that is no repository", f.root, "", false, v1.ReachNotFound},
		{"a host nobody answers on", "https://" + closed + "/acme.git", "", false, v1.ReachNetwork},
		{"a token in the URL", "https://ghp_secret@" + closed + "/acme.git", "", false, v1.ReachRefused},
		{"a password in an ssh URL", "ssh://git:pw@" + closed + "/acme.git", "", false, v1.ReachRefused},
		{"a remote helper", "ext::sh -c touch% /tmp/pwned", "", false, v1.ReachRefused},
		{"plain http", "http://" + closed + "/acme.git", "", false, v1.ReachRefused},
		{"a path outside the roots", filepath.Join(outside, "elsewhere.git"), "", false, v1.ReachRefused},
		{"a ref git would read as an option", repo, "--upload-pack=touch", false, v1.ReachRefused},
		{"a ref with a space", repo, "a b", false, v1.ReachRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, class := f.m.Probe(context.Background(), tc.url, tc.ref)
			if ok != tc.reachable || class != tc.class {
				t.Errorf("Probe(%q, %q) = %v, %q; want %v, %q", tc.url, tc.ref, ok, class, tc.reachable, tc.class)
			}
		})
	}
}

// The probe is a run's git: a credential the owner gives git — here through
// [env], as a credential helper — is what reaches the remote, and the answer
// changes once the owner fixes it, which is the re-check a hub waits on.
func TestProbeUsesTheRunsCredentials(t *testing.T) {
	f := newFixture(t)
	served := t.TempDir()
	newOrigin(t, served, "acme", map[string]string{"README": "v1\n"})
	cs := newCredServer(t, served, "someone", fakeToken)
	url := cs.srv.URL + "/acme.git"

	if ok, class := f.m.Probe(context.Background(), url, ""); ok || class != v1.ReachAuth {
		t.Fatalf("with no credential: %v, %q; want unreachable, auth", ok, class)
	}

	store := filepath.Join(t.TempDir(), "credentials")
	host := strings.TrimPrefix(cs.srv.URL, "https://")
	if err := os.WriteFile(store, []byte("https://someone:"+fakeToken+"@"+host+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	supervise.SetOwnerEnv(map[string]string{
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "credential.helper", "GIT_CONFIG_VALUE_0": "store --file " + store,
	})
	t.Cleanup(func() { supervise.SetOwnerEnv(nil) })
	if ok, class := f.m.Probe(context.Background(), url, ""); !ok || class != "" {
		t.Fatalf("with the owner's credential: %v, %q; want reachable", ok, class)
	}
	if cs.served() == 0 {
		t.Fatal("the server never saw the credential")
	}
}

func TestProbeTimeout(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	f.m.ProbeTimeout = 300 * time.Millisecond
	if ok, class := f.m.Probe(context.Background(), srv.URL+"/acme.git", ""); ok || class != v1.ReachTimeout {
		t.Errorf("Probe = %v, %q; want unreachable, timeout", ok, class)
	}
	srv.CloseClientConnections()
}

// The words are matched under LC_ALL=C, which every git here runs with.
func TestReachClass(t *testing.T) {
	for stderr, want := range map[string]v1.SourceReachError{
		"git@github.com: Permission denied (publickey).\r\nfatal: Could not read from remote repository.": v1.ReachAuth,
		"fatal: could not read Username for 'https://github.com': terminal prompts disabled":              v1.ReachAuth,
		"remote: Invalid username or token.\nfatal: Authentication failed for 'https://github.com/a/b/'":  v1.ReachAuth,
		"remote: Repository not found.\nfatal: repository 'https://github.com/a/b/' not found":            v1.ReachNotFound,
		"ERROR: Repository not found.\nfatal: Could not read from remote repository.":                     v1.ReachNotFound,
		"fatal: unable to access 'https://x/': Could not resolve host: x":                                 v1.ReachNetwork,
		"ssh: Could not resolve hostname x: nodename nor servname provided":                               v1.ReachNetwork,
		"fatal: something nobody has seen":                                                                v1.ReachFailed,
	} {
		if got := reachClass(stderr); got != want {
			t.Errorf("reachClass(%q) = %q, want %q", stderr, got, want)
		}
	}
}
