package runner

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
)

// destroyGrants removes dir, a tree of grant files, and when it cannot remove
// it destroys what the files hold. It is the one way grant files leave the
// disk: the run's own cleanup and the sweep at the next start both call it, so
// a reason that defeats one does not quietly defeat the other the same way.
//
// Unlinking a file needs write permission on its directory; overwriting it
// needs write permission on the file. So when removal fails the contents can
// usually still be destroyed, and a secret that would otherwise sit on disk
// until someone read a log line becomes an empty file. A read-only mount or an
// immutable file defeats both, and that is what the log line is for.
//
// It reports whether dir is gone. Whatever it logs names dir and counts,
// never a file under it: a grant's name says as much about what a hub sent as
// its value does.
func destroyGrants(dir string, log *slog.Logger) bool {
	err := os.RemoveAll(dir)
	if err == nil {
		return true
	}
	writableDirs(dir)
	if err = os.RemoveAll(dir); err == nil {
		return true
	}
	// Only now, when unlinking is out of reach: emptying writes through
	// whatever sits at a grant's path, and removal is the step that cannot
	// touch anything outside the tree.
	emptyErr := emptyGrantFiles(dir)
	files, holding, unseen := grantsLeft(dir)
	if holding == 0 && !unseen {
		log.Error("grant files could not be removed; their contents were destroyed and the empty files remain — delete them by hand",
			"dir", dir, "files", files, "err", pathReason(err))
		return false
	}
	log.Error("grant files could not be removed or emptied — the secrets a hub sent are still on disk; delete them by hand",
		"dir", dir, "files", files, "holding", holding, "err", pathReason(err), "empty_err", pathReason(emptyErr))
	return false
}

// writableDirs gives every directory under dir back its owner's write bit, so
// the removal after it can unlink what the missing bit held on to — a harness
// that chmods its surroundings, or an owner who did. It is safe because every
// directory here is one yad made 0700 in its own data directory: 0700 hands
// the owning user, the only one who can change the mode at all, nothing it
// could not give itself, and hands anyone else nothing. It never follows a
// symlink — WalkDir reports one as a link, not a directory — so it changes
// nothing outside the tree. Errors are the removal's to report.
func writableDirs(dir string) {
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		// Called for a directory before it is read, so a directory with no
		// read or search bit is opened by the chmod and then walked.
		if err == nil && d.IsDir() {
			os.Chmod(path, 0o700)
		}
		return nil
	})
}

// emptyGrantFiles overwrites each grant file under dir with zeros and then
// truncates it, which needs the file's write bit and not its directory's. The
// overwrite is for a file system that frees a truncated block without clearing
// it; on one that copies on write it buys nothing, and costs a few bytes.
//
// A harness runs as the owner and can replace a grant file with a link to
// anything the owner has, so a link is never written through: O_NOFOLLOW for a
// symlink, O_NONBLOCK so a FIFO swapped in cannot hang the cleanup, the opened
// file must still be a regular one, and it must have no other name — a hard
// link to an owner's file is a regular file too, and the grant yad wrote has
// exactly one. A file refused here is left whole and counted as still holding
// a secret. It returns the first error.
func emptyGrantFiles(dir string) error {
	var first error
	keep := func(err error) {
		if first == nil {
			first = err
		}
	}
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			keep(err)
			return nil
		}
		if d.Type().IsRegular() {
			if err := emptyFile(path); err != nil {
				keep(err)
			}
		}
		return nil
	})
	return first
}

func emptyFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return &fs.PathError{Op: "empty", Path: path, Err: errors.New("not a regular file")}
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || st.Nlink != 1 {
		return &fs.PathError{Op: "empty", Path: path, Err: errors.New("the file has another name, so emptying it would reach past the grant")}
	}
	if _, err := io.CopyN(f, zeros{}, fi.Size()); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	return f.Sync()
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// grantsLeft counts the files still under dir and how many of them still hold
// something. unseen is a directory the walk could not read, whose files are
// neither counted nor known to be empty.
func grantsLeft(dir string) (files, holding int, unseen bool) {
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			unseen = true
			return nil
		}
		if d.IsDir() {
			return nil
		}
		files++
		if fi, err := d.Info(); err != nil || fi.Size() > 0 {
			holding++
		}
		return nil
	})
	return files, holding, unseen
}

// pathReason is err without the path a *fs.PathError carries. Under a grant
// directory that path ends in a grant's name, which the log never says; the
// directory and the reason are what the owner acts on.
func pathReason(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
