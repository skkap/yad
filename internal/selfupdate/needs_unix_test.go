//go:build unix

package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// CanReplace answers what upgrade.Apply would meet: a directory beside the
// real binary this user may write, and in a sticky one a binary of their own.
// A link is followed to the file it names.
func TestCanReplaceAsksWhatTheRenameNeeds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	bin := func(t *testing.T, mode os.FileMode) string {
		t.Helper()
		dir := t.TempDir()
		path := filepath.Join(dir, "yad")
		if err := os.WriteFile(path, nil, 0o555); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		return path
	}
	if !CanReplace(bin(t, 0o755)) {
		t.Error("a read-only binary in a directory this user may write cannot be replaced, says CanReplace; rename(2) can")
	}
	if !CanReplace(bin(t, 0o777|os.ModeSticky)) {
		t.Error("this user's own binary in a sticky directory cannot be replaced, says CanReplace")
	}
	locked := bin(t, 0o555)
	if CanReplace(locked) {
		t.Error("a binary in a directory this user may not write can be replaced, says CanReplace")
	}
	link := filepath.Join(t.TempDir(), "yad")
	if err := os.Symlink(locked, link); err != nil {
		t.Fatal(err)
	}
	if CanReplace(link) {
		t.Error("a writable link to a binary this user may not replace counts as replaceable")
	}
	if CanReplace(filepath.Join(t.TempDir(), "missing")) {
		t.Error("a binary that is not there can be replaced, says CanReplace")
	}
}

// OnlyRoot is a directory root owns and nobody else may write: a directory
// this user owns is not, whatever its mode.
func TestOnlyRoot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "yad")
	if err := os.WriteFile(path, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if os.Geteuid() != 0 && OnlyRoot(path) {
		t.Error("a directory this user owns counts as root's alone")
	}
	fi, err := os.Stat("/bin")
	if err != nil {
		t.Skip(err)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid == 0 && fi.Mode().Perm()&0o022 == 0 && !OnlyRoot("/bin/sh") {
		t.Error("/bin/sh, in a directory root owns and nobody else may write, does not count as root's alone")
	}
}

// A FIFO under a record's name — planted by another user in a needs directory
// they may write — is refused at once, by the daemon reading its own record
// and by the updater reading everyone's, rather than waited on for a writer:
// a daemon blocked there would never start.
func TestAFIFOWhereARecordShouldBeIsRefusedWithoutWaiting(t *testing.T) {
	h := newHarness(t)
	o := h.options()
	o.ConfigDir = profileDir(t)
	dir, err := NeedsDir(h.target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, recordName(o.ConfigDir)), 0o644); err != nil {
		t.Skip(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, ok, err := ReadRecord(dir, o.ConfigDir); err == nil || ok {
			t.Errorf("ReadRecord of a FIFO = %v, %v; want it refused", ok, err)
		}
		u := New(o)
		if u.Check(context.Background()) {
			t.Error("Check took a release past a FIFO it could not read")
		}
		if u.Status().LastError == "" {
			t.Error("the check did not fail on the FIFO")
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("reading a FIFO where a record should be blocked")
	}
}
