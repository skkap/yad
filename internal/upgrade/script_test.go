package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// scripts/install.sh is the other half of this package — the same release, the
// same checksums.txt, the same refusal when a download does not match — so it
// is tested beside it. `curl` is a shell stub on PATH: no test here touches the
// network (ARCHITECTURE.md §7).
//
// What this proves is local. It proves the script picks the right asset for
// this machine, refuses a download whose checksum does not match, leaves an
// installed yad alone when it refuses, and places the binary 0755 in
// ~/.local/bin. It proves nothing about a fresh Linux VM or a real GitHub
// release.

// curlStub answers the URLs the install script asks for the way github.com
// answers them without a login, and records each one. It *acts* on the URL
// rather than handing over fixtures whatever it was asked: a stub that did
// would stay green if the script requested another machine's binary or
// another repository, which is the failure it exists to catch. It refuses a
// request that does not pin HTTPS, redirects included.
const curlStub = `#!/bin/sh
out= ; fmt= ; url= ; proto= ; redir=
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift ;;
    -w) fmt=$2; shift ;;
    --proto) proto=$2; shift ;;
    --proto-redir) redir=$2; shift ;;
    -*) ;;
    *) url=$1 ;;
  esac
  shift
done
echo "$url" >>"$CURL_LOG"
[ "$proto" = "=https" ] && [ "$redir" = "=https" ] || { echo "fake curl: $url without HTTPS pinned" >&2; exit 2; }
if [ -n "$CURL_FAIL" ]; then echo "curl: (6) Could not resolve host: github.com" >&2; exit 6; fi
code=404 ; location= ; file=
releases="https://github.com/$FAKE_REPO/releases"
exists() { for t in $FAKE_TAGS; do [ "$t" = "$1" ] && return 0; done; return 1; }
case "$url" in
  "$releases/latest")
    code=302
    if [ -n "$FAKE_TAGS" ]; then location="$releases/tag/${FAKE_TAGS%% *}"; else location=$releases; fi ;;
  "$releases/tag/"*)
    exists "${url#"$releases/tag/"}" && code=200 ;;
  "$releases/download/"*)
    rest=${url#"$releases/download/"}
    if exists "${rest%%/*}" && [ -f "$FAKE_RELEASE/${rest#*/}" ]; then code=200; file="$FAKE_RELEASE/${rest#*/}"; fi ;;
esac
if [ -n "$out" ] && [ "$out" != /dev/null ]; then
  if [ -n "$file" ]; then cp "$file" "$out"; else echo "Not Found" >"$out"; fi
fi
printf '%s' "$fmt" | sed -e "s|%{http_code}|$code|" -e "s|%{redirect_url}|$location|"
`

type install struct {
	t          *testing.T
	home       string
	releaseDir string
	curlLog    string
	env        []string
}

// newInstall serves one release of skkap/yad under tag; an empty tag is a
// repository that has published nothing.
func newInstall(t *testing.T, tag string, assets map[string]string) *install {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX shell on this machine")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		if _, err := exec.LookPath("shasum"); err != nil {
			t.Skip("neither sha256sum nor shasum on this machine")
		}
	}
	i := &install{t: t, home: t.TempDir(), releaseDir: t.TempDir()}
	i.curlLog = filepath.Join(t.TempDir(), "curl.log")

	for name, body := range assets {
		if err := os.WriteFile(filepath.Join(i.releaseDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(curlStub), 0o755); err != nil {
		t.Fatal(err)
	}
	i.env = []string{
		"HOME=" + i.home,
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"CURL_LOG=" + i.curlLog,
		"FAKE_REPO=" + DefaultRepo,
		"FAKE_RELEASE=" + i.releaseDir,
		"FAKE_TAGS=" + tag,
	}
	return i
}

func (i *install) run(env ...string) (stdout, stderr string, err error) {
	i.t.Helper()
	cmd := exec.Command("sh", filepath.Join("..", "..", "scripts", "install.sh"))
	cmd.Env = append(append([]string{}, i.env...), env...)
	var out, errs strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errs
	err = cmd.Run()
	return out.String(), errs.String(), err
}

func (i *install) installed() string { return filepath.Join(i.home, ".local", "bin", "yad") }

func (i *install) calls() string {
	i.t.Helper()
	b, err := os.ReadFile(i.curlLog)
	if err != nil {
		return ""
	}
	return string(b)
}

// hostAsset is the asset this machine's uname resolves to, so the stub's
// release carries the one the script will ask for.
func hostAsset(t *testing.T) string {
	t.Helper()
	name, err := AssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skipf("yad publishes no release for this machine: %v", err)
	}
	return name
}

