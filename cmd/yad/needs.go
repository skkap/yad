package main

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/selfupdate"
)

// needsEvery is how often the daemon looks at config.toml for a connection
// added or removed. A self-update checks every six hours, so a minute is
// nothing to it; what it buys is that `yad connect` and `yad disconnect`,
// which do not all reach the daemon, need not tell it.
const needsEvery = time.Minute

// needsRecorder keeps this profile's needs record beside the binary it runs
// (decision 0078): every daemon, whether or not its own self-update is on,
// since another profile's update replaces the binary for it too.
type needsRecorder struct {
	paths     config.Paths
	configDir string
	// dir is the needs directory beside the binary; empty when the binary
	// could not be found, and then lost says why.
	dir   string
	lost  string
	exe   string
	every time.Duration
	log   *slog.Logger

	mu sync.Mutex
	// stamp is config.toml as it was when protocols was read from it.
	stamp      os.FileInfo
	protocols  map[string][]string
	unrecorded string
	// noted is what was last logged: why, and whether it was a warning.
	noted string
}

// recordNeeds writes the record for cfg, the configuration the daemon
// started with, and keeps it current until ctx ends. Nothing it meets stops
// the runner: a record it cannot write is a warning in the log and a line in
// `yad status` — or, beside a binary only root may replace, a note in the
// log alone.
func recordNeeds(ctx context.Context, p config.Paths, cfg config.Config, exe string, log *slog.Logger) *needsRecorder {
	// No stamp yet, so the first refresh reads config.toml again and keeps
	// the stat beside what it read: a `yad connect` that landed while the
	// daemon was starting is in the first record. cfg is what the record
	// says only if that read fails.
	r := &needsRecorder{paths: p, exe: exe, every: needsEvery, log: log, protocols: connectionProtocols(cfg)}
	var err error
	if r.configDir, err = filepath.Abs(p.Config); err != nil {
		r.lost = fmt.Sprintf("the config directory %s has no absolute path: %v", p.Config, err)
	} else if r.dir, err = selfupdate.NeedsDir(exe); err != nil {
		r.lost = err.Error()
	}
	r.refresh()
	go r.run(ctx)
	return r
}

func (r *needsRecorder) run(ctx context.Context) {
	if r.dir == "" {
		return
	}
	t := time.NewTicker(r.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.refresh()
		}
	}
}

// Unrecorded is why the record is not beside the binary, for `yad status`;
// empty when it is.
func (r *needsRecorder) Unrecorded() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unrecorded
}

// refresh reads config.toml again when it has changed, and writes the record
// when it is missing or says something else — another process may have
// removed it, or the binary been replaced by a release whose daemon wrote it
// in another shape.
func (r *needsRecorder) refresh() {
	if r.dir == "" {
		r.note(r.lost, false)
		return
	}
	fi, err := os.Stat(r.paths.ConfigFile())
	if err != nil {
		// No config.toml is a profile being taken apart: its record is
		// removed by the next updater that sees it, and writing it again
		// would only undo that.
		return
	}
	r.mu.Lock()
	changed := r.stamp == nil || !fi.ModTime().Equal(r.stamp.ModTime()) || fi.Size() != r.stamp.Size()
	r.mu.Unlock()
	if changed {
		// A config.toml that does not load now is half an edit, or a
		// mistake the next start reports: the record keeps what was last
		// read.
		if cfg, err := config.Load(r.paths); err == nil {
			r.mu.Lock()
			r.stamp, r.protocols = fi, connectionProtocols(cfg)
			r.mu.Unlock()
		}
	}
	r.mu.Lock()
	want := selfupdate.Record{ConfigDir: r.configDir, Profile: r.paths.Profile, Protocols: maps.Clone(r.protocols), Version: buildinfo.Version}
	r.mu.Unlock()
	if want.Protocols == nil {
		want.Protocols = map[string][]string{}
	}
	if have, ok, err := selfupdate.ReadRecord(r.dir, r.configDir); err == nil && ok && sameNeeds(have, want) {
		r.note("", false)
		return
	}
	want.Written = time.Now().UTC()
	if err := selfupdate.WriteRecord(r.dir, want); err != nil {
		// Quiet only beside a binary no profile but root's could update: this
		// user cannot replace it, and nobody but root may write where it
		// is. A root-owned yad in /usr/local/bin, as the work-machine kit
		// installs it with self-update off, would otherwise warn at every
		// start for nothing — a runner as root is not how yad is run, and
		// the kit holds one runner. A directory another user owns is
		// another matter: that user's runner may update the binary, and
		// would need this record, so not writing it is a warning.
		quiet := !selfupdate.CanReplace(r.exe) && selfupdate.OnlyRoot(r.exe)
		r.note(fmt.Sprintf("cannot write it in %s: %v", r.dir, err), quiet)
		return
	}
	r.note("", false)
}

// note keeps why the record is missing — for `yad status`, unless quiet: only
// root may update the binary — and logs only a change, so a
// directory that stays unwritable is one line and not one a minute.
func (r *needsRecorder) note(why string, quiet bool) {
	key := why
	if quiet {
		key = "quiet: " + why
	}
	r.mu.Lock()
	was := r.noted
	r.noted = key
	r.unrecorded = why
	if quiet {
		r.unrecorded = ""
	}
	r.mu.Unlock()
	switch {
	case key == was:
	case why == "":
		r.log.Info("this profile's protocol needs are recorded beside the binary it runs", "dir", r.dir)
	case quiet:
		r.log.Info("this profile's protocol needs are not recorded beside the binary it runs; only root may replace that binary, so no runner that would read them can update it (decision 0078)", "reason", why, "binary", r.exe)
	default:
		r.log.Warn("this profile's protocol needs are not recorded beside the binary it runs, so another profile that updates the binary itself would not see them (decision 0078); the runner carries on", "reason", why)
	}
}

func sameNeeds(a, b selfupdate.Record) bool {
	return a.ConfigDir == b.ConfigDir && a.Profile == b.Profile && a.Version == b.Version &&
		maps.EqualFunc(a.Protocols, b.Protocols, slices.Equal)
}
