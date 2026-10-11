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