func checksums(assets map[string]string) string {
	var b strings.Builder
	for name, body := range assets {
		h := sha256.Sum256([]byte(body))
		fmt.Fprintf(&b, "%s  %s\n", hex.EncodeToString(h[:]), name)
	}
	return b.String()
}

func TestInstallScriptPlacesTheBinary(t *testing.T) {
	name := hostAsset(t)
	bodies := map[string]string{name: "the release binary"}
	i := newInstall(t, "v0.4.0", map[string]string{name: bodies[name], ChecksumsName: checksums(bodies)})

	stdout, stderr, err := i.run()
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, stderr)
	}
	if got := read(t, i.installed()); got != "the release binary" {
		t.Errorf("~/.local/bin/yad holds %q, want the release", got)
	}
	info, err := os.Stat(i.installed())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode is %v, want 0755", info.Mode().Perm())
	}
	if !strings.Contains(stdout, "installed v0.4.0") {
		t.Errorf("stdout %q does not name what it installed", stdout)
	}
	// A fresh ~/.local/bin is not on PATH, and an operator who is not told so
	// has a yad that `command -v` cannot find.
	if !strings.Contains(stdout, "not on PATH") {
		t.Errorf("stdout %q does not warn that the directory is not on PATH", stdout)
	}
	for _, want := range []string{"/releases/latest", "/releases/download/v0.4.0/" + name, "/releases/download/v0.4.0/" + ChecksumsName} {
		if !strings.Contains(i.calls(), "https://github.com/"+DefaultRepo+want) {
			t.Errorf("requests were %q, want %s", i.calls(), want)
		}
	}
	// Nothing is staged in ~/.local/bin but the binary itself.
	entries, err := os.ReadDir(filepath.Dir(i.installed()))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("~/.local/bin holds %d entries, want only yad", len(entries))
	}
}

