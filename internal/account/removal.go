package account

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/skkap/yad/internal/config"
)

// A removal outlives the process that started it in one way only: a marker
// file inside the home, written before config.toml drops the label (DEV-160,
// decision 0070). Everything else a removal knows — that a run is still in
// the home and the home goes when it ends, or that the rename out of the
// account's path failed — is in the daemon's memory, and a daemon killed
// before it acts leaves a home config.toml no longer names. That home is
// indistinguishable from the one a `yad account add` in progress, or walked
// away from, keeps on purpose for the next try (decision 0043), which must
// never be deleted. The marker is the difference: the daemon's next start
// finishes every unlisted home that carries it (Unfinished, Finish), and
// touches none that does not.
//
// The home rather than state.db, because the CLI never writes state.db
// (decision 0043) and `yad account remove` is one of the writers; and a mark
// rather than a move, because a run still in the home keeps paths under it
// open, and moving it from under a running claude is not something to find
// out about in production.

// removingMarker is the marker's name inside the home: a dotfile with a
// name neither harness uses, beside what each keeps there by names of its
// own. It sits at the top of the home, and nothing in sharedConfig or
// transcriptDir is called this, so it is never written through one of the
// links a home holds into the shared transcripts or the owner's own
// configuration, where every account and the owner's own harness would see it.
const removingMarker = ".yad-removing"

// markerText is what the marker says to an owner who finds it. Nothing reads
// it back: the marker's presence is the whole signal.
const markerText = "yad is removing this account. Its daemon deletes this directory, with any login the harness keeps for it outside it, when the last run in it ends or at the daemon's next start.\n"

func markerPath(data, harness, label string) string {
	return filepath.Join(HomeDir(data, harness, label), removingMarker)
}

// Mark writes the marker in an account's home, 0600, and makes it durable
// before returning: the write that drops the label from config.toml comes
// next, and a power cut that kept that write and lost this one would leave
// exactly the home this exists to tell apart. No home is nothing to mark —
// an account removed before it was ever logged in has no home to leave
// behind, and a Keychain login left for its path is Prepare's to clear.
//
// Never through a link: a home is yad's own directory, but a marker name
// that is a symlink would have the write land wherever it points.
func Mark(data, harness, label string) error {
	if err := checkNames(harness, label); err != nil {
		return err
	}
	home := HomeDir(data, harness, label)
	switch fi, err := os.Lstat(home); {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	case !fi.IsDir():
		// Not a home yad made. SetAside moves it out of the path all the
		// same; the start sweep has no business deleting what it is.
		return nil
	}
	path := filepath.Join(home, removingMarker)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("mark %s for removal: %w", home, err)
	}
	// Chmod as well as the mode above: an existing file keeps its own, and
	// umask may narrow a new one but never widen it.
	err = errors.Join(f.Chmod(0o600), write(f, markerText), f.Sync(), f.Close())
	if err == nil {
		err = syncDir(home)
	}
	if err != nil {
		return fmt.Errorf("mark %s for removal: %w", home, err)
	}
	return nil
}

func write(f *os.File, s string) error {
	_, err := f.WriteString(s)
	return err
}

// syncDir makes a new name in dir durable: fsync on the file alone makes its
// contents durable, not the entry that finds it.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

// Marked says whether an account's home carries the marker: a regular file,
// since only Mark makes one and it never makes anything else.
func Marked(data, harness, label string) bool {
	if checkNames(harness, label) != nil {
		return false
	}
	fi, err := os.Lstat(markerPath(data, harness, label))
	return err == nil && fi.Mode().IsRegular()
}

// Unmark takes the marker off an account's home: the label is being added
// again, and the home is the one being logged in now. A marker already gone,
// or a home that is not there, is no error.
func Unmark(data, harness, label string) error {
	if err := checkNames(harness, label); err != nil {
		return err
	}
	if err := os.Remove(markerPath(data, harness, label)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("take the removal marker off %s: %w", HomeDir(data, harness, label), err)
	}
	return nil
}

// Unlist is the start of every removal that changes config.toml: the home is
// marked, and then the label is taken off the harness's list, under
// config.toml's lock in that order. A marker that cannot be written leaves the
// file as it was, so a removal either leaves its label listed or has left a
// mark the next start finishes — never an unlisted home with nothing to say
// it is going.
//
// mark says, from whether the file lists the label now, whether to mark its
// home: the owner's own removal marks whatever it finds, since the owner
// asked for that home to go; a hub's marks only a label something lists,
// since a home under a label nothing lists may be a `yad account add` in
// progress (decision 0043) and a hub asking about it is no reason to doom it.
func Unlist(ctx context.Context, p config.Paths, harness, label string, mark func(listed bool) bool) (listed bool, err error) {
	if err := checkNames(harness, label); err != nil {
		return false, err
	}
	var markErr error
	_, err = config.UpdateAccounts(ctx, p, harness, func(labels []string) []string {
		listed = slices.Contains(labels, label)
		if mark(listed) {
			if markErr = Mark(p.Data, harness, label); markErr != nil {
				return labels
			}
		}
		return config.WithoutAccount(label)(labels)
	})
	if err == nil && markErr != nil {
		err = fmt.Errorf("%w, so config.toml still lists the account", markErr)
	}
	return listed, err
}

// SetAsideMarked is SetAside for a home that carries the marker, and nothing
// for one that does not: a home with no marker is never a removal's to move.
// It says whether it set one aside.
func SetAsideMarked(data, harness, label string) (bool, error) {
	if !Marked(data, harness, label) {
		return false, nil
	}
	return true, SetAside(data, harness, label)
}

// Finish completes a removal the process that started it did not: a marked
// home at the account's path is set aside, and every set-aside copy is
// deleted with, on macOS, the Keychain login (RemoveSetAside). The caller has
// made sure config.toml does not list the label and that nothing is in the
// home; an unmarked home is left where it is either way.
func Finish(data, harness, label string) error {
	if _, err := SetAsideMarked(data, harness, label); err != nil {
		return err
	}
	return RemoveSetAside(data, harness, label)
}
