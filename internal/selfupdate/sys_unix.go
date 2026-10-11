//go:build unix

package selfupdate

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// chownGroup gives dir the group of like, when this user belongs to it; a
// user who does not is refused, and the directory keeps its group.
func chownGroup(dir string, like fs.FileInfo) {
	if st, ok := like.Sys().(*syscall.Stat_t); ok {
		os.Chown(dir, -1, int(st.Gid))
	}
}

// wOK is access(2)'s W_OK, the same 2 on every Unix: syscall names no
// constant for it on darwin, and golang.org/x/sys is not a dependency of its
// own (ARCHITECTURE.md §6).
const wOK = 0x2

// CanReplace is whether this process could put a release in place of binary
// the way upgrade.Apply does: a temporary directory made beside the real
// file and a rename(2) over it. Both need write access to the directory, not
// to the file; in a sticky directory the rename also needs the file to be
// this user's. access(2) asks with the real uid, which for a daemon is the
// one it runs as.
func CanReplace(binary string) bool {
	real, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return false
	}
	dir := filepath.Dir(real)
	if syscall.Access(dir, wOK) != nil {
		return false
	}
	d, err := os.Stat(dir)
	if err != nil {
		return false
	}
	if d.Mode()&os.ModeSticky == 0 {
		return true
	}
	f, err := os.Stat(real)
	if err != nil {
		return false
	}
	st, ok := f.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// OnlyRoot is whether no user but root may write the directory beside the
// real binary: it is root's, and neither its group nor others may write it —
// /usr/local/bin as the work-machine kit leaves it with self-update off.
func OnlyRoot(binary string) bool {
	real, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return false
	}
	d, err := os.Stat(filepath.Dir(real))
	if err != nil {
		return false
	}
	st, ok := d.Sys().(*syscall.Stat_t)
	return ok && st.Uid == 0 && d.Mode().Perm()&0o022 == 0
}

// openNonblock keeps an open of a FIFO planted where a record should be from
// waiting for a writer that never comes; readRecord refuses anything but a
// regular file once it is open.
const openNonblock = syscall.O_NONBLOCK
