//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
)

// watched is an output a test can wait on: what a command prints, and a
// signal once a given piece of it has been printed.
type watched struct {
	mu   sync.Mutex
	b    bytes.Buffer
	want string
	seen chan struct{}
}

func watch(want string) *watched { return &watched{want: want, seen: make(chan struct{})} }

func (w *watched) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	had := strings.Contains(w.b.String(), w.want)
	w.b.Write(p)
	if !had && strings.Contains(w.b.String(), w.want) {
		close(w.seen)
	}
	return len(p), nil
}

func (w *watched) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// echoes says whether the terminal f echoes what is typed into it.
func echoes(t *testing.T, f *os.File) bool {
	t.Helper()
	var tio syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ptyGetAttr, uintptr(unsafe.Pointer(&tio))); e != 0 {
		t.Fatalf("read the terminal's settings: %v", e)
	}
	return tio.Lflag&syscall.ECHO != 0
}

// The bug in DEV-171, through a real terminal: an owner pastes a token into
// `yad account add claude tl --token -` and presses Enter. The command must
// ask for it, keep it off the screen, and go on at the end of the line —
// not wait for an end of input a person at a keyboard never sends.
func TestAccountAddTakesATokenPastedAtATerminal(t *testing.T) {
	p := accountHarness(t, claudeE2E)
	interactive = func() bool { return false }
	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()
	if !echoes(t, slave) {
		t.Fatal("a new terminal does not echo; the test cannot tell echo off from the default")
	}
	// What the terminal shows the owner: everything the line discipline
	// echoes back to the master side.
	screen := watch("\x00never")
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := master.Read(buf)
			screen.Write(buf[:n])
			if err != nil {
				return
			}
		}
	}()

	const tok = "sk-ant-oat01-not-a-real-token"
	old := stdin
	stdin = slave
	defer func() { stdin = old }()
	var out bytes.Buffer
	errs := watch("press Enter")
	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), []string{"account", "add", "claude", "tl", "--token", "-"}, &out, errs)
	}()

	select {
	case <-errs.seen:
	case code := <-done:
		t.Fatalf("exit %d before asking for the token: %s", code, errs)
	case <-time.After(10 * time.Second):
		t.Fatalf("no prompt for the token after 10s — the command is waiting silently; stderr so far %q", errs)
	}
	if echoes(t, slave) {
		t.Error("the terminal echoes while the token is read")
	}
	if _, err := master.Write([]byte(tok + "\n")); err != nil {
		t.Fatal(err)
	}
	var code int
	select {
	case code = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a pasted token and Enter did not end the read after 10s")
	}
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, errs, out.String())
	}
	if !echoes(t, slave) {
		t.Error("the terminal's echo was left off")
	}
	for name, s := range map[string]string{"stdout": out.String(), "stderr": errs.String(), "the terminal": screen.String()} {
		if strings.Contains(s, tok) {
			t.Errorf("the token reached %s: %q", name, s)
		}
	}
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Harness["claude"].Accounts; !slices.Equal(got, []string{"tl"}) {
		t.Errorf("accounts = %v, want [tl]", got)
	}
	home := account.HomeDir(p.Data, "claude", "tl")
	if got := account.Env("claude", home); !slices.Contains(got, "CLAUDE_CODE_OAUTH_TOKEN="+tok) {
		t.Errorf("the account's runs would get %q", got)
	}
}

// Which files are terminals: only one that answers the terminal's ioctl. A
// character device is not enough — `< /dev/null` is a redirect, read as the
// empty input it is, with no prompt.
func TestTerminalEchoOff(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	for name, f := range map[string]*os.File{"/dev/null": devNull, "a pipe": pr} {
		if restore, isTerm, err := terminalEchoOff(f); isTerm || err != nil || restore != nil {
			t.Errorf("%s: terminal = %v, err = %v", name, isTerm, err)
		}
	}

	master, slave := openPTY(t)
	defer master.Close()
	defer slave.Close()
	restore, isTerm, err := terminalEchoOff(slave)
	if !isTerm || err != nil {
		t.Fatalf("a pseudo-terminal: terminal = %v, err = %v", isTerm, err)
	}
	if echoes(t, slave) {
		t.Error("echo is still on")
	}
	restore()
	if !echoes(t, slave) {
		t.Error("restore left echo off")
	}
}
