package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword"
)

// NeedsDirName is the directory beside a binary where every profile that runs
// it says which protocol majors its hubs need (decision 0078). It sits beside
// the binary, not under any profile, because the binary is the one thing the
// profiles share and the only place an updater replacing it is sure to look:
// each profile may keep its own directories anywhere.
const NeedsDirName = ".yad-needs"

// maxRecord bounds what is read of one record: a profile's record names its
// config directory and its connections, a few hundred bytes.
const maxRecord = 64 << 10

// Record is one profile's needs of the binary it runs.
type Record struct {
	// ConfigDir is the profile's config directory, absolute: what names the
	// profile, and what says it still exists, by holding a config.toml.
	ConfigDir string `json:"config_dir"`
	Profile   string `json:"profile"`
	// Protocols is each protocol major the profile's connections sync over,
	// with the connections that do; empty, never absent, for a profile with
	// no hub. A record without it is not one this build can read.
	Protocols map[string][]string `json:"protocols"`
	// Version is the yad that wrote it, and Written when: for a person
	// reading the directory, not for the vet.
	Version string    `json:"version"`
	Written time.Time `json:"written"`
}

// NeedsDir is where the profiles running binary keep their records: beside
// the file binary names once symlinks are resolved, which is the file a
// release is renamed over (upgrade.Apply), so /usr/local/bin/yad linked to
// ~/.local/bin/yad keeps its records in ~/.local/bin.
func NeedsDir(binary string) (string, error) {
	real, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return "", fmt.Errorf("cannot find the binary at %s: %w", binary, err)
	}
	return filepath.Join(filepath.Dir(real), NeedsDirName), nil
}

// recordName is a profile's file in the needs directory. A hash, because the
// config directory is a path, and two profiles never share one.
func recordName(configDir string) string {
	h := sha256.Sum256([]byte(configDir))
	return hex.EncodeToString(h[:16]) + ".json"
}

// WriteRecord puts r in dir, replacing the profile's last one in one rename,
// so an updater reading at the same moment sees the old record or the new and
// never half of one. The file is 0644: it holds no secret, and an updater run
// by another user of the same binary has to be able to read it.
func WriteRecord(dir string, r Record) error {
	if !filepath.IsAbs(r.ConfigDir) {
		return fmt.Errorf("a needs record names its profile by an absolute config directory, not %q", r.ConfigDir)
	}
	if r.Protocols == nil {
		r.Protocols = map[string][]string{}
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := realDir(dir); err != nil {
		return err
	}
	shareLike(dir)
	name := filepath.Join(dir, recordName(r.ConfigDir))
	// A leading dot, so a reader passing over a write in progress skips it.
	tmp, err := os.CreateTemp(dir, "."+recordName(r.ConfigDir)+".*")
	if err != nil {
		// Without the temporary name, which differs at every attempt: the
		// daemon logs a reason only when it changes.
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return fmt.Errorf("cannot create a file in %s: %w", dir, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// realDir refuses a needs directory that is anything but a directory: a link
// planted beside a binary in a directory others may write would send records
// — and shareLike's chmod — wherever it points.
func realDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		return nil
	}
	what := "a file"
	if fi.Mode()&os.ModeSymlink != 0 {
		what = "a symbolic link"
	}
	return fmt.Errorf("%s is %s, not a directory, and yad neither writes records through it nor reads them from it — `%s` removes it, if no one made it on purpose",
		dir, what, shellword.Command("rm", dir))
}

// shareLike opens the needs directory to whoever may write the directory
// holding the binary: a group-writable bin directory shared by profiles of
// several users lets each of them replace the binary, so each has to be able
// to record what it needs beside it, whoever made the directory first. The
// sticky bit keeps each user to their own records, as in /tmp. A sticky bin
// directory is left alone: there no user may replace another's binary, so
// none needs to write beside it. Best effort: a directory another user made,
// and left narrower, fails the write that follows, which says so.
func shareLike(dir string) {
	parent, err := os.Stat(filepath.Dir(dir))
	if err != nil || parent.Mode()&os.ModeSticky != 0 {
		return
	}
	open := parent.Mode().Perm() & 0o022
	if open == 0 {
		return
	}
	// Checked again here, not only by the caller, since this is the one
	// place that widens anything.
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		return
	}
	// Readable by all, as every record is, and writable by whoever may
	// write beside the binary. Linux gives a new directory its maker's
	// group, not its parent's, so the parent's is set too.
	if fi.Mode().Perm()&open != open || fi.Mode()&os.ModeSticky == 0 {
		os.Chmod(dir, 0o755|open|os.ModeSticky)
	}
	chownGroup(dir, parent)
}

// ReadRecord is the record dir holds for the profile at configDir, and whether
// there is one: the owning daemon compares what it would write with it.
func ReadRecord(dir, configDir string) (Record, bool, error) {
	if err := realDir(dir); errors.Is(err, fs.ErrNotExist) {
		return Record{}, false, nil
	} else if err != nil {
		return Record{}, false, err
	}
	r, err := readRecord(filepath.Join(dir, recordName(configDir)))
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, false, nil
	}
	return r, err == nil, err
}

