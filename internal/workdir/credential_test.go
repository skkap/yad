//go:build unix

package workdir

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"
)

// credServer is a repository served by git's own smart-http backend on a
// loopback TLS listener, in process, that answers only the one credential.
type credServer struct {
	srv        *httptest.Server
	mu         sync.Mutex
	authorized int // requests that carried the credential
}

func newCredServer(t *testing.T, root, user, password string) *credServer {
	t.Helper()
	execPath := sh(t, "", "git", "--exec-path")
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend is not installed")
	}
	cs := &credServer{}
	h := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1"}}
	cs.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="repo"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		cs.mu.Lock()
		cs.authorized++
		cs.mu.Unlock()
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(cs.srv.Close)
	t.Setenv("GIT_SSL_NO_VERIFY", "true")
	return cs
}

func (cs *credServer) served() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.authorized
}

// argvLog puts a git in front of the real one that writes every command's
// argv to a file, one line each, so a test can read what reached argv.
func argvLog(t *testing.T, m *Manager) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	wrapper := filepath.Join(dir, "git")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	m.Git = wrapper
	return log
}

// holding lists every file under dir whose contents or name hold s.
func holding(t *testing.T, dir, s string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(path, s) {
			found = append(found, path)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), s) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// An https URL's userinfo is the run's credential (decision 0068): the
