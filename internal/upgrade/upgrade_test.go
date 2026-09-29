package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSource is a release that lives in memory. Nothing here reaches GitHub:
// the network is a seam to fake, not a thing to exercise (ARCHITECTURE.md §7).
type fakeSource struct {
	tag       string
	files     map[string][]byte
	latestErr error
	downErr   error

	// seenAtDownload is what the installed binary held at the moment the
	// download ran. The whole safety claim of this package is that it is still
	// the old binary, so the test reads it from inside the seam.
	seenAtDownload []byte
	watch          string
}

func (f *fakeSource) Latest(context.Context) (string, error) {
	if f.latestErr != nil {
		return "", f.latestErr
	}
	return f.tag, nil
}

func (f *fakeSource) Download(_ context.Context, tag string, assets []string, dir string) error {
	if f.watch != "" {
		f.seenAtDownload, _ = os.ReadFile(f.watch)
	}
	if f.downErr != nil {
		return f.downErr
	}
	if tag != f.tag {
		return fmt.Errorf("no release %s", tag)
	}
	// A Source leaves out an asset the release does not carry rather than
	// failing on it. A fake that refused the moment one is missing would never
	// let Apply reach its own guards, which is the case a release published
	// without its checksums lands in.
	for _, a := range assets {
		b, ok := f.files[a]
		if !ok {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, a), b, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// release builds the assets a real release carries: one binary per target and
// a checksums.txt in sha256sum's own format.
func release(t *testing.T, tag string, binaries map[string][]byte) *fakeSource {
	t.Helper()
	files := map[string][]byte{}
	var sums strings.Builder
	for name, body := range binaries {
		files[name] = body
		fmt.Fprintf(&sums, "%s  %s\n", sum(body), name)
	}
	files[ChecksumsName] = []byte(sums.String())
	return &fakeSource{tag: tag, files: files}
}

// installed is a yad on disk, in a directory of its own, as ~/.local/bin holds one.
func installed(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "yad")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// leftovers names anything in the target's directory that is not the target:
// a staging directory that outlived a failure would show up here.
func leftovers(t *testing.T, target string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	var extra []string
	for _, e := range entries {
		if e.Name() != filepath.Base(target) {
			extra = append(extra, e.Name())
		}
	}
	return extra
}

const asset = "yad-linux-amd64"

func opts(src Source, target, tag string) Options {
	return Options{Source: src, Target: target, GOOS: "linux", GOARCH: "amd64", Tag: tag}
}

func TestApplyReplacesTheBinary(t *testing.T) {
	target := installed(t, "old yad")
	src := release(t, "v0.4.0", map[string][]byte{asset: []byte("new yad")})
	src.watch = target

	res, err := Apply(context.Background(), opts(src, target, ""))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// macOS puts a test's directory under /var, which is itself a symlink to
	// /private/var, so the path Apply reports is the resolved one.
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tag != "v0.4.0" || res.Path != resolved {
		t.Errorf("Apply = %+v, want tag v0.4.0 at %s", res, resolved)
	}
	if got := read(t, target); got != "new yad" {
		t.Errorf("binary is %q, want the release", got)
	}
	if string(src.seenAtDownload) != "old yad" {
		t.Errorf("the installed binary was %q when the download ran, want it untouched", src.seenAtDownload)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("mode is %v, want 0755 — a release that lands unexecutable is no upgrade", info.Mode().Perm())
	}
	if extra := leftovers(t, target); extra != nil {
		t.Errorf("left %v beside the binary", extra)
	}
}

// A self-update runs the release before it is installed (decision 0069): Vet
// sees the verified binary, executable, and a refusal from it leaves the
// installed one exactly as it was.
func TestApplyVetsTheVerifiedBinaryBeforeTheRename(t *testing.T) {
	for _, c := range []struct {
		name   string
		refuse error
		want   string
	}{
		{"taken", nil, "new yad"},
		{"refused", errors.New("it no longer speaks protocol 1"), "old yad"},
	} {
		t.Run(c.name, func(t *testing.T) {
			target := installed(t, "old yad")
			src := release(t, "v0.4.0", map[string][]byte{asset: []byte("new yad")})
			o := opts(src, target, "")
			var vetted, installedThen string
			o.Vet = func(_ context.Context, staged string) error {
				vetted = read(t, staged)
				installedThen = read(t, target)
				info, err := os.Stat(staged)
				if err != nil || info.Mode().Perm() != 0o755 {
					t.Errorf("the binary vetted is %v, %v; want it executable, since vetting runs it", info, err)
				}
				return c.refuse
			}
			_, err := Apply(context.Background(), o)
			if !errors.Is(err, c.refuse) {
				t.Fatalf("Apply = %v, want %v", err, c.refuse)
			}
			if vetted != "new yad" || installedThen != "old yad" {
				t.Errorf("vetted %q with %q installed; want the release, before anything was replaced", vetted, installedThen)
			}
			if got := read(t, target); got != c.want {
				t.Errorf("binary is %q, want %q", got, c.want)
			}
			if extra := leftovers(t, target); extra != nil {
				t.Errorf("left %v beside the binary", extra)
			}
		})
	}
}

// TestApplyRefusesABadChecksum is the acceptance criterion: the checksum is
// checked before the binary is replaced, so a corrupted or tampered download
// leaves a working yad on a machine the owner may only reach through it.
func TestApplyRefusesABadChecksum(t *testing.T) {
	for _, c := range []struct {
		name      string
		checksums string
		want      string
	}{
		{"mismatch", fmt.Sprintf("%s  %s\n", sum([]byte("something else")), asset), "does not match its published checksum"},
		{"asset not listed", fmt.Sprintf("%s  yad-darwin-arm64\n", sum([]byte("new yad"))), "does not list"},
		{"not a checksum", "deadbeef  " + asset + "\n", "no readable checksum"},
		{"empty file", "", "does not list"},
	} {
		t.Run(c.name, func(t *testing.T) {
			target := installed(t, "old yad")
			src := release(t, "v0.4.0", map[string][]byte{asset: []byte("new yad")})
			src.files[ChecksumsName] = []byte(c.checksums)

			_, err := Apply(context.Background(), opts(src, target, ""))
			if err == nil {
				t.Fatal("Apply replaced the binary on a checksum it could not confirm")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q, want it to say %q", err, c.want)
			}
			if !strings.Contains(err.Error(), "nothing was replaced") {
				t.Errorf("error %q does not tell the operator the binary is intact", err)
			}
			if got := read(t, target); got != "old yad" {
				t.Errorf("binary is %q, want the one that was working", got)
			}
			if extra := leftovers(t, target); extra != nil {
				t.Errorf("left %v beside the binary", extra)
			}
		})
	}
}

func TestApplyLeavesTheBinaryWhenTheFetchFails(t *testing.T) {
	for _, c := range []struct {
		name string
		with func(*fakeSource)
	}{
		{"no newest release", func(s *fakeSource) { s.latestErr = errors.New("could not reach github.com") }},
		{"download refused", func(s *fakeSource) { s.downErr = errors.New("skkap/yad has no release v0.4.0") }},
		{"nothing in the release matches at all", func(s *fakeSource) { s.files = map[string][]byte{} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			target := installed(t, "old yad")
			src := release(t, "v0.4.0", map[string][]byte{asset: []byte("new yad")})
			c.with(src)

			if _, err := Apply(context.Background(), opts(src, target, "")); err == nil {
				t.Fatal("Apply reported success without a release to install")
			}
			if got := read(t, target); got != "old yad" {
				t.Errorf("binary is %q, want the one that was working", got)
			}
			if extra := leftovers(t, target); extra != nil {
				t.Errorf("left %v beside the binary", extra)
			}
		})
	}
}

// TestApplyRefusesAnIncompleteRelease is the Go half of what round 1 found in
// the install script: a release that published the binaries and no
// checksums.txt arrives here as a successful download. The
// refusal has to name the release as incomplete — read as a checksum failure
// it sends the operator after tampering that never happened.
func TestApplyRefusesAnIncompleteRelease(t *testing.T) {
	for _, c := range []struct {
		name, missing, want string
	}{
		{"no checksums.txt", ChecksumsName, "has no " + ChecksumsName},
		{"no binary for this machine", asset, "did not produce " + asset},
	} {
		t.Run(c.name, func(t *testing.T) {
			target := installed(t, "old yad")
			src := release(t, "v0.4.0", map[string][]byte{asset: []byte("new yad")})
			delete(src.files, c.missing)

			_, err := Apply(context.Background(), opts(src, target, ""))
			if err == nil {
				t.Fatal("Apply installed from a release that was missing an asset")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q, want it to name what the release is missing (%q)", err, c.want)
			}
			if !strings.Contains(err.Error(), "nothing was replaced") {
				t.Errorf("error %q does not tell the operator the binary is intact", err)
			}
			// "does not match its published checksum" is the tampering
			// message; an incomplete release must not borrow it.
			if strings.Contains(err.Error(), "does not match") {
				t.Errorf("error %q reads as a tampered download when the release is simply incomplete", err)
			}
			if got := read(t, target); got != "old yad" {
				t.Errorf("binary is %q, want the one that was working", got)
			}
			if extra := leftovers(t, target); extra != nil {
				t.Errorf("left %v beside the binary", extra)
			}
		})
	}
}

// TestApplyReplacesWhatASymlinkPointsAt keeps a ~/.local/bin/yad that someone
// linked from elsewhere a link, rather than turning it into a binary and
// stranding whatever it pointed at.
func TestApplyReplacesWhatASymlinkPointsAt(t *testing.T) {
	real := installed(t, "old yad")
	link := filepath.Join(t.TempDir(), "yad")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	src := release(t, "v0.4.0", map[string][]byte{asset: []byte("new yad")})

	res, err := Apply(context.Background(), opts(src, link, ""))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Path == link {
		t.Errorf("Apply reports %s, want the file the link points at", res.Path)
	}
	if got := read(t, real); got != "new yad" {
		t.Errorf("the linked-to binary is %q, want the release", got)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link is no longer a link (%v, %v)", info, err)
	}
}

func TestApplyRefusesAnUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into a 0500 directory anyway")
	}
	target := installed(t, "old yad")
	dir := filepath.Dir(target)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	src := release(t, "v0.4.0", map[string][]byte{asset: []byte("new yad")})
	src.watch = target

	_, err := Apply(context.Background(), opts(src, target, ""))
	if err == nil {
		t.Fatal("Apply reported success in a directory it cannot write")
	}
	if !strings.Contains(err.Error(), "cannot write beside") {
		t.Errorf("error %q, want it to name the directory as the problem", err)
	}
	if src.seenAtDownload != nil {
		t.Error("Apply downloaded a release it had nowhere to put — the check belongs before the fetch")
	}
	if got := read(t, target); got != "old yad" {
		t.Errorf("binary is %q, want the one that was working", got)
	}
}

