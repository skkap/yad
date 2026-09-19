//go:build unix

package workdir

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive flock without waiting. flock belongs to the open
// file, so two opens conflict even within one process, and the kernel drops
// the lock when the process dies, however it dies.
func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}
