package hostool

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/skkap/yad/internal/probe"
)

// Locate finds a host tool's binary the way detection does, so what a run
// uses is what the capability document advertised: the file YAD_<ID>_PATH
// names, or PATH's copy when that names nothing (0044, 0045).
func Locate(id string) (string, bool) {
	t, ok := Lookup(id)
	if !ok {
		return "", false
	}
	f := find(t)
	return f.Path, f.Path != ""
}

func find(t Tool) probe.Found { return probe.Find(t.EnvPath, t.Binary, t.VersionArgs) }

// LinksDir is where a profile keeps the links Links makes, under its data
// directory.
func LinksDir(data string) string { return filepath.Join(data, "host-tools") }

// defaultPath is what a child searches when the runner itself has no PATH at
// all — a daemon a service manager started bare. A PATH of the links alone
// would take /bin/sh and every other system command away from a harness that
// had them before; this is the search an unset PATH gets from execvp.
const defaultPath = "/usr/bin:/bin"

// linksMu serialises Links: every run starting on this runner reconciles the
// same directory, and two doing it at once would race each other's renames.
var linksMu sync.Mutex

// Links is the PATH a run's children search, so a harness's own git, gh and
// docker are the ones detection resolved rather than whatever the runner's
// PATH holds (0045). It returns "" when that PATH would be the runner's own,
// and the caller adds nothing.
//
// An override's file need not be called git, gh or docker —
// /opt/git-2.45/bin/git-2.45 is a fine thing to point YAD_GIT_PATH at — so its
// directory on PATH would not find it. dir instead holds a link named for each
// tool that only its override finds, and goes first. A tool PATH already
// resolves needs none: that is the copy a child finds anyway.
//
// It is rebuilt from the current detection every time it is asked, which is a
// stat or two per tool, so an override edited or deleted since the last run is
// what the next one sees. A link is replaced by rename, so a run already going
// never finds its tool missing mid-swap.
func Links(dir string) (string, error) {
	linksMu.Lock()
	defer linksMu.Unlock()
	linked := false
	for _, t := range Catalog() {
		want := ""
		if f := find(t); f.FromOverride {
			// Absolute, because a link's relative target is read from the
			// link's own directory, not from where the runner was started.
			abs, err := filepath.Abs(f.Path)
			if err != nil {
				return "", err
			}
			want = abs
		}
		link := filepath.Join(dir, t.Binary)
		have, err := os.Readlink(link)
		switch {
		case want == "":
			if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			continue
		case err == nil && have == want:
		default:
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", err
			}
			tmp := link + ".new"
			if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return "", err
			}
			if err := os.Symlink(want, tmp); err != nil {
				return "", err
			}
			if err := os.Rename(tmp, link); err != nil {
				os.Remove(tmp)
				return "", err
			}
		}
		linked = true
	}
	if !linked {
		return "", nil
	}
	path := os.Getenv("PATH")
	if path == "" {
		path = defaultPath
	}
	return "PATH=" + dir + string(os.PathListSeparator) + path, nil
}