// readRecord reads one record, and nothing that is not a regular file: a
// directory another user may write can hold a FIFO under a record's name,
// and a daemon blocked opening it would never start.
func readRecord(path string) (Record, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|openNonblock, 0)
	if err != nil {
		return Record{}, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Record{}, err
	}
	if !fi.Mode().IsRegular() {
		return Record{}, fmt.Errorf("it is not a regular file (%s)", fi.Mode().Type())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxRecord+1))
	if err != nil {
		return Record{}, err
	}
	if len(b) > maxRecord {
		return Record{}, fmt.Errorf("it is over %d bytes, which no record is", maxRecord)
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, fmt.Errorf("it is not JSON this build reads: %w", err)
	}
	if !filepath.IsAbs(r.ConfigDir) {
		return Record{}, fmt.Errorf("it names no profile by an absolute config directory (config_dir %q)", r.ConfigDir)
	}
	if r.Protocols == nil {
		return Record{}, errors.New("it has no protocols field, so what its profile needs cannot be told")
	}
	return r, nil
}

// unreadable is a record no updater can read: it may be a live profile's, and
// what that profile needs cannot be told, so no release is taken while it is
// there. It is a failure and not a refusal, since removing it, or its daemon
// writing it again, ends it before the next check.
type unreadable struct {
	path string
	err  error
}

func (u unreadable) Error() string {
	return fmt.Sprintf("the needs record %s, which some profile running this binary wrote, cannot be read (%v), so what that profile's hubs need cannot be told — nothing was downloaded. A running daemon writes its own again within a minute; `%s` removes it if its profile is gone",
		u.path, u.err, shellword.Command("rm", u.path))
}

// records reads every record in dir. The live ones are returned, by path;
// those whose profile is gone — its config directory holds no config.toml —
// are removed, since nothing will start that profile again to need anything.
// A directory that does not exist holds no record: no profile has written one
// yet.
func records(dir string) (map[string]Record, error) {
	if err := realDir(dir); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("what the other profiles running this binary need cannot be told — nothing was downloaded: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot read the needs records in %s, so what the other profiles running this binary need cannot be told — nothing was downloaded: %w", dir, err)
	}
	out := map[string]Record{}
	for _, e := range entries {
		name := e.Name()
		// A write in progress, and anything a person put here.
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") || e.IsDir() {
			continue
		}
		path := filepath.Join(dir, name)
		r, err := readRecord(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // removed since the listing
		}
		if err != nil {
			return nil, unreadable{path: path, err: err}
		}
		if _, err := os.Stat(config.Paths{Config: r.ConfigDir}.ConfigFile()); errors.Is(err, fs.ErrNotExist) {
			// Gone. Any other error — a directory this user may not read —
			// leaves the profile counted: it may well be there.
			os.Remove(path)
			continue
		}
		out[path] = r
	}
	return out, nil
}

// need is what one release must speak: each protocol major, and who syncs
// over it.
type need struct {
	own    []string
	others []Record
}

// gather is this runner's own needs and every other live profile's, by major.
// The record of the profile at self is this runner's too: config.toml as it
// read when the record was last written, which adds a connection made since
// this process started — the release it takes over as syncs over that one.
func gather(own map[string][]string, self string, recs map[string]Record) map[string]*need {
	all := map[string]*need{}
	at := func(major string) *need {
		if all[major] == nil {
			all[major] = &need{}
		}
		return all[major]
	}
	mine := func(major string, conns []string) {
		n := at(major)
		for _, c := range conns {
			if !slices.Contains(n.own, c) {
				n.own = append(n.own, c)
			}
		}
	}
	for _, major := range slices.Sorted(maps.Keys(own)) {
		if len(own[major]) > 0 {
			mine(major, own[major])
		}
	}
	for _, path := range slices.Sorted(maps.Keys(recs)) {
		r := recs[path]
		for _, major := range slices.Sorted(maps.Keys(r.Protocols)) {
			switch {
			case len(r.Protocols[major]) == 0:
			case r.ConfigDir == self:
				mine(major, r.Protocols[major])
			default:
				n := at(major)
				n.others = append(n.others, r)
			}
		}
	}
	return all
}

// missing is what a release speaking speaks leaves out, each as the words of
// a refusal; nil when it speaks everything needed.
func missing(needs map[string]*need, speaks []string) []string {
	var gone []string
	for _, major := range slices.Sorted(maps.Keys(needs)) {
		if slices.Contains(speaks, major) {
			continue
		}
		n := needs[major]
		var who []string
		if len(n.own) > 0 {
			who = append(who, fmt.Sprintf("%s %s", strings.Join(n.own, ", "), syncsOver(len(n.own))))
		}
		for _, r := range n.others {
			conns := r.Protocols[major]
			who = append(who, fmt.Sprintf("profile %s's %s %s (its config is in %s, and it runs this binary too)",
				r.Profile, strings.Join(conns, ", "), syncsOver(len(conns)), r.ConfigDir))
		}
		gone = append(gone, fmt.Sprintf("v%s, which %s", major, strings.Join(who, "; and ")))
	}
	return gone
}
