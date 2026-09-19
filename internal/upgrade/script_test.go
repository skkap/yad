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
// is tested beside it. `gh` is a shell stub on PATH: no test here touches the
// network (ARCHITECTURE.md §7).
//
// What this proves is local. It proves the script picks the right asset for
// this machine, refuses a download whose checksum does not match, leaves an
// installed yad alone when it refuses, and places the binary 0755 in
// ~/.local/bin. It proves nothing about a fresh Linux VM, a real `gh` login or
// a real GitHub release.

// ghStub answers the calls the install script makes and records its argv, so a
// test can say which calls happened. It deliberately does not answer
// `auth status`: the script must not gate on it, and a stub that answered it
// would hide the day someone puts the gate back.
const ghStub = `#!/bin/sh
echo "$@" >>"$GH_LOG"
if [ -n "$GH_FAIL" ]; then echo "$GH_FAIL" >&2; exit 1; fi
case "$1 $2" in
  "release view") echo "$FAKE_TAG" ;;
  "release download")
    dir=
    while [ $# -gt 0 ]; do
      if [ "$1" = "--dir" ]; then dir=$2; fi
      shift
    done
    cp "$FAKE_RELEASE"/* "$dir"/
    ;;
  *) echo "fake gh: unexpected: $*" >&2; exit 1 ;;
esac
`

type install struct {
	t          *testing.T
	home       string
	releaseDir string
	tag        string
	ghLog      string
	env        []string
}

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
	i := &install{t: t, home: t.TempDir(), releaseDir: t.TempDir(), tag: tag}
	i.ghLog = filepath.Join(t.TempDir(), "gh.log")

	for name, body := range assets {
		if err := os.WriteFile(filepath.Join(i.releaseDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(ghStub), 0o755); err != nil {
		t.Fatal(err)
	}
	i.env = []string{
		"HOME=" + i.home,
		"PATH=" + bin + ":" + os.Getenv("PATH"),
		"GH_LOG=" + i.ghLog,
		"FAKE_RELEASE=" + i.releaseDir,
		"FAKE_TAG=" + tag,
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
	b, err := os.ReadFile(i.ghLog)
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
	// `gh auth status` reports a problem with an account on *any* host, so a
	// stale GitHub Enterprise entry would refuse an install that works. The
	// real calls say what is wrong in gh's own words instead.
	if strings.Contains(i.calls(), "auth status") {
		t.Errorf("gh calls were %q — the install is gated on the login state of every host", i.calls())
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

// gh takes several --pattern flags as alternatives and fails only when all of
// them match nothing, so a release missing one asset is a success it has to
// notice itself. Before it did, a missing checksums.txt died in awk's words
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

	if _, stderr, err := i.run("YAD_VERSION=v0.3.1"); err != nil {
		t.Fatalf("install.sh: %v\n%s", err, stderr)
	}
	calls := i.calls()
	if strings.Contains(calls, "release view") {
		t.Errorf("gh calls were %q — a pinned version asks for no newest release", calls)
	}
	if !strings.Contains(calls, "release download v0.3.1") {
		t.Errorf("gh calls were %q, want the pinned tag downloaded", calls)
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
	if strings.Contains(i.calls(), "release download") {
		t.Errorf("gh calls were %q — it fetched a release it had nowhere to put", i.calls())
	}
}

// gh fails the same way for a repository that has published nothing as for a
// login problem, and an operator sent to `gh auth login` here debugs a login
// that works. This is the state skkap/yad is in today.
func TestInstallScriptSaysWhenThereAreNoReleases(t *testing.T) {
	i := newInstall(t, "v0.4.0", map[string]string{})
	i.env = append(i.env, "GH_FAIL=release not found")

	_, stderr, err := i.run()
	if err == nil {
		t.Fatal("install.sh invented a release")
	}
	if !strings.Contains(stderr, "published no release yet") {
		t.Errorf("stderr %q, want it to say the repository has no releases", stderr)
	}
	if strings.Contains(stderr, "gh auth status") {
		t.Errorf("stderr %q sends the operator to check a login that is fine", stderr)
	}
}

func TestInstallScriptRefusesWithoutGH(t *testing.T) {
	name := hostAsset(t)
	bodies := map[string]string{name: "the release binary"}
	i := newInstall(t, "v0.4.0", map[string]string{name: bodies[name], ChecksumsName: checksums(bodies)})
	// An empty PATH but for the directories the script's own tools live in,
	// with no gh: the first thing a fresh box gets wrong.
	bare := t.TempDir()
	for _, tool := range []string{"uname", "mktemp", "awk", "cp", "mv", "chmod", "mkdir", "rm", "cut", "sha256sum", "shasum"} {
		if p, err := exec.LookPath(tool); err == nil {
			os.Symlink(p, filepath.Join(bare, tool))
		}
	}
	i.env = []string{"HOME=" + i.home, "PATH=" + bare, "GH_LOG=" + i.ghLog}

	_, stderr, err := i.run()
	if err == nil {
		t.Fatal("install.sh carried on without gh")
	}
	if !strings.Contains(stderr, "gh auth login") {
		t.Errorf("stderr %q does not say how to fix it", stderr)
	}
}
