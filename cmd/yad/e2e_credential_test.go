package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// A run whose git source is an https URL carrying a token, end to end, twice
// in one session (decision 0068). The repository is served by git's own
// smart-http backend on a loopback TLS listener, in process, and answers only
// that token — so the fetch proves the token reached git. After both runs no
// byte of it is anywhere the runner keeps things: the data directory, which
// holds state.db, the daemon's log, the bare cache and the session's workdir;
// the daemon's own output; the events the hub received.
func TestE2EASourceURLsTokenIsNotKept(t *testing.T) { eachHarness(t, testE2EASourceURLsTokenIsNotKept) }

func testE2EASourceURLsTokenIsNotKept(t *testing.T, h *e2eHarness) {
	const token = "ghp_FAKEt0kenFAKEt0ken"
	m, git, served := tokenRepo(t, h, token)
	url := "https://" + token + "@" + strings.TrimPrefix(served, "https://") + "/acme.git"

	d := m.daemon()
	runOnce := func(runID string, extra ...string) {
		t.Helper()
		m.tokenRun(d, runID, url, extra...)
	}
	runOnce("e2e-cred-1")
	session := localRun(t, m.runnerStore(), "e2e-cred-1").SessionID
	runOnce("e2e-cred-2", "--session", session)

	if held := tokenHeld(t, m.p.data, "FAKEt0ken"); len(held) > 0 {
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
	if want := served + "/acme.git"; err != nil || strings.TrimSpace(string(origin)) != want {
		t.Errorf("the cache's origin is %q (%v), want %s", origin, err, want)
	}
	sess, err := m.runnerStore().GetSession(context.Background(), db.GetSessionParams{Connection: "home", ID: session})
	if err != nil {
		t.Fatal(err)
	}
	var recorded []v1.Source
	if err := json.Unmarshal([]byte(sess.Sources.String), &recorded); err != nil || len(recorded) != 1 || recorded[0].Git.URL != served+"/acme.git" {
		t.Errorf("the session's sources are %s (%v), want the URL without its userinfo", sess.Sources.String, err)
	}
}

// A version before decision 0068 kept an https source URL's token: as the
// bare cache's origin, in the cache's directory name, keyed by the URL whole,
// and in state.db — the run's spec, the session's sources, the "fetching"
// event that quoted the URL. The data directory is laid out as that version
// left it, with the token, and the daemon started once: after that start
// nothing under the data directory holds the token, the daemon printed none
// of it, and the session opened before the upgrade continues instead of
// failing on "a checkout of another repository" (DEV-154).
func TestE2EAPreUpgradeSourceTokenIsTakenOut(t *testing.T) {
	const token = "ghp_FAKEt0kenFAKEt0ken"
	m, git, served := tokenRepo(t, claudeE2E, token)
	stripped := served + "/acme.git"
	url := "https://" + token + "@" + strings.TrimPrefix(stripped, "https://")
	gitOK := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(git, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	d := m.daemon()
	m.tokenRun(d, "e2e-pre-1", url)
	session := localRun(t, m.runnerStore(), "e2e-pre-1").SessionID
	d.halt(t)

	// The layout v0.1.0 made: the cache named by the URL whole, trailing
	// slash trimmed (workdir.parseRemote's key then), fetched from it as its
	// origin, and the session's worktree pointed there by git itself.
	caches, _ := filepath.Glob(filepath.Join(m.p.data, "repos", "*.git"))
	if len(caches) != 1 {
		t.Fatalf("bare caches: %v", caches)
	}
	sum := sha256.Sum256([]byte(url))
	stale := filepath.Join(m.p.data, "repos", "acme-"+hex.EncodeToString(sum[:8])+".git")
	if err := os.Rename(caches[0], stale); err != nil {
		t.Fatal(err)
	}
	gitOK("-C", stale, "config", "remote.origin.url", url)
	// Offered a password by a helper, as the owner's would have: v0.1.0
	// handed git the URL whole, and the listener takes the token as the user.
	gitOK("-C", stale, "-c", "credential.helper=!f() { echo password=x; }; f", "fetch", "--quiet", "origin")
	gitOK("-C", stale, "worktree", "repair")
	st, err := store.Open(context.Background(), filepath.Join(m.p.data, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE runs SET spec = replace(spec, ?1, ?2), had_grants = 0`,
		`UPDATE sessions SET sources = replace(sources, ?1, ?2)`,
		`UPDATE events SET body = replace(body, 'fetching ' || ?1, 'fetching ' || ?2)`,
		// Owed again: the start that made the session ran the migration.
		`INSERT OR IGNORE INTO start_sweeps (name) VALUES ('source_credentials')`,
	} {
		args := []any{stripped, url}
		if !strings.Contains(q, "?1") {
			args = nil
		}
		if _, err := st.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	st.Close()
	for _, where := range []string{"state.db", filepath.Base(stale)} {
		if held := tokenHeld(t, m.p.data, "FAKEt0ken"); !strings.Contains(strings.Join(held, " "), where) {
			t.Fatalf("the fixture holds the token in %v, not in %s: it is not what v0.1.0 left", held, where)
		}
	}

	d = m.daemon()
	m.tokenRun(d, "e2e-pre-2", url, "--session", session)
	d.halt(t)

	if held := tokenHeld(t, m.p.data, "FAKEt0ken"); len(held) > 0 {
		t.Errorf("after the upgrade's first start the runner still holds the token in %v", held)
	}
	if strings.Contains(d.out.String(), "FAKEt0ken") {
		t.Errorf("the daemon printed the token:\n%s", d.out.String())
	}
	caches, _ = filepath.Glob(filepath.Join(m.p.data, "repos", "*.git"))
	sum = sha256.Sum256([]byte(stripped))
	if want := "acme-" + hex.EncodeToString(sum[:8]) + ".git"; len(caches) != 1 || filepath.Base(caches[0]) != want {
		t.Errorf("bare caches %v, want the one moved to %s", caches, want)
	}
	rs := m.runnerStore()
	if r := localRun(t, rs, "e2e-pre-1"); r.HadGrants == 0 {
		t.Error("the run that carried the token can be rebuilt from its row, which no longer holds it")
	}
	// Once: a later start pays one lookup, not a read of every row.
	if pending, err := rs.StartSweepPending(context.Background(), "source_credentials"); err != nil || pending {
		t.Errorf("the sweep is still owed after the start that did it (%v)", err)
	}
}

// tokenRepo is a machine and a repository only the token may fetch: git's
// own smart-http backend on a loopback TLS listener, in process. It returns
// the machine, the git the test runs and the listener's URL.
func tokenRepo(t *testing.T, h *e2eHarness, token string) (*machine, string, string) {
	t.Helper()
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
	t.Cleanup(srv.Close)

	t.Setenv(h.starts, filepath.Join(t.TempDir(), "harness.starts"))
	t.Setenv(h.read, "note.txt")
	return m, git, srv.URL
}

// tokenRun submits a run from the repository tokenRepo serves, at url, and
// waits for it to answer with what the repository holds.
func (m *machine) tokenRun(d *daemon, runID, url string, extra ...string) {
	m.t.Helper()
	const answer = "fetched with the token"
	args := append([]string{"--run-id", runID, "--git", url}, extra...)
	m.ok(m.submitArgs(append(args, m.h.instruction)...)...)
	if code, watched, errs := m.watch(runID); code != 0 || !strings.Contains(watched, answer) {
		m.t.Fatalf("%s: watch exit %d: %s\n%s\ndaemon:\n%s", runID, code, errs, watched, d.out.String())
	}
}

// tokenHeld lists every file under data whose name or contents hold token,
// but the hub's own store: it lives there in these tests only, and holding
// the URL it was sent is the hub's business.
func tokenHeld(t *testing.T, data, token string) []string {
	t.Helper()
	var held []string
	err := filepath.WalkDir(data, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(e.Name(), "hub.db") || !e.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(path, token) || strings.Contains(string(b), token) {
			held = append(held, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return held
}
