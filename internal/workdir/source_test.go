//go:build unix

package workdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/config"
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

// fakeToken stands for a credential a hub put in a git source's URL.
const fakeToken = "ghp_FAKEt0kenFAKEt0ken"

// A refused git URL goes back to the hub that sent it, in the run's error,
// and never with the credential it carried: its userinfo, query and fragment
// are taken out, and a URL that cannot be taken apart is not repeated at all
// (decision 0063, as DEV-91 for a hub's own URL). Every shape here is refused,
// each by a different check.
func TestARefusedGitURLNeverCarriesItsCredential(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		url string
		why string // a word the refusal must still carry
	}{
		{"http://user:" + fakeToken + "@example.com/a.git", "http://"},
		{"git://" + fakeToken + "@example.com/a.git", "git://"},
		{"https::https://" + fakeToken + "@example.com/a", "remote helper"},
		{"file://" + fakeToken + "@evil.example/a", "another host"},
		{"https://" + fakeToken + "@/a", "no host"},
		{"https://" + fakeToken + "@-oProxyCommand=x/a", "'-'"},
		{fakeToken + "@-oProxyCommand=x:a", "'-'"},
		{"https://" + fakeToken + "@example.com/a b", "whitespace"},
		{"-https://" + fakeToken + "@example.com/a", "'-'"},
		{"https://" + fakeToken + "@example.com:" + fakeToken + "/a", "not a URL"},
		{"https://x:" + fakeToken + "@example.com:bad/a", "not a URL"},
		{fakeToken + "@example.com/a", "neither"},
		{"http://example.com/a.git?access_token=" + fakeToken, "http://"},
		{"http://example.com/a.git#" + fakeToken, "http://"},
		{"http://" + strings.Replace(fakeToken, "_", "%40", 1) + "%40x@example.com/a", "http://"},
	} {
		_, err := parseRemote(tc.url, []string{root})
		if err == nil {
			t.Errorf("%s was accepted", tc.url)
			continue
		}
		if strings.Contains(err.Error(), "FAKEt0ken") {
			t.Errorf("the refusal carries the token: %v", err)
		}
		if !strings.Contains(err.Error(), tc.why) {
			t.Errorf("refused as %q; want it to say %q", err, tc.why)
		}
	}
}

// What a refusal or an event prints of a source: nothing to take out leaves
// it exactly as the hub wrote it, so the hub can find it.
func TestShownURL(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"https://github.com/skkap/yad.git", "https://github.com/skkap/yad.git"},
		{"git@github.com:skkap/yad.git", "redacted@github.com:skkap/yad.git"},
		{"github.com:skkap/yad", "github.com:skkap/yad"},
		{"/home/someone/src/yad", "/home/someone/src/yad"},
		{"/home/someone/go/pkg/mod/example.com/yad@v2.0.0", "/home/someone/go/pkg/mod/example.com/yad@v2.0.0"},
		{"https://" + fakeToken + "@github.com/skkap/yad", "https://redacted@github.com/skkap/yad"},
		{"ssh://" + fakeToken + "@github.com/skkap/yad", "ssh://redacted@github.com/skkap/yad"},
		{"a@b@github.com:skkap/yad", config.UnprintableURL},
		{"github.com:skkap/" + fakeToken + "@yad", config.UnprintableURL},
		{fakeToken + "@github.com", config.UnprintableURL},
	} {
		if got := shownURL(tc.raw); got != tc.want {
			t.Errorf("shownURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// git's reason quotes the remote it could not reach, whatever credential the
// URL carried; the reason reaches the hub in the run's error.
func TestRedactURLsInGitsReason(t *testing.T) {
	for _, tc := range []struct{ line, want string }{
		{"fatal: unable to access 'https://" + fakeToken + "@github.com/a/b.git/': Could not resolve host: github.com",
			"fatal: unable to access 'https://redacted@github.com/a/b.git/': Could not resolve host: github.com"},
		{"fatal: repository https://x:" + fakeToken + "@github.com/a/b/ not found",
			"fatal: repository https://redacted@github.com/a/b/ not found"},
		{"fatal: couldn't find remote ref main", "fatal: couldn't find remote ref main"},
		{"fatal: 'https://github.com/a/b.git?t=" + fakeToken + "' not found", "fatal: 'https://github.com/a/b.git?redacted' not found"},
	} {
		if got := redactURLs(tc.line); got != tc.want {
			t.Errorf("redactURLs(%q)\n = %q\nwant %q", tc.line, got, tc.want)
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