// repository behind it is fetched with it, and nothing that outlives the
// run, or that another user on the machine can read, holds it — not git's
// argv, not the cache's config or name, not a file under the data directory,
// not the run's events, and not the setup hook's environment. The owner's
// own credential helper neither answers for the host nor is asked to store
// what worked. A run from the same repository without the credential shares
// the cache and is not given it.
func TestAURLsCredentialIsTheRunsAlone(t *testing.T) {
	hook := "#!/bin/sh\nenv > \"$WT_ROOT/hook.env\"\n"
	for _, tc := range []struct {
		name, userinfo, user, password string
	}{
		{"a token as the user", fakeToken, fakeToken, ""},
		{"a user and a password", "someone:" + fakeToken, "someone", fakeToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			served := t.TempDir()
			newOrigin(t, served, "acme", map[string]string{hookPath: hook, "README": "v1\n"})
			cs := newCredServer(t, served, tc.user, tc.password)

			// The owner's helper holds a wrong credential for the host: had it
			// been asked first, the fetch would fail.
			store := filepath.Join(t.TempDir(), "credentials")
			host := strings.TrimPrefix(cs.srv.URL, "https://")
			if err := os.WriteFile(store, []byte("https://wrong:wrong@"+host+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			global := filepath.Join(t.TempDir(), "gitconfig")
			if err := os.WriteFile(global, []byte("[credential]\n\thelper = store --file "+store+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_CONFIG_GLOBAL", global)
			argv := argvLog(t, f.m)

			url := "https://" + tc.userinfo + "@" + host + "/acme.git"
			bare := cs.srv.URL + "/acme.git"
			p, ev, err := f.prepare("s1", gitSource(url, "", ""))
			if err != nil {
				t.Fatalf("prepare: %v", err)
			}
			if cs.served() == 0 {
				t.Fatal("the server never saw the credential")
			}
			if got := read(t, filepath.Join(p.Dir, "README")); got != "v1\n" {
				t.Errorf("README = %q", got)
			}
			// The session continues: nothing is fetched, nothing is kept.
			if _, _, err := f.prepare("s1", gitSource(url, "", "")); err != nil {
				t.Fatalf("second run: %v", err)
			}

			caches, _ := filepath.Glob(filepath.Join(f.m.Data, "repos", "*.git"))
			if len(caches) != 1 {
				t.Fatalf("caches = %v, want one", caches)
			}
			if got := sh(t, caches[0], "git", "config", "--get", "remote.origin.url"); got != bare {
				t.Errorf("the cache's remote.origin.url = %q, want %q", got, bare)
			}
			if b := filepath.Base(caches[0]); b != "acme-"+digest(bare)+".git" {
				t.Errorf("the cache is %s, want it keyed by the URL without its userinfo", b)
			}
			for _, where := range []struct{ name, dir string }{{"the data directory", f.m.Data}, {"the owner's credential store", filepath.Dir(store)}} {
				if found := holding(t, where.dir, "FAKEt0ken"); len(found) > 0 {
					t.Errorf("%s holds the token: %v", where.name, found)
				}
			}
			if log := read(t, argv); strings.Contains(log, "FAKEt0ken") {
				t.Errorf("git's argv held the token:\n%s", log)
			}
			for _, e := range ev.evs {
				if strings.Contains(e.Status, "FAKEt0ken") {
					t.Errorf("an event holds the token: %q", e.Status)
				}
			}
			if env := read(t, filepath.Join(p.Dir, "hook.env")); strings.Contains(env, "FAKEt0ken") || strings.Contains(env, "YAD_GIT_") || strings.Contains(env, "GIT_CONFIG_COUNT") {
				t.Errorf("the setup hook was handed the credential:\n%s", env)
			}

			// Without it, the same repository is fetched into the same cache,
			// and the credential of the run before is not offered.
			before := cs.served()
			_, _, err = f.prepare("s2", gitSource(bare, "", ""))
			if class(err) != ClassSourceFailed {
				t.Errorf("a run without the credential: err = %v, want %s", err, ClassSourceFailed)
			}
			if cs.served() != before {
				t.Error("a run without the credential was fetched with the one before")
			}
			if again, _ := filepath.Glob(filepath.Join(f.m.Data, "repos", "*.git")); len(again) != 1 {
				t.Errorf("caches = %v, want the one shared", again)
			}
		})
	}
}

// A wrong credential fails the fetch with the class and a next action that
// point at the hub's credential, not the machine's.
func TestAWrongCredentialNamesTheHubs(t *testing.T) {
	f := newFixture(t)
	served := t.TempDir()
	newOrigin(t, served, "acme", nil)
	cs := newCredServer(t, served, "right", "")
	host := strings.TrimPrefix(cs.srv.URL, "https://")
	for i, tc := range []struct{ userinfo, want string }{
		{fakeToken, "offered as a token"},
		{"someone:" + fakeToken, "the URL's credential"},
	} {
		_, _, err := f.prepare(fmt.Sprintf("s%d", i), gitSource("https://"+tc.userinfo+"@"+host+"/acme.git", "", ""))
		if class(err) != ClassSourceFailed || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "FAKEt0ken") {
			t.Errorf("%s: err = %v", tc.userinfo, err)
		}
	}
}

// A user alone in the URL may be the account's name rather than a token —
// Azure DevOps and Bitbucket hand out clone URLs with it — so when the remote
// refuses it as a token, the owner's own helper is asked for that name, as it
// was when git was given the URL whole. What it stores is its own credential,
// and the name still reaches git only through the environment.
func TestAUserAloneFallsBackToTheOwnersHelper(t *testing.T) {
	f := newFixture(t)
	served := t.TempDir()
	newOrigin(t, served, "acme", map[string]string{"README": "v1\n"})
	cs := newCredServer(t, served, "alice", "owners-secret")
	host := strings.TrimPrefix(cs.srv.URL, "https://")
	store := filepath.Join(t.TempDir(), "credentials")
	stored := "https://alice:owners-secret@" + host + "\n"
	if err := os.WriteFile(store, []byte(stored), 0o600); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(global, []byte("[credential]\n\thelper = store --file "+store+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	argv := argvLog(t, f.m)

	p, _, err := f.prepare("s1", gitSource("https://alice@"+host+"/acme.git", "", ""))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got := read(t, filepath.Join(p.Dir, "README")); got != "v1\n" {
		t.Errorf("README = %q", got)
	}
	// The store helper writes back what worked, spelling the port its own way.
	if got := read(t, store); strings.Count(got, "\n") != 1 || !strings.HasPrefix(got, "https://alice:owners-secret@127.0.0.1") {
		t.Errorf("the owner's store now holds %q, want its one entry", got)
	}
	if log := read(t, argv); strings.Contains(log, "alice") {
		t.Errorf("git's argv held the URL's user:\n%s", log)
	}
}

// What is stored of a run's sources, and compared between a session's runs:
// an https URL loses its userinfo, an ssh URL keeps its user, which is the
// login name, and loses a password; nothing else changes spelling.
func TestStoredSources(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://github.com/skkap/yad.git", "https://github.com/skkap/yad.git"},
		{"https://" + fakeToken + "@github.com/skkap/yad", "https://github.com/skkap/yad"},
		{"HTTPS://u:" + fakeToken + "@github.com", "HTTPS://github.com"},
		{"https://u:" + fakeToken + "@github.com:8443/a?x=@#y", "https://github.com:8443/a?x=@#y"},
		{"https://a%40b:" + fakeToken + "@github.com/a%2Fb", "https://github.com/a%2Fb"},
		{"ssh://git@github.com/skkap/yad", "ssh://git@github.com/skkap/yad"},
		{"ssh://git:" + fakeToken + "@github.com/skkap/yad", "ssh://git@github.com/skkap/yad"},
		{"git@github.com:skkap/yad.git", "git@github.com:skkap/yad.git"},
		{"/home/someone/yad@v2", "/home/someone/yad@v2"},
		{"http://" + fakeToken + "@example.com/a", "http://example.com/a"},
	} {
		got := StoredSources([]v1.Source{gitSource(tc.raw, "main", ""), {Path: "/p"}})
		if got[0].Git.URL != tc.want || got[0].Git.Base != "main" || got[1].Path != "/p" {
			t.Errorf("StoredSources(%q) = %+v, want %q", tc.raw, got[0].Git, tc.want)
		}
		if carries := CarriesCredential([]v1.Source{gitSource(tc.raw, "", "")}); carries != (tc.raw != tc.want) {
			t.Errorf("CarriesCredential(%q) = %v", tc.raw, carries)
		}
	}
	if StoredSources(nil) != nil {
		t.Error("no sources stored as some")
	}
}
