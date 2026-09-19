// Package upgrade replaces this binary with a newer tagged release, on the
// owner's command and never on anyone else's: there is no poller here, no
// control message and no re-exec — decision 0018 holds those back to a backlog
// item, and this package is deliberately the whole of what v1 does about
// updating itself.
//
// The order is the point. A download goes to a temporary directory beside the
// installed binary, its SHA-256 is checked against the release's checksums
// file, and only then does one rename(2) put it in place. Nothing truncates
// the binary it is replacing, so an upgrade that fails at any step — no
// network, no `gh`, a corrupted asset, a checksum that does not match — leaves
// a working yad behind.
package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/skkap/yad/internal/buildinfo"
)

// DefaultRepo is where yad's releases live. It is private (decided
// 2026-09-19), which is why every fetch goes through `gh`: there is no
// unauthenticated URL to download from.
const DefaultRepo = "skkap/yad"

// ChecksumsName is the asset holding one `sha256sum` line per binary. The
// release workflow writes it with sha256sum itself, so `sha256sum -c` reads
// the same file an operator can check by hand.
const ChecksumsName = "checksums.txt"

// binaryMode is what `make install` gives the binary, and what a release
// asset — which arrives with no mode worth keeping — is given here.
const binaryMode = 0o755

// A Source hands out the releases of one repository. The only implementation
// that talks to GitHub is GH; tests supply one that copies prepared files,
// because no test here touches the network (ARCHITECTURE.md §7).
type Source interface {
	// Latest names the newest release.
	Latest(ctx context.Context) (tag string, err error)
	// Download places the named assets of one release into dir.
	Download(ctx context.Context, tag string, assets []string, dir string) error
}

// targets are the four `make dist` builds, and so the four assets a release
// carries. A machine outside this list has no binary to fetch.
var targets = map[string]bool{
	"linux/amd64":  true,
	"linux/arm64":  true,
	"darwin/amd64": true,
	"darwin/arm64": true,
}

// AssetName is the release asset for one machine, named as `make dist` names
// its output.
func AssetName(goos, goarch string) (string, error) {
	if !targets[goos+"/"+goarch] {
		return "", fmt.Errorf("yad publishes no release binary for %s/%s — build it from source with `make install`", goos, goarch)
	}
	return fmt.Sprintf("yad-%s-%s", goos, goarch), nil
}

// State is how the installed build stands against the newest release.
type State int

const (
	// Behind: the release is newer than this build, which is what upgrade is for.
	Behind State = iota
	// Current: this build is that release. A build a few commits past the tag
	// reads as Current too — buildinfo.Number compares release cores only.
	Current
	// Ahead: this build is newer than the newest release.
	Ahead
	// Unknown: this build is not stamped from a tag, so there is nothing to
	// compare — `go build ./cmd/yad` without the Makefile's ldflags says "dev".
	Unknown
)

// Compare places the installed version against a release tag.
func Compare(installed, released string) State {
	got, ok := buildinfo.ParseNumber(installed)
	if !ok {
		return Unknown
	}
	want, ok := buildinfo.ParseNumber(released)
	if !ok {
		return Unknown
	}
	switch {
	case got.Older(want):
		return Behind
	case want.Older(got):
		return Ahead
	default:
		return Current
	}
}

// Options is one upgrade.
type Options struct {
	Source Source
	// Target is the binary to replace, usually os.Executable().
	Target string
	// GOOS and GOARCH choose the asset; runtime's values, except in tests.
	GOOS, GOARCH string
	// Tag pins a release. Empty means whichever is newest.
	Tag string
}

// Result is what an upgrade did.
type Result struct {
	// Tag is the release now installed.
	Tag string
	// Path is the file that was replaced, with every symlink resolved.
	Path string
}

// Apply fetches a release and puts it in place of the target binary. It
// returns only after the replacement is complete; every failure before that
// leaves the target exactly as it was.
func Apply(ctx context.Context, o Options) (Result, error) {
	asset, err := AssetName(o.GOOS, o.GOARCH)
	if err != nil {
		return Result{}, err
	}
	tag := o.Tag
	if tag == "" {
		if tag, err = o.Source.Latest(ctx); err != nil {
			return Result{}, err
		}
	}
	// A symlinked yad — one `ln -s`ed into ~/.local/bin from elsewhere —
	// should end with the real file replaced, not with the link overwritten
	// by a binary.
	target, err := filepath.EvalSymlinks(o.Target)
	if err != nil {
		return Result{}, fmt.Errorf("cannot find the installed yad at %s: %w", o.Target, err)
	}
	// The staging directory sits beside the target because rename(2) is only
	// atomic within one filesystem — and because failing to create it here
	// says the directory is unwritable before anything has been downloaded.
	dir, err := os.MkdirTemp(filepath.Dir(target), ".yad-upgrade-")
	if err != nil {
		return Result{}, fmt.Errorf("cannot write beside the installed yad at %s: %w — install yad somewhere you own, such as ~/.local/bin", target, err)
	}
	defer os.RemoveAll(dir)

	if err := o.Source.Download(ctx, tag, []string{asset, ChecksumsName}, dir); err != nil {
		return Result{}, err
	}
	sums, err := os.ReadFile(filepath.Join(dir, ChecksumsName))
	if err != nil {
		return Result{}, fmt.Errorf("release %s has no %s to check the download against — nothing was replaced", tag, ChecksumsName)
	}
	staged := filepath.Join(dir, asset)
	if err := Verify(staged, sums, asset); err != nil {
		return Result{}, fmt.Errorf("%w — nothing was replaced; run `yad upgrade` again, and if it says this twice the release %s is bad", err, tag)
	}
	if err := os.Chmod(staged, binaryMode); err != nil {
		return Result{}, err
	}
	if err := os.Rename(staged, target); err != nil {
		return Result{}, fmt.Errorf("cannot replace %s: %w", target, err)
	}
	return Result{Tag: tag, Path: target}, nil
}

// Verify refuses a downloaded file whose SHA-256 is not the one the release
// published for it. It is the last thing that runs before the installed binary
// is touched, and the reason the order in Apply is not negotiable.
func Verify(path string, checksums []byte, name string) error {
	want, err := checksumFor(checksums, name)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("the release did not produce %s: %w", name, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("%s does not match its published checksum (got %s, the release says %s)", name, got, want)
	}
	return nil
}

// checksumFor reads sha256sum's own output format: the hash, two spaces, and
// the file name, which sha256sum -b prefixes with an asterisk.
func checksumFor(checksums []byte, name string) (string, error) {
	for line := range strings.Lines(string(checksums)) {
		sum, file, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if strings.TrimPrefix(strings.TrimSpace(file), "*") != name {
			continue
		}
		if _, err := hex.DecodeString(sum); err != nil || len(sum) != hex.EncodedLen(sha256.Size) {
			return "", fmt.Errorf("the release's %s has no readable checksum for %s", ChecksumsName, name)
		}
		return sum, nil
	}
	return "", fmt.Errorf("the release's %s does not list %s", ChecksumsName, name)
}
