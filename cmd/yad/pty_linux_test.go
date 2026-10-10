package main

import (
	"os"
	"strconv"
	"syscall"
	"testing"
	"unsafe"
)

const ptyGetAttr = syscall.TCGETS

// openPTY is posix_openpt, unlockpt and ptsname by hand: the standard
// library has the ioctls but not the calls, and a module for four lines of
// test setup is not worth a line in ARCHITECTURE.md §6.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	fd := m.Fd()
	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		m.Close()
		t.Skipf("no pseudo-terminal here: unlock: %v", e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		m.Close()
		t.Skipf("no pseudo-terminal here: ptsname: %v", e)
	}
	s, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(n), 10), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	return m, s
}