// TestInstallScriptRefusesABadChecksum is the install script's half of the
// acceptance criterion: a download that does not match is refused, and the yad
// that was already there still runs.
func TestInstallScriptRefusesABadChecksum(t *testing.T) {
	name := hostAsset(t)
	for _, c := range []struct {
		label, sums, want string
	}{
		{"a hash for other bytes", checksums(map[string]string{name: "some other build"}), "does not match its published checksum"},
		{"only another target listed", checksums(map[string]string{"yad-nothing-here": "x"}), "does not list"},
		{"nothing listed at all", "", "does not list"},
	} {
		t.Run(c.label, func(t *testing.T) {
			i := newInstall(t, "v0.4.0", map[string]string{name: "the release binary", ChecksumsName: c.sums})
			// A yad is already installed: it is what must survive a refusal.
			if err := os.MkdirAll(filepath.Dir(i.installed()), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(i.installed(), []byte("the yad that works"), 0o755); err != nil {
				t.Fatal(err)
			}

			_, stderr, err := i.run()
			if err == nil {
				t.Fatal("install.sh installed a binary it could not verify")
			}
			if !strings.Contains(stderr, c.want) {
				t.Errorf("stderr %q, want it to say %q", stderr, c.want)
			}
			if !strings.Contains(stderr, "nothing was installed") {
				t.Errorf("stderr %q does not tell the operator the installed yad is intact", stderr)
			}
			if got := read(t, i.installed()); got != "the yad that works" {
				t.Errorf("~/.local/bin/yad holds %q, want the one that was working", got)
			}
		})
	}
}

// A release missing one asset downloads the other, so the script has to notice
// the gap itself. Before it did, a missing checksums.txt died in awk's words
// and a missing binary hashed to nothing and read as a checksum mismatch.
func TestInstallScriptRefusesAnIncompleteRelease(t *testing.T) {
	name := hostAsset(t)
	bodies := map[string]string{name: "the release binary"}
	for _, c := range []struct {
		label, missing, want string
	}{
		{"no checksums.txt", ChecksumsName, "has no checksums.txt"},
		{"no binary for this machine", name, "has no " + name},
	} {
		t.Run(c.label, func(t *testing.T) {
			assets := map[string]string{name: bodies[name], ChecksumsName: checksums(bodies)}
			delete(assets, c.missing)
			i := newInstall(t, "v0.4.0", assets)

			_, stderr, err := i.run()
			if err == nil {
				t.Fatal("install.sh installed from a release that was missing an asset")
			}
			if !strings.Contains(stderr, c.want) {
				t.Errorf("stderr %q, want it to name what the release is missing (%q)", stderr, c.want)
			}
			if !strings.Contains(stderr, "nothing was installed") {
				t.Errorf("stderr %q does not tell the operator nothing changed", stderr)
			}
			if !strings.Contains(stderr, "install:") {
				t.Errorf("stderr %q is not the script's own refusal — it died in another tool's words", stderr)
			}
			if _, err := os.Stat(i.installed()); err == nil {
				t.Error("it installed something anyway")
			}
		})
	}
}

func TestInstallScriptPinsAVersion(t *testing.T) {
	name := hostAsset(t)
	bodies := map[string]string{name: "the pinned binary"}
	i := newInstall(t, "v0.4.0", map[string]string{name: bodies[name], ChecksumsName: checksums(bodies)})

	if _, stderr, err := i.run("YAD_VERSION=v0.3.1", "FAKE_TAGS=v0.4.0 v0.3.1"); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, stderr)
	}
	calls := i.calls()
	if strings.Contains(calls, "/releases/latest") {
		t.Errorf("requests were %q — a pinned version asks for no newest release", calls)
	}
	if !strings.Contains(calls, "/releases/download/v0.3.1/"+name) {
		t.Errorf("requests were %q, want the pinned tag downloaded", calls)
	}
}

// Every asset of a tag that does not exist is missing too, and naming them as
// an incomplete release would send the operator after the wrong problem.
func TestInstallScriptSaysWhenAPinnedVersionIsMissing(t *testing.T) {
	name := hostAsset(t)
	bodies := map[string]string{name: "the release binary"}
	i := newInstall(t, "v0.4.0", map[string]string{name: bodies[name], ChecksumsName: checksums(bodies)})

	_, stderr, err := i.run("YAD_VERSION=v9.9.9")
	if err == nil {
		t.Fatal("install.sh installed a release that does not exist")
	}
	if !strings.Contains(stderr, "has no release v9.9.9") {
		t.Errorf("stderr %q, want it to name the tag as the problem", stderr)
	}
	if strings.Contains(i.calls(), "/download/") {
		t.Errorf("requests were %q — it asked for assets of a release that does not exist", i.calls())
	}
}

func TestInstallScriptHonoursAnInstallDir(t *testing.T) {
	name := hostAsset(t)
	bodies := map[string]string{name: "the release binary"}
	i := newInstall(t, "v0.4.0", map[string]string{name: bodies[name], ChecksumsName: checksums(bodies)})
	dir := filepath.Join(t.TempDir(), "somewhere", "else")

	stdout, stderr, err := i.run("YAD_INSTALL_DIR=" + dir)
	if err != nil {
		t.Fatalf("install.sh: %v\n%s", err, stderr)
	}
	if got := read(t, filepath.Join(dir, "yad")); got != "the release binary" {
		t.Errorf("%s/yad holds %q, want the release", dir, got)
	}
	if _, err := os.Stat(i.installed()); err == nil {
		t.Error("it wrote into ~/.local/bin as well as the directory it was given")
	}
	if !strings.Contains(stdout, dir) {
		t.Errorf("stdout %q does not say where it installed", stdout)
	}
}

