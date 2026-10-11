//go:build unix

package selfupdate

import (
	"io/fs"
	"os"
	"syscall"
)

// chownGroup gives dir the group of like, when this user belongs to it; a
// user who does not is refused, and the directory keeps its group.
func chownGroup(dir string, like fs.FileInfo) {
	if st, ok := like.Sys().(*syscall.Stat_t); ok {
		os.Chown(dir, -1, int(st.Gid))
	}
}

// openNonblock keeps an open of a FIFO planted where a record should be from
// waiting for a writer that never comes; readRecord refuses anything but a
// regular file once it is open.
const openNonblock = syscall.O_NONBLOCK
