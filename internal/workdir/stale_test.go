//go:build unix

package workdir

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// downgrade lays the cache out as a version before decision 0068 would have
// made it from url: named by url whole, trailing slash trimmed, with url as
// its origin, and the worktrees of it pointed at that name by git itself.
func downgrade(t *testing.T, cache, url string) string {
	t.Helper()
	stale := filepath.Join(filepath.Dir(cache), "acme-"+digest(strings.TrimRight(url, "/"))+".git")
	if err := os.Rename(cache, stale); err != nil {
		t.Fatal(err)
	}
	sh(t, stale, "git", "config", "remote.origin.url", url)
	sh(t, stale, "git", "worktree", "repair")
	return stale
}

// A version before decision 0068 named a bare cache by its URL with the
// credential in it and kept that URL as the cache's origin. At the daemon's
// start the first such cache of a repository moves to the name the URL
// without its credential gives, where the next session from the repository
// finds it; one more from the same repository, with another credential,
// stays where it is. Neither holds the credential after, and the sessions
// with a worktree of either continue.
func TestAStaleCacheLosesItsCredentialAndItsSessionsContinue(t *testing.T) {
	f := newFixture(t)
	served := t.TempDir()
	newOrigin(t, served, "acme", map[string]string{"README": "v1\n"})
	cs := newCredServer(t, served, fakeToken, "")
	host := strings.TrimPrefix(cs.srv.URL, "https://")
	url := "https://" + fakeToken + "@" + host + "/acme.git"
	// The same repository with another token: the name a hub's rotating
	// token gave a cache of its own each time.
	other := "https://ghp_FAKEt0kenSECOND@" + host + "/acme.git"
	bare := cs.srv.URL + "/acme.git"
	current := filepath.Join(f.m.Data, "repos", "acme-"+digest(bare)+".git")

	if _, _, err := f.prepare("s1", gitSource(url, "", "")); err != nil {
		t.Fatal(err)
	}
	first := downgrade(t, current, url)
	if _, _, err := f.prepare("s2", gitSource(url, "", "")); err != nil {
		t.Fatal(err)
	}
	second := downgrade(t, current, other)
	if found := holding(t, f.m.Data, "FAKEt0ken"); len(found) == 0 {
		t.Fatal("the fixture holds no token: it is not what an earlier version left")
	}
	ctx := context.Background()
	stale, err := f.m.StaleCaches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 2 {
		t.Fatalf("stale caches = %+v, want both", stale)
	}
	targets := 0
	for _, c := range stale {
		if strings.Contains(c.Scrub.New, "FAKEt0ken") || !strings.Contains(c.Scrub.Old, "FAKEt0ken") || c.Scrub.New != "https://" {
			t.Errorf("scrub %+v", c.Scrub)
		}
		if c.Target != "" {
			targets++
			if !samePath(filepath.Dir(c.Target), filepath.Dir(current)) || filepath.Base(c.Target) != filepath.Base(current) {
				t.Errorf("target %s, want %s", c.Target, current)
			}
		}
		if err := f.m.CleanCache(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	if targets != 1 {
		t.Errorf("%d caches were given the name used now, want one", targets)
	}
	if found := holding(t, f.m.Data, "FAKEt0ken"); len(found) > 0 {
		t.Errorf("the data directory holds the token after the sweep: %v", found)
	}
	if again, err := f.m.StaleCaches(ctx); err != nil || len(again) > 0 {
		t.Errorf("a second sweep finds %+v (%v), want nothing", again, err)
	}
	if _, err := os.Stat(current); err != nil {
		t.Errorf("no cache has the name used now: %v", err)
	}
	for _, s := range []string{"s1", "s2"} {
		p, ev, err := f.prepare(s, gitSource(url, "", ""))
		if err != nil {
			t.Fatalf("%s after the sweep: %v", s, err)
		}
		if !strings.Contains(ev.statuses(), "continuing in the session's worktree") {
			t.Errorf("%s was not continued: %s", s, ev.statuses())
		}
		if got := read(t, filepath.Join(p.Dir, "README")); got != "v1\n" {
			t.Errorf("%s: README = %q", s, got)
		}
	}
	// The moved cache is the one a new session uses, with the branches the
	// sessions before the upgrade made in it.
	if _, _, err := f.prepare("s3", gitSource(url, "", "")); err != nil {
		t.Fatal(err)
	}
	caches, _ := filepath.Glob(filepath.Join(f.m.Data, "repos", "*.git"))
	if len(caches) != 2 {
		t.Errorf("caches = %v, want the moved one and the one left", caches)
	}
	branches := sh(t, current, "git", "branch", "--list")
	if !strings.Contains(branches, "s3") || !(strings.Contains(branches, "/s1") || strings.Contains(branches, "/s2")) {
		t.Errorf("the cache used now has branches %q, want one from before the upgrade beside s3's", branches)
	}
	for _, c := range []string{first, second} {
		if _, err := os.Stat(c); err == nil {
			if got := sh(t, c, "git", "config", "--get", "remote.origin.url"); got != bare {
				t.Errorf("%s's origin = %q, want %q", c, got, bare)
			}
		}
	}
}

// A start that stopped after the move and before the config was rewritten
// finds the cache at the name used now, with the credential still in its
// origin, and takes it out there.
func TestAStaleCacheMovedButNotRewrittenIsFinished(t *testing.T) {
	f := newFixture(t)
	served := t.TempDir()
	newOrigin(t, served, "acme", map[string]string{"README": "v1\n"})
	cs := newCredServer(t, served, fakeToken, "")
	url := "https://" + fakeToken + "@" + strings.TrimPrefix(cs.srv.URL, "https://") + "/acme.git"
	if _, _, err := f.prepare("s1", gitSource(url, "", "")); err != nil {
		t.Fatal(err)
	}
	caches, _ := filepath.Glob(filepath.Join(f.m.Data, "repos", "*.git"))
	sh(t, caches[0], "git", "config", "remote.origin.url", url)

	ctx := context.Background()
	stale, err := f.m.StaleCaches(ctx)
	if err != nil || len(stale) != 1 || stale[0].Target != "" {
		t.Fatalf("stale = %+v (%v), want the cache, staying where it is", stale, err)
	}
	if err := f.m.CleanCache(ctx, stale[0]); err != nil {
		t.Fatal(err)
	}
	if found := holding(t, f.m.Data, "FAKEt0ken"); len(found) > 0 {
		t.Errorf("the data directory holds the token: %v", found)
	}
	if _, _, err := f.prepare("s1", gitSource(url, "", "")); err != nil {
		t.Errorf("s1 after the sweep: %v", err)
	}
}

func TestScrubOf(t *testing.T) {
	for _, tc := range []struct {
		raw string
		ok  bool
		sc  Scrub
	}{
		{"https://tok@github.com/o/r.git", true, Scrub{"https://tok@", "https://"}},
		{"https://u:p@github.com:8443/o/r", true, Scrub{"https://u:p@", "https://"}},
		{"ssh://git:pw@host/o/r", true, Scrub{"ssh://git:pw@", "ssh://git@"}},
		{"ssh://git@host/o/r", false, Scrub{}},
		{"https://github.com/o/r@v1", false, Scrub{}},
		{"git@github.com:o/r.git", false, Scrub{}},
	} {
		sc, ok := scrubOf(tc.raw)
		if ok != tc.ok || sc != tc.sc {
			t.Errorf("scrubOf(%q) = %+v, %v; want %+v, %v", tc.raw, sc, ok, tc.sc, tc.ok)
		}
	}
	forms := Scrub{Old: "https://a&b@", New: "https://"}.Forms()
	if len(forms) != 2 || forms[1].Old != "https://a\\u0026b@" {
		t.Errorf("forms = %+v, want the JSON spelling too", forms)
	}
}
