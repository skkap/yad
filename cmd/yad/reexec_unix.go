//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// reexec replaces this process with the binary at path, with the same
// arguments and environment: the pid stays, so launchd and systemd see no exit
// and restart nothing (decision 0069). Only fds 0 to 2 cross it — every file
// Go opens is close-on-exec, and the daemon has closed its own before this.
// It returns only on failure, and then the process exits with the error: a
// service manager restarts a failed runner, and the binary it starts is the
// new one already in place. A variable, so a test running the daemon in
// its own process sees the call rather than being replaced by it.
var reexec = func(path string) error {
	err := syscall.Exec(path, os.Args, os.Environ())
	return fmt.Errorf("could not re-execute %s for the self-update: %w — the new release is installed there, so starting the runner again starts it", path, err)
}
