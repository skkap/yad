package main

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/store/db"
)

// A run whose git source is an https URL carrying a token, end to end, twice
// in one session (decision 0067). The repository is served by git's own
// smart-http backend on a loopback TLS listener, in process, and answers only
// that token — so the fetch proves the token reached git. After both runs no
// byte of it is anywhere the runner keeps things: the data directory, which
// holds state.db, the daemon's log, the bare cache and the session's workdir;
// the daemon's own output; the events the hub received.
func TestE2EASourceURLsTokenIsNotKept(t *testing.T) { eachHarness(t, testE2EASourceURLsTokenIsNotKept) }

func testE2EASourceURLsTokenIsNotKept(t *testing.T, h *e2eHarness) {
	const token = "ghp_FAKEt0kenFAKEt0ken"
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	out, err := exec.Command(git, "--exec-path").Output()
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend is not installed")
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@example.com",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@example.com",
		// The listener's certificate is the test's own.
		"GIT_SSL_NO_VERIFY": "true",
	} {
		t.Setenv(k, v)
	}
	m := newMachine(t, h)
	if err := os.Symlink(git, filepath.Join(os.Getenv("PATH"), "git")); err != nil {
		t.Fatal(err)
	}

	served := t.TempDir()
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte("fetched with the token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--bare", "--initial-branch=main", filepath.Join(served, "acme.git")},
		{"-C", work, "init", "--quiet", "--initial-branch=main"},
		{"-C", work, "add", "-A"},
		{"-C", work, "commit", "--quiet", "-m", "initial"},
		{"-C", work, "push", "--quiet", filepath.Join(served, "acme.git"), "main"},
	} {
		if out, err := exec.Command(git, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	backendHandler := &cgi.Handler{Path: backend, Env: []string{"GIT_PROJECT_ROOT=" + served, "GIT_HTTP_EXPORT_ALL=1"}}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, _, ok := r.BasicAuth(); !ok || user != token {
			w.Header().Set("WWW-Authenticate", `Basic realm="acme"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		backendHandler.ServeHTTP(w, r)
	}))
	defer srv.Close()
	url := "https://" + token + "@" + strings.TrimPrefix(srv.URL, "https://") + "/acme.git"

	t.Setenv(h.starts, filepath.Join(t.TempDir(), "harness.starts"))
	t.Setenv(h.read, "note.txt")
	d := m.daemon()
	const answer = "fetched with the token"
	runOnce := func(runID string, extra ...string) {
		t.Helper()
		args := append([]string{"--run-id", runID, "--git", url}, extra...)
		m.ok(m.submitArgs(append(args, h.instruction)...)...)
		if code, watched, errs := m.watch(runID); code != 0 || !strings.Contains(watched, answer) {
			t.Fatalf("%s: watch exit %d: %s\n%s\ndaemon:\n%s", runID, code, errs, watched, d.out.String())
		}
	}
	runOnce("e2e-cred-1")
	session := localRun(t, m.runnerStore(), "e2e-cred-1").SessionID
	runOnce("e2e-cred-2", "--session", session)

	var held []string
	err = filepath.WalkDir(m.p.data, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// The hub's own store lives here in this test only; the hub was
		// sent the URL, and holding what it sent is its business.
		if strings.HasPrefix(e.Name(), "hub.db") || !e.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(path, "FAKEt0ken") || strings.Contains(string(b), "FAKEt0ken") {
			held = append(held, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(held) > 0 {
		t.Errorf("the runner kept the token in %v", held)
	}
	if strings.Contains(d.out.String(), "FAKEt0ken") {
		t.Errorf("the daemon printed the token:\n%s", d.out.String())
	}
	for _, id := range []string{"e2e-cred-1", "e2e-cred-2"} {
		body, _ := json.Marshal(m.hubEvents(id))
		if strings.Contains(string(body), "FAKEt0ken") {
			t.Errorf("run %s's events carry the token: %s", id, body)
		}
	}
	caches, _ := filepath.Glob(filepath.Join(m.p.data, "repos", "*.git"))
	if len(caches) != 1 {
		t.Fatalf("bare caches: %v", caches)
	}
	origin, err := exec.Command(git, "-C", caches[0], "config", "--get", "remote.origin.url").Output()
	if want := srv.URL + "/acme.git"; err != nil || strings.TrimSpace(string(origin)) != want {
		t.Errorf("the cache's origin is %q (%v), want %s", origin, err, want)
	}
	sess, err := m.runnerStore().GetSession(context.Background(), db.GetSessionParams{Connection: "home", ID: session})
	if err != nil {
		t.Fatal(err)
	}
	var recorded []v1.Source
	if err := json.Unmarshal([]byte(sess.Sources.String), &recorded); err != nil || len(recorded) != 1 || recorded[0].Git.URL != srv.URL+"/acme.git" {
		t.Errorf("the session's sources are %s (%v), want the URL without its userinfo", sess.Sources.String, err)
	}
}
