//go:build darwin || linux

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// terminalEchoOff makes the changes golang.org/x/term's ReadPassword makes,
// with the standard library's ioctl rather than a module that would need a
// line in ARCHITECTURE.md §6. Echo goes off; canonical mode, ISIG and ICRNL
// go on, even on a terminal a crashed program left raw: Enter must end the
// line as a newline readLine sees, and Ctrl-C must still be SIGINT.
//
// A file is a terminal only if it answers the terminal's own ioctl. A
// character device is not enough: /dev/null is one, and a command run with
// `< /dev/null` must read it as the empty input it is.
func terminalEchoOff(f *os.File) (restore func(), isTerm bool, err error) {
	fd := f.Fd()
	var was syscall.Termios
	if ioctlTermios(fd, termiosGet, &was) != nil {
		return nil, false, nil
	}
	quiet := was
	quiet.Lflag &^= syscall.ECHO
	quiet.Lflag |= syscall.ICANON | syscall.ISIG
	quiet.Iflag |= syscall.ICRNL
	if err := ioctlTermios(fd, termiosSet, &quiet); err != nil {
		return nil, true, err
	}
	return func() { _ = ioctlTermios(fd, termiosSet, &was) }, true, nil
}

func ioctlTermios(fd, req uintptr, t *syscall.Termios) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}
