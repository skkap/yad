//go:build unix

package runner

import (
	"errors"
	"io/fs"
	"path/filepath"
	"syscall"
)

// diskFree is the space an unprivileged process may still write on the file
// system holding path — f_bavail, not f_bfree: the blocks reserved for root
// are not the runner's to fill. A path not made yet is measured at the
// nearest directory above it that exists, which is where it would be made.
func diskFree(path string) (int64, error) {
	for {
		var st syscall.Statfs_t
		err := syscall.Statfs(path, &st)
		if err == nil {
			return int64(st.Bavail) * int64(st.Bsize), nil
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, fs.ErrNotExist) || parent == path {
			return 0, err
		}
		path = parent
	}
}
