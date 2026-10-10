package main

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

const ptyGetAttr = syscall.TIOCGETA

// openPTY is posix_openpt, grantpt, unlockpt and ptsname by hand: the
// standard library has the ioctls but not the calls, and a module for four
// lines of test setup is not worth a line in ARCHITECTURE.md §6.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	fd := m.Fd()
	var name [128]byte
	for _, c := range []struct {
		req uintptr
		arg uintptr
	}{
		{syscall.TIOCPTYGRANT, 0},
		{syscall.TIOCPTYUNLK, 0},
		{syscall.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))},
	} {
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, c.req, c.arg); e != 0 {
			m.Close()
			t.Skipf("no pseudo-terminal here: ioctl %#x: %v", c.req, e)
		}
	}
	path := string(name[:bytes.IndexByte(name[:], 0)])
	s, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		t.Skipf("no pseudo-terminal here: %v", err)
	}
	return m, s
}