func TestApplyInstallsANamedTag(t *testing.T) {
	target := installed(t, "old yad")
	src := release(t, "v0.3.1", map[string][]byte{asset: []byte("older yad")})

	res, err := Apply(context.Background(), opts(src, target, "v0.3.1"))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Tag != "v0.3.1" || read(t, target) != "older yad" {
		t.Errorf("Apply = %+v, binary %q — a named tag is installed as named", res, read(t, target))
	}
}

func TestVerify(t *testing.T) {
	body := []byte("the binary")
	for _, c := range []struct {
		name      string
		checksums string
		ok        bool
	}{
		{"sha256sum's format", sum(body) + "  " + asset + "\n", true},
		{"binary mode's asterisk", sum(body) + " *" + asset + "\n", true},
		{"among the other targets", "0000  yad-darwin-arm64\n" + sum(body) + "  " + asset + "\n", true},
		{"trailing blank lines", sum(body) + "  " + asset + "\n\n", true},
		{"no trailing newline", sum(body) + "  " + asset, true},
		{"a different binary's hash", sum([]byte("other")) + "  " + asset + "\n", false},
		{"truncated hash", sum(body)[:32] + "  " + asset + "\n", false},
		{"not hex at all", strings.Repeat("z", 64) + "  " + asset + "\n", false},
		{"another target only", sum(body) + "  yad-darwin-arm64\n", false},
		{"nothing at all", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, asset)
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatal(err)
			}
			err := Verify(path, []byte(c.checksums), asset)
			if (err == nil) != c.ok {
				t.Errorf("Verify = %v, want ok=%v", err, c.ok)
			}
		})
	}
}

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		installed, released string
		want                State
	}{
		{"v0.3.1", "v0.4.0", Behind},
		{"0.3.1", "v0.4.0", Behind},
		{"v0.9.0", "v0.10.0", Behind}, // compared as numbers, not strings
		{"v0.4.0", "v0.4.0", Current},
		{"v0.4", "v0.4.0", Current},
		{"v0.4.0-4-gabc1234", "v0.4.0", Current}, // four commits past the tag, not before it
		{"v0.5.0", "v0.4.0", Ahead},              // --tag rolling back, or a tag not yet cut
		{"v1.0.0", "v0.9.9", Ahead},
		{"dev", "v0.4.0", Unstamped}, // a `go build` in someone's checkout
		// `git describe --tags --always` in a checkout with no reachable tag —
		// an untagged repository, a shallow clone — stamps a short SHA, and
		// about one in thirty of those is all decimal digits. Read as a
		// version it would outrank every release and stop the upgrade.
		{"4886173", "v0.4.0", Unstamped},
		{"7f2331c", "v0.4.0", Unstamped},
		// The release workflow triggers on `v*`, so `v1` is a tag someone may
		// cut, and the "v" is what keeps it apart from a short SHA.
		{"v1", "v0.4.0", Ahead}, // v1 is 1.0.0, which is past 0.4.0
		{"v0.4.0", "v1", Behind},
		{"v1", "v2", Behind},
		{"", "v0.4.0", Unstamped},
		// The tag is the unreadable side here, and the two are kept apart so
		// the message blames the one the operator can do something about.
		{"v0.4.0", "latest", UnreadableTag},
		{"v0.4.0", "stable", UnreadableTag},
		{"dev", "latest", Unstamped}, // both unreadable: the build is judged first
	} {
		if got := Compare(c.installed, c.released); got != c.want {
			t.Errorf("Compare(%q, %q) = %v, want %v", c.installed, c.released, got, c.want)
		}
	}
}

func TestAssetName(t *testing.T) {
	for _, c := range []struct {
		goos, goarch, want string
	}{
		{"linux", "amd64", "yad-linux-amd64"},
		{"linux", "arm64", "yad-linux-arm64"},
		{"darwin", "amd64", "yad-darwin-amd64"},
		{"darwin", "arm64", "yad-darwin-arm64"},
		{"windows", "amd64", ""},
		{"linux", "386", ""},
		{"freebsd", "arm64", ""},
	} {
		got, err := AssetName(c.goos, c.goarch)
		if c.want == "" {
			if err == nil {
				t.Errorf("AssetName(%s, %s) = %q, want a refusal", c.goos, c.goarch, got)
			} else if !strings.Contains(err.Error(), "make install") {
				t.Errorf("AssetName(%s, %s) error %q carries no next action", c.goos, c.goarch, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("AssetName(%s, %s) = %q, %v, want %q", c.goos, c.goarch, got, err, c.want)
		}
	}
}
