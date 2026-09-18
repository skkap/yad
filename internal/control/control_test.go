//go:build unix

package control

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skkap/yad/internal/config"
)

// testPaths is a profile in a short directory: t.TempDir() on macOS is long
// enough, with a long test name, to pass the socket path limit on its own.
func testPaths(t *testing.T) config.Paths {
	t.Helper()
	dir, err := os.MkdirTemp("", "yadc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return config.Paths{Profile: "default", Config: dir, Data: dir}
}

// serve claims the profile and answers until the test ends.
func serve(t *testing.T, p config.Paths, h Handler) *Daemon {
	t.Helper()
	d, err := Claim(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Serve(ctx, h); close(done) }()
	t.Cleanup(func() {
		cancel()
		<-done
		d.Close()
	})
	return d
}

func TestStatusAndSingleInstance(t *testing.T) {
	p := testPaths(t)
	serve(t, p, Handler{Status: func(context.Context) Status {
		return Status{Profile: "default", Capacity: Capacity{Total: 2, Free: 1}}
	}, Stop: func() {}})

	fi, err := os.Stat(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket mode %v, want a 0600 socket", fi.Mode())
	}

	res, err := Ask(context.Background(), p, "status")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == nil || res.Status.PID != os.Getpid() || res.Status.Capacity.Free != 1 || res.Status.Stopping {
		t.Errorf("status = %+v", res.Status)
	}

	_, err = Claim(p)
	var running *RunningError
	if !errors.As(err, &running) || running.PID != os.Getpid() {
		t.Fatalf("a second daemon: %v, want a RunningError naming pid %d", err, os.Getpid())
	}
	if pid, ok, err := Holder(p); !ok || pid != os.Getpid() || err != nil {
		t.Errorf("Holder = %d, %v, %v", pid, ok, err)
	}
}

func TestStopIsAskedOnce(t *testing.T) {
	p := testPaths(t)
	var stops atomic.Int32
	serve(t, p, Handler{Status: func(context.Context) Status { return Status{} }, Stop: func() { stops.Add(1) }})
	for range 2 {
		if res, err := Ask(context.Background(), p, "stop"); err != nil || res.PID != os.Getpid() {
			t.Fatalf("stop: %+v, %v", res, err)
		}
	}
	if n := stops.Load(); n != 1 {
		t.Errorf("Stop called %d times, want once", n)
	}
	res, err := Ask(context.Background(), p, "status")
	if err != nil || !res.Status.Stopping {
		t.Errorf("status after stop: %+v, %v", res.Status, err)
	}
	if _, err := Ask(context.Background(), p, "reboot"); err == nil || !strings.Contains(err.Error(), "unknown request") {
		t.Errorf("unknown op: %v", err)
	}
}

// A crash leaves the socket file behind. It must read as no daemon, and must
// not stop the next one starting.
func TestStaleSocketIsNotADaemon(t *testing.T) {
	p := testPaths(t)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: p.Socket(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	if err := os.WriteFile(p.Lock(), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.Socket()); err != nil {
		t.Fatalf("the stale socket file is not there: %v", err)
	}

	if _, err := Ask(context.Background(), p, "status"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Ask on a stale socket: %v, want ErrNotRunning", err)
	}
	if _, ok, _ := Holder(p); ok {
		t.Error("a lock file with a dead pid reads as a running daemon")
	}
	d, err := Claim(p)
	if err != nil {
		t.Fatalf("a stale socket blocked the start: %v", err)
	}
	d.Close()
	if _, err := os.Stat(p.Socket()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket left after Close: %v", err)
	}
	if _, ok, _ := Holder(p); ok {
		t.Error("the lock is still held after Close")
	}
}

// A daemon that holds the lock and does not answer is named by its pid, so
// the CLI can signal it.
func TestUnresponsiveDaemon(t *testing.T) {
	p := testPaths(t)
	d, err := Claim(p) // listening, never serving
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err = Ask(ctx, p, "status")
	var un *UnresponsiveError
	if !errors.As(err, &un) || un.PID != os.Getpid() {
		t.Errorf("Ask on a wedged daemon: %v", err)
	}
}

func TestNoDaemonAtAll(t *testing.T) {
	p := testPaths(t)
	if _, err := Ask(context.Background(), p, "status"); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Ask: %v", err)
	}
	if _, ok, err := Holder(p); ok || err != nil {
		t.Errorf("Holder: %v, %v", ok, err)
	}
}

func TestRefusedPlaces(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, p *config.Paths)
		want  string
	}{
		{"path too long", func(t *testing.T, p *config.Paths) {
			p.Data = filepath.Join(p.Data, strings.Repeat("d", 120))
			if err := os.MkdirAll(p.Data, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "YAD_DATA_DIR"},
		{"directory open to others", func(t *testing.T, p *config.Paths) {
			if err := os.Chmod(p.Data, 0o755); err != nil {
				t.Fatal(err)
			}
		}, "chmod 700"},
		{"not a directory", func(t *testing.T, p *config.Paths) {
			p.Data = filepath.Join(p.Data, "file")
			if err := os.WriteFile(p.Data, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "not a directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPaths(t)
			tc.setup(t, &p)
			d, err := Claim(p)
			if err == nil {
				d.Close()
				t.Fatal("claimed")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

func TestCheckPath(t *testing.T) {
	if err := CheckPath("/tmp/" + strings.Repeat("a", maxSocketPath()-5)); err != nil {
		t.Errorf("a path at the limit: %v", err)
	}
	err := CheckPath("/tmp/" + strings.Repeat("a", maxSocketPath()-4))
	if err == nil || !strings.Contains(err.Error(), "--profile") {
		t.Errorf("a path one past the limit: %v", err)
	}
}
