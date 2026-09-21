package runner

import (
	"syscall"
	"testing"
)

// ufImmutable is UF_IMMUTABLE from <sys/stat.h>, which the syscall package
// does not export. Unlike its Linux counterpart the owner may set it without
// privilege, which is what lets a test reach the case nothing gets past.
const ufImmutable = 0x2

const canFreeze = true

// freeze makes path immutable: nothing may unlink from it, write it or change
// its mode — the owner included — until the flag is cleared. It reports with
// t.Error, not t.Fatal, because a run's fake harness calls it from its own
// goroutine.
func freeze(t *testing.T, path string) {
	if err := syscall.Chflags(path, ufImmutable); err != nil {
		t.Errorf("chflags: %v", err)
		return
	}
	t.Cleanup(func() { syscall.Chflags(path, 0) })
}
