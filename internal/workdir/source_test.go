//go:build unix

package workdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Hub input that git would read as an option, a program or a place the owner
// did not allow is refused before any git runs.
func TestParseRemote(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "acme.git")
	outside := t.TempDir()
	for _, d := range []string{repo, filepath.Join(outside, "other.git")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	escape := filepath.Join(root, "escape")
	if err := os.Symlink(filepath.Join(outside, "other.git"), escape); err != nil {
		t.Fatal(err)
	}
	realRepo, _ := filepath.EvalSymlinks(repo)
	roots := []string{root}

	for _, tc := range []struct {
		url   string
		roots []string
		name  string // want accepted, with this WT_REPO; "" wants refused
		fetch string // what git is given, when not the URL itself
		why   string // a word the refusal must carry
	}{
		{url: "https://github.com/skkap/yad.git", name: "yad"},
		{url: "https://github.com/skkap/yad", name: "yad"},
		{url: "ssh://git@github.com/skkap/yad.git", name: "yad"},
		{url: "git@github.com:skkap/yad.git", name: "yad"},
		{url: "github.com:skkap/yad", name: "yad"},
		{url: repo, roots: roots, name: "acme", fetch: realRepo},
		{url: "file://" + repo, roots: roots, name: "acme", fetch: realRepo},
		{url: filepath.Join(root, "x", "..", "acme.git"), roots: roots, name: "acme", fetch: realRepo},

		{url: "--upload-pack=touch /tmp/p", why: "'-'"},
		{url: "-oProxyCommand=x", why: "'-'"},
		{url: "ext::sh -c touch% /tmp/p", why: "whitespace"},
		{url: "ext::sh", why: "remote helper"},
		{url: "fd::17", why: "remote helper"},
		{url: "http://example.com/a.git", why: "http://"},
		{url: "git://example.com/a.git", why: "git://"},
		{url: "https://user:hunter2@github.com/a/b.git", why: "password"},
		{url: "ssh://-oProxyCommand=x/a", why: "'-'"},
		{url: "git@-oProxyCommand=x:a", why: "'-'"},
		{url: "-@host:a", why: "'-'"},
		{url: "a/b:c", why: "neither"},
		{url: "relative/path", why: "neither"},
		{url: "https://github.com/a\nb", why: "control"},
		{url: "", why: "empty"},
		{url: repo, why: "may reach none"},
		{url: "file://" + repo, why: "may reach none"},
		{url: "file://evil.example/" + repo, roots: roots, why: "another host"},
		{url: filepath.Join(outside, "other.git"), roots: roots, why: "outside"},
		{url: escape, roots: roots, why: "outside"},
		{url: filepath.Join(root, "..", filepath.Base(outside), "other.git"), roots: roots, why: "outside"},
		{url: filepath.Join(root, "missing.git"), roots: roots, why: "no such file"},
	} {
		r, err := parseRemote(tc.url, tc.roots)
		if tc.name == "" {
			if err == nil {
				t.Errorf("%q was accepted", tc.url)
			} else if !strings.Contains(err.Error(), tc.why) {
				t.Errorf("%q refused as %q; want it to say %q", tc.url, err, tc.why)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q refused: %v", tc.url, err)
			continue
		}
		fetch := tc.fetch
		if fetch == "" {
			fetch = tc.url
		}
		if r.name != tc.name || r.url != fetch {
			t.Errorf("%q = %+v, want name %q fetched from %q", tc.url, r, tc.name, fetch)
		}
	}
}

func TestCheckRef(t *testing.T) {
	for _, ok := range []string{"main", "feature/x", "yad/s1", "v1.2.3", "0123abc", "a-b_c"} {
		if err := checkRef("branch", ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"", "-b", "--orphan", "HEAD", "@", "a..b", "a b", "a~1", "a^", "a:b", "a?", "a*", "a[", `a\b`,
		"a@{1}", "/a", "a/", "a//b", "a.", ".a", "a/.b", "a.lock", "a/b.lock/c", "a\x00b", strings.Repeat("a", 256),
	} {
		if err := checkRef("branch", bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestSafeComponent(t *testing.T) {
	for id, want := range map[string]string{
		"s1":        "s1",
		"ZUM-19":    "ZUM-19",
		"../../etc": "s-" + digest("../../etc"),
		"a b":       "s-" + digest("a b"),
		"x.lock":    "s-" + digest("x.lock"),
	} {
		if got := safeComponent(id); got != want {
			t.Errorf("safeComponent(%q) = %q, want %q", id, got, want)
		}
		if err := checkRef("branch", "yad/"+safeComponent(id)); err != nil {
			t.Errorf("the branch for session %q is not a valid ref: %v", id, err)
		}
	}
}
