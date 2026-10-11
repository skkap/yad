package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/skkap/yad/internal/adapter"
)

// A session archive (decision 0081) is a gzip'd tar of manifest.json and the
// conversation's files under conversation/, by the names its adapter gave
// them. A hub stores it and hands it back and reads none of it; the runner
// that opens a session from it reads the manifest and gives the rest to the
// adapter of the harness the manifest names. Standard library only: an
// archive is unpacked on a machine that runs harnesses with its owner's
// credentials, and what reads it is a page of code anyone can check.

// archiveFormat is the manifest's format; one this runner does not know is
// refused, never guessed at.
const archiveFormat = 1

// Bounds on what an archive unpacks to. A conversation is a transcript and
// what Claude saved beside it, which reaches a few hundred megabytes at the
// most measured; these are far past that and still stop an archive that
// would fill the disk.
const (
	maxUnpackedBytes = 4 << 30
	maxArchiveFiles  = 100_000
	maxManifestBytes = 1 << 20
)

// manifest is what a runner needs, beside the conversation, to carry a
// session on.
type manifest struct {
	Format         int    `json:"format"`
	Harness        string `json:"harness"`
	HarnessVersion string `json:"harness_version,omitempty"`
	// NativeID is the harness's session id, which the conversation goes on
	// under on the next runner.
	NativeID string `json:"native_id"`
	// Dir is where the conversation's files were on the runner that released
	// it, which the conversation names (adapter.Exported.Dir).
	Dir string `json:"dir,omitempty"`
	// Sources are the session's git sources and the branch each was on.
	Sources   []carriedSource `json:"sources,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// carriedSource is one git source of a released session: its URL as the
// session kept it, without a credential, and the branch its worktree was on.
type carriedSource struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
}

// packArchive writes the archive to file, made 0600 beside where it goes and
// renamed into place, and returns the sha256 of its bytes and their count.
func packArchive(file string, m manifest, files []adapter.ConversationFile) (sha string, size int64, err error) {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return "", 0, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), "."+filepath.Base(file)+"-")
	if err != nil {
		return "", 0, err
	}
	done := false
	defer func() {
		if !done {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return "", 0, err
	}
	h := sha256.New()
	counted := &countingWriter{w: io.MultiWriter(tmp, h)}
	gz := gzip.NewWriter(counted)
	tw := tar.NewWriter(gz)
	body, err := json.Marshal(m)
	if err != nil {
		return "", 0, err
	}
	if err := writeEntry(tw, "manifest.json", 0o600, int64(len(body)), bytes.NewReader(body)); err != nil {
		return "", 0, err
	}
	for _, f := range files {
		if err := packFile(tw, f); err != nil {
			return "", 0, err
		}
	}
	if err := tw.Close(); err != nil {
		return "", 0, err
	}
	if err := gz.Close(); err != nil {
		return "", 0, err
	}
	if err := tmp.Sync(); err != nil {
		return "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp.Name(), file); err != nil {
		return "", 0, err
	}
	done = true
	return hex.EncodeToString(h.Sum(nil)), counted.n, nil
}

func packFile(tw *tar.Writer, f adapter.ConversationFile) error {
	name, ok := cleanName(f.Name)
	if !ok {
		return fmt.Errorf("the adapter named a conversation file %q, which is no plain relative path", f.Name)
	}
	in, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", f.Path)
	}
	// Written at the size it had when opened: a harness appending meanwhile
	// is not running, since no run is held in a session being released, but
	// a size that moved must not make the tar lie about its entry.
	return writeEntry(tw, "conversation/"+name, 0o600, fi.Size(), io.LimitReader(in, fi.Size()))
}

func writeEntry(tw *tar.Writer, name string, mode int64, size int64, r io.Reader) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: size, Typeflag: tar.TypeReg, Format: tar.FormatPAX}); err != nil {
		return err
	}
	n, err := io.Copy(tw, r)
	if err == nil && n != size {
		err = fmt.Errorf("%s: read %d bytes of %d", name, n, size)
	}
	return err
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// cleanName is name as a plain relative path, and whether it is one: no
// absolute path, no "..", nothing empty.
func cleanName(name string) (string, bool) {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return "", false
	}
	c := path.Clean(name)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", false
	}
	return c, true
}

// errArchive is an archive this runner will not open: what the hub sent is
// not one, or not one it can take. Its message says which.
type errArchive string

func (e errArchive) Error() string { return string(e) }

// unpackArchive reads the archive at file, writing the conversation's files
// under dir — which it makes, 0700 — and returns its manifest. It takes
// regular files and directories only, each under conversation/ or the
// manifest itself, and none outside dir: a link, a device or a path climbing
// out is refused, and so is an archive past the bounds above.
func unpackArchive(file, dir string) (manifest, error) {
	f, err := os.Open(file)
	if err != nil {
		return manifest{}, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return manifest{}, errArchive("the archive is not gzip: " + err.Error())
	}
	defer gz.Close()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return manifest{}, err
	}
	tr := tar.NewReader(gz)
	var (
		m       manifest
		found   bool
		total   int64
		entries int
	)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return manifest{}, errArchive("the archive is not a readable tar: " + err.Error())
		}
		if entries++; entries > maxArchiveFiles {
			return manifest{}, errArchive(fmt.Sprintf("the archive holds more than %d files", maxArchiveFiles))
		}
		if h.Name == "manifest.json" && h.Typeflag == tar.TypeReg {
			if h.Size > maxManifestBytes {
				return manifest{}, errArchive("the archive's manifest is too large")
			}
			body, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes))
			if err != nil {
				return manifest{}, errArchive("the archive's manifest could not be read: " + err.Error())
			}
			if err := json.Unmarshal(body, &m); err != nil {
				return manifest{}, errArchive("the archive's manifest is not JSON: " + err.Error())
			}
			found = true
			continue
		}
		rest, ok := strings.CutPrefix(h.Name, "conversation/")
		name, clean := cleanName(strings.TrimSuffix(rest, "/"))
		if !ok || !clean {
			return manifest{}, errArchive(fmt.Sprintf("the archive holds %q, outside its conversation", h.Name))
		}
		to := filepath.Join(dir, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(to, 0o700); err != nil {
				return manifest{}, err
			}
		case tar.TypeReg:
			if total += h.Size; h.Size < 0 || total > maxUnpackedBytes {
				return manifest{}, errArchive(fmt.Sprintf("the archive unpacks to more than %d bytes", int64(maxUnpackedBytes)))
			}
			if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
				return manifest{}, err
			}
			out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return manifest{}, err
			}
			n, err := io.Copy(out, io.LimitReader(tr, h.Size))
			if cerr := out.Close(); err == nil {
				err = cerr
			}
			if err == nil && n != h.Size {
				err = errArchive(fmt.Sprintf("the archive's %s is cut short", h.Name))
			}
			if err != nil {
				return manifest{}, err
			}
		default:
			return manifest{}, errArchive(fmt.Sprintf("the archive's %s is not a plain file or directory", h.Name))
		}
	}
	if !found {
		return manifest{}, errArchive("the archive has no manifest")
	}
	if m.Format != archiveFormat {
		return manifest{}, errArchive(fmt.Sprintf("the archive is format %d, and this runner reads format %d — upgrade yad on it", m.Format, archiveFormat))
	}
	return m, nil
}

// fileSHA256 is the sha256 of a file's bytes, and their count.
func fileSHA256(file string) (string, int64, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
