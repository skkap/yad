package logfile

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"
)

// Tail returns the last n lines of the log, reaching back into the backups
// when the live file has fewer, oldest first.
func Tail(path string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	var lines []string
	for i := 0; len(lines) < n; i++ {
		b, err := os.ReadFile(Backup(path, i))
		if errors.Is(err, fs.ErrNotExist) {
			if i == 0 {
				continue // rotated just now, or never written: the backups may still hold lines
			}
			break
		}
		if err != nil {
			return nil, err
		}
		got := splitLines(b)
		if want := n - len(lines); len(got) > want {
			got = got[len(got)-want:]
		}
		lines = append(got, lines...)
	}
	return lines, nil
}

func splitLines(b []byte) []string {
	var out []string
	for line := range bytes.Lines(b) {
		out = append(out, string(bytes.TrimRight(line, "\n")))
	}
	return out
}

// pollEvery is how often Follow looks for new lines. Polling, not a file
// watcher: it needs no dependency and works the same on macOS and Linux, and a
// quarter-second lag is invisible to someone reading logs.
const pollEvery = 250 * time.Millisecond

// followReady is called once Follow is watching, from the end of the file.
// A test that appends has to know the follower is in place first, and the only
// alternative is a sleep long enough to be sure on the slowest machine that
// will ever run it — which every green run then pays, and a slower one still
// breaks (DEV-55). Nothing in production assigns it.
var followReady = func() {}

// Follow writes each whole line appended to the log from its current end,
// following it across rotation, until ctx ends.
func Follow(ctx context.Context, path string, w io.Writer) error {
	t := tailer{w: w}
	defer t.close()
	if err := t.open(path, true); err != nil {
		return err
	}
	followReady()
	for {
		if err := t.drain(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(pollEvery):
		}
		fi, err := os.Stat(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Between rotation's rename and the new file: next poll.
		case err != nil:
			return err
		case t.info == nil || !os.SameFile(fi, t.info):
			// Rotated: what reached the old file after the last read is
			// still readable through its descriptor, and comes first.
			if err := t.drain(); err != nil {
				return err
			}
			// Several rotations between two polls leave whole files the
			// follower never opened, between its old file and the live one.
			for _, b := range t.skipped(path) {
				if err := t.open(b, false); err != nil {
					return err
				}
				if err := t.drain(); err != nil {
					return err
				}
			}
			if err := t.open(path, false); err != nil {
				return err
			}
		case fi.Size() < t.off:
			t.off, t.partial = 0, nil // truncated in place
		}
	}
}

type tailer struct {
	w       io.Writer
	f       *os.File
	info    os.FileInfo
	off     int64
	partial []byte
}

// skipped lists, oldest first, the backups rotated in after the file the
// follower holds. When that file has itself been rotated out of the kept set,
// every backup is newer than it.
func (t *tailer) skipped(path string) []string {
	var newer []string
	for i := 1; ; i++ {
		fi, err := os.Stat(Backup(path, i))
		if err != nil || (t.info != nil && os.SameFile(fi, t.info)) {
			break
		}
		newer = append([]string{Backup(path, i)}, newer...)
	}
	return newer
}

func (t *tailer) open(path string, fromEnd bool) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	t.close()
	t.f, t.info, t.off, t.partial = f, fi, 0, nil
	if fromEnd {
		t.off = fi.Size()
	}
	return nil
}

// drain writes every whole line past the offset; a line still missing its
// newline waits for the rest.
func (t *tailer) drain() error {
	if t.f == nil {
		return nil
	}
	r := bufio.NewReader(io.NewSectionReader(t.f, t.off, 1<<62))
	for {
		line, err := r.ReadBytes('\n')
		t.off += int64(len(line))
		t.partial = append(t.partial, line...)
		if err != nil {
			return nil
		}
		if _, err := t.w.Write(t.partial); err != nil {
			return err
		}
		t.partial = t.partial[:0]
	}
}

func (t *tailer) close() {
	if t.f != nil {
		t.f.Close()
		t.f = nil
	}
}
