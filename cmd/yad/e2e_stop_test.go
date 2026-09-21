//go:build unix

package main

import (
	"bufio"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/skkap/yad/internal/control"
)

// yad daemon stop against a runner that is listening but slow to answer
// (DEV-65). The runner holds a run and a drain wait of an hour, so only a
// second stop could cancel it: whatever the CLI concludes from a late answer,
// the runner hears one stop, drains, and the run finishes. Before the fix the
// daemon acted on the first line of the ask, the CLI took the late answer for
// no answer and sent SIGTERM, and the runner counted that as its second stop
// and cancelled the run (decision 0029).
func TestE2ESlowDaemonStop(t *testing.T) { eachHarness(t, testE2ESlowDaemonStop) }

func testE2ESlowDaemonStop(t *testing.T, h *e2eHarness) {
	for _, tc := range []struct {
		name string
		// onTime is how many of the daemon's answers reach the CLI at once;
		// every later one is held past the CLI's wait.
		onTime int
		// sigterm is whether the CLI falls back to SIGTERM: only for an ask
		// the daemon never acknowledged, which it therefore never acted on,
		// so the SIGTERM is the runner's first stop.
		sigterm bool
		said    string
	}{
		{"no answer in time", 0, true, "sent it SIGTERM"},
		{"acknowledged, last answer late", 1, false, "took the request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t, h)
			m.setDrainWait(time.Hour)
			m.gated()
			m.submit("e2e-held")
			c := m.childDaemon()
			m.waitAtGate()

			control.AskTimeoutForTests = 300 * time.Millisecond
			t.Cleanup(func() { control.AskTimeoutForTests = 0 })
			slowSocket(t, filepath.Join(m.p.data, "yad.sock"), tc.onTime, 2*time.Second)

			code, out, errs := m.p.yad("", "daemon", "stop", "--timeout", "500ms")
			if code != 1 || !strings.Contains(errs, "still stopping") || !strings.Contains(out, tc.said) {
				t.Errorf("stop exit %d, want 1 and still stopping: %s%s — want %q", code, out, errs, tc.said)
			}
			if got := strings.Contains(out, "SIGTERM"); got != tc.sigterm {
				t.Errorf("sent SIGTERM %v, want %v: %s", got, tc.sigterm, out)
			}
			// From a signal or from the socket, as the case sends it.
			c.said(t, "draining:")
			m.open()
			c.exited(t)
			if strings.Contains(c.logged(), "cancelling every run held") {
				t.Errorf("the runner took a second stop and cancelled its runs:\n%s", c.logged())
			}
			if code, out, errs := m.watch("e2e-held"); code != 0 || !strings.Contains(out, "── succeeded") {
				t.Fatalf("watch exit %d: %s\n%s", code, errs, out)
			}
		})
	}
}

// slowSocket moves the daemon's control socket aside and answers in its place,
// relaying each connection both ways. The daemon's first onTime lines go
// through at once and every later one only after lag, as a daemon busy with
// something else answers; the CLI's lines are never held, and a CLI that hangs
// up hangs up on the daemon too.
func slowSocket(t *testing.T, sock string, onTime int, lag time.Duration) {
	t.Helper()
	real := sock + "r"
	if err := os.Rename(sock, real); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			cli, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				d, err := net.Dial("unix", real)
				if err != nil {
					cli.Close()
					return
				}
				var once sync.Once
				hangUp := func() { once.Do(func() { cli.Close(); d.Close() }) }
				go func() { io.Copy(d, cli); hangUp() }()
				r := bufio.NewReader(d)
				for i := 0; ; i++ {
					line, err := r.ReadBytes('\n')
					if err != nil {
						hangUp()
						return
					}
					if i >= onTime {
						time.Sleep(lag)
					}
					if _, err := cli.Write(line); err != nil {
						hangUp()
						return
					}
				}
			}()
		}
	}()
}