// The Go half creates its staging directory before downloading, so an
// unwritable destination is refused in a second rather than after the whole
// release has been fetched. The script has to do the same, and refuse in its
// own words: dying in cp's leaves the operator a temporary path they never
// chose and no next action.
func TestInstallScriptRefusesAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a 0500 directory anyway")
	}
	name := hostAsset(t)
	bodies := map[string]string{name: "the release binary"}
	i := newInstall(t, "v0.4.0", map[string]string{name: bodies[name], ChecksumsName: checksums(bodies)})
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	_, stderr, err := i.run("YAD_INSTALL_DIR=" + dir)
	if err == nil {
		t.Fatal("install.sh reported success in a directory it cannot write")
	}
	if !strings.Contains(stderr, "install:") || !strings.Contains(stderr, dir) {
		t.Errorf("stderr %q is not the script's own refusal naming the directory", stderr)
	}
	if i.calls() != "" {
		t.Errorf("requests were %q — it fetched a release it had nowhere to put", i.calls())
	}
}

// GitHub redirects /releases/latest of a repository with no releases to its
// releases page, and that is a state to name rather than a failure.
func TestInstallScriptSaysWhenThereAreNoReleases(t *testing.T) {
	i := newInstall(t, "", map[string]string{})

	_, stderr, err := i.run()
	if err == nil {
		t.Fatal("install.sh invented a release")
	}
	if !strings.Contains(stderr, "published no release yet") {
		t.Errorf("stderr %q, want it to say the repository has no releases", stderr)
	}
}

// A private fork answers 404 without a login, exactly as a mistyped one does.
func TestInstallScriptSaysWhenTheRepositoryIsNotThere(t *testing.T) {
	i := newInstall(t, "v0.4.0", map[string]string{})

	_, stderr, err := i.run("YAD_REPO=someone/private-fork")
	if err == nil {
		t.Fatal("install.sh reported success from a repository that is not there")
	}
	if !strings.Contains(stderr, "someone/private-fork does not exist or is private") {
		t.Errorf("stderr %q, want it to name the repository and why it may be missing", stderr)
	}
}

func TestInstallScriptSaysWhenItCannotConnect(t *testing.T) {
	i := newInstall(t, "v0.4.0", map[string]string{})

	_, stderr, err := i.run("CURL_FAIL=1")
	if err == nil {
		t.Fatal("install.sh reported success with no network")
	}
	if !strings.Contains(stderr, "could not reach github.com") || !strings.Contains(stderr, "Could not resolve host") {
		t.Errorf("stderr %q, want its own refusal carrying curl's words", stderr)
	}
}

func TestInstallScriptRefusesWithoutCurl(t *testing.T) {
	i := newInstall(t, "v0.4.0", map[string]string{})
	// An empty PATH but for the directories the script's own tools live in,
	// with no curl: the first thing a minimal container gets wrong.
	bare := t.TempDir()
	for _, tool := range []string{"uname", "mktemp", "awk", "cp", "mv", "chmod", "mkdir", "rm", "cut", "sha256sum", "shasum"} {
		if p, err := exec.LookPath(tool); err == nil {
			os.Symlink(p, filepath.Join(bare, tool))
		}
	}
	i.env = []string{"HOME=" + i.home, "PATH=" + bare}

	_, stderr, err := i.run()
	if err == nil {
		t.Fatal("install.sh carried on without curl")
	}
	if !strings.Contains(stderr, "curl is not on PATH") {
		t.Errorf("stderr %q does not say what is missing", stderr)
	}
}
