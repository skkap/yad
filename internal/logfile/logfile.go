// Package logfile is the daemon's log on disk: slog JSON lines in one file,
// rotated by size into numbered backups, read back by `yad daemon logs`.
// Stdlib only — rotation is a rename and a counter, not worth a dependency.
package logfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Defaults for the daemon's file: a busy runner writes a few megabytes a day,
// so four files of 10 MiB keep days of history in bounded space.
const (
	DefaultMaxBytes = 10 << 20
	DefaultBackups  = 3
)

// File is an append-only log that rotates when a write would take it past
// MaxBytes: path becomes path.1, path.1 becomes path.2, and the oldest past
// Backups is deleted. A write is never split across two files, so every JSON
// line stays whole.
type File struct {
	path     string
	maxBytes int64
	backups  int

	mu     sync.Mutex
	f      *os.File // nil after a reopen failed; the next Write tries again
	size   int64
	closed bool
	// retryAt is the size at which a rotation that failed is tried again,
	// so a directory that refuses renames costs one attempt per MaxBytes
	// written rather than one per line.
	retryAt int64
}

// Open opens path for appending, creating it and its directory private to the
// owner: a log names runs, sessions and hubs.
func Open(path string, maxBytes int64, backups int) (*File, error) {
	if maxBytes <= 0 || backups < 1 {
		return nil, fmt.Errorf("logfile: maxBytes %d and backups %d must be positive", maxBytes, backups)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &File{path: path, maxBytes: maxBytes, backups: backups}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *File) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size, l.retryAt = f, fi.Size(), 0
	return nil
}

// Write appends p, rotating first when p would not fit. A log that cannot
// rotate keeps logging into the one file, and one whose file could not be
// reopened tries again on every write: losing lines is worse than an
// oversized file, and a daemon that goes silent for good is worse than both.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, fs.ErrClosed
	}
	if l.f != nil && l.size > 0 && l.size+int64(len(p)) > l.maxBytes && l.size >= l.retryAt {
		if err := l.rotate(); err != nil {
			fmt.Fprintf(os.Stderr, "yad: rotate %s: %v\n", l.path, err)
			l.retryAt = l.size + l.maxBytes
		}
	}
	if l.f == nil {
		if err := l.open(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotate shifts the files along and reopens the live one. Whatever fails, the
// old descriptor is let go and a reopen is attempted, so the file is never left
// pointing at something closed.
func (l *File) rotate() error {
	errs := []error{l.f.Close()}
	l.f = nil
	for i := l.backups; i >= 1; i-- {
		from := Backup(l.path, i-1)
		if err := os.Rename(from, Backup(l.path, i)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	// Backups past the limit, left by an earlier run with a higher one.
	os.Remove(Backup(l.path, l.backups+1))
	errs = append(errs, l.open())
	return errors.Join(errs...)
}

// Close closes the file.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Backup is the path of the nth file back: 0 is the live file itself.
func Backup(path string, n int) string {
	if n == 0 {
		return path
	}
	return fmt.Sprintf("%s.%d", path, n)
}
