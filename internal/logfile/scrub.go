package logfile

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Scrub replaces, in the live file and in every backup, each old string of
// oldnew with the new one after it. It is for text that should never have
// been logged: the credential a version before decision 0068 wrote into a
// source URL it quoted.
//
// Where two olds match at one place the longer wins, so a credential that
// holds another as its start is taken out whole. The olds are what must go,
// and a file that holds one and cannot be rewritten is removed instead: its
// lines are worth less than what they hold. removed names each file that went
// so; err is for a file that could be neither rewritten nor removed, and a
// caller that must not lose the olds keeps them until a Scrub returns nil.
//
// The live file is rewritten in place, through a second descriptor, while
// writes wait on the lock, and every line in it keeps its length: what came
// out is made up in spaces before its newline, which JSON reads past. So
// nothing after a changed line moves — the appending descriptor goes on at
// the same end, and `yad daemon logs --follow`, which reads on from the
// offset it had reached, neither misses a line nor starts one in its middle.
// Hence no new may be longer than its old. What this gives up to a rename's
// atomicity: power lost before the sync can keep any page of the file as it
// was, and a rewritten line that crosses into such a page keeps what of an
// old lay past the boundary — never the whole of one, which a retry would
// find, but a tail no old matches any more. A rename would instead make a
// follower take the live file for a rotated one and print every backup again.
// A backup is replaced whole, through a file beside it.
func (l *File) Scrub(oldnew ...string) (removed []string, err error) {
	if len(oldnew)%2 != 0 {
		return nil, errors.New("logfile: Scrub takes old and new strings in pairs")
	}
	type pair struct{ old, new string }
	var pairs []pair
	for i := 0; i < len(oldnew); i += 2 {
		if len(oldnew[i+1]) > len(oldnew[i]) {
			return nil, errors.New("logfile: Scrub cannot replace a string with a longer one in place")
		}
		// An empty old would match between every two bytes.
		if oldnew[i] != "" {
			pairs = append(pairs, pair{oldnew[i], oldnew[i+1]})
		}
	}
	if len(pairs) == 0 {
		return nil, nil
	}
	// strings.Replacer tries its olds in argument order at each place.
	slices.SortStableFunc(pairs, func(a, b pair) int { return cmp.Compare(len(b.old), len(a.old)) })
	args := make([]string, 0, 2*len(pairs))
	for _, p := range pairs {
		args = append(args, p.old, p.new)
	}
	r := strings.NewReplacer(args...)

	l.mu.Lock()
	defer l.mu.Unlock()
	dir, base := filepath.Dir(l.path), filepath.Base(l.path)
	// Listed rather than counted up to backups: an earlier daemon may have
	// kept more, and a gap — a backup deleted by hand — must not end the
	// search.
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		live := name == base
		n, ok := strings.CutPrefix(name, base+".")
		if !live && (!ok || n == "" || strings.Trim(n, "0123456789") != "") {
			continue
		}
		path := filepath.Join(dir, name)
		var err error
		if live {
			err = l.scrubLive(r)
		} else {
			err = rewrite(path, r)
		}
		if err == nil {
			continue
		}
		if live && l.f != nil {
			l.f.Close()
			l.f = nil
		}
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("%w, and it could not be removed either: %w", err, rmErr))
			continue
		}
		removed = append(removed, path)
		// The next Write reopens it, as after a failed rotation.
		if live && !l.closed {
			if err := l.open(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return removed, errors.Join(errs...)
}

// scrubLive rewrites the live file in place; the caller holds l.mu.
func (l *File) scrubLive(r *strings.Replacer) error {
	f, err := os.OpenFile(l.path, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	// Reads go through f's offset and writes land behind it, at lines
	// already read, so the buffer never holds a byte that has since changed.
	br := bufio.NewReader(f)
	var off int64
	changed := false
	for {
		line, err := br.ReadString('\n')
		if body := strings.TrimSuffix(line, "\n"); body != "" {
			if s := r.Replace(body); s != body {
				padded := s + strings.Repeat(" ", len(body)-len(s)) + line[len(body):]
				if _, err := f.WriteAt([]byte(padded), off); err != nil {
					return err
				}
				changed = true
			}
		}
		off += int64(len(line))
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if !changed {
		return nil
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}

// rewrite replaces the file at path with its text through r, whole or not at
// all, keeping its mode.
func rewrite(path string, r *strings.Replacer) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // deleted by hand since the listing
	}
	if err != nil {
		return err
	}
	s := r.Replace(string(b))
	if s == string(b) {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".scrub-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(s)
	if err == nil {
		err = tmp.Chmod(fi.Mode().Perm())
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
