//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"
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

// onScreen is what the terminal shows its owner: everything the line
// discipline echoes back to the master side.
func onScreen(master *os.File) *watched {
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
	return screen
}

// echoes says whether the terminal f echoes what is typed into it.
func echoes(t *testing.T, f *os.File) bool {
	t.Helper()
	return termios(t, f).Lflag&syscall.ECHO != 0
}

func termios(t *testing.T, f *os.File) syscall.Termios {
	t.Helper()
	var tio syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), ptyGetAttr, uintptr(unsafe.Pointer(&tio))); e != 0 {
		t.Fatalf("read the terminal's settings: %v", e)
	}
	return tio
}

// The bug in YAD-171, through a real terminal: an owner pastes a token into
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
	screen := onScreen(master)

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

	// A terminal left raw: Enter would arrive as \r, which ends no line, and
	// Ctrl-C as a byte rather than SIGINT. Reading the token turns both back
	// on, and restore puts the raw settings back as they were.
	raw := termios(t, slave)
	raw.Lflag &^= syscall.ICANON | syscall.ISIG
	raw.Iflag &^= syscall.ICRNL
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), ptySetAttr, uintptr(unsafe.Pointer(&raw))); e != 0 {
		t.Fatalf("make the terminal raw: %v", e)
	}
	restore, _, err = terminalEchoOff(slave)
	if err != nil {
		t.Fatal(err)
	}
	got := termios(t, slave)
	if got.Lflag&syscall.ICANON == 0 || got.Lflag&syscall.ISIG == 0 || got.Iflag&syscall.ICRNL == 0 {
		t.Errorf("a raw terminal stays raw while the token is read: lflag %#x iflag %#x", got.Lflag, got.Iflag)
	}
	restore()
	if got := termios(t, slave); got.Lflag != raw.Lflag || got.Iflag != raw.Iflag {
		t.Errorf("restore did not put the raw settings back: lflag %#x iflag %#x", got.Lflag, got.Iflag)
	}
}

// fakeLoginHub is the service API's login endpoints for one link login on
// runner r1: waiting with its link until a code comes, then taken. It keeps
// the code it was sent, and whether the login was cancelled.
type fakeLoginHub struct {
	mu        sync.Mutex
	code      string
	cancelled bool
}

func (h *fakeLoginHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	base := hubapi.BasePath + "/runners/r1/logins"
	l := hubapi.Login{LoginID: "l1", RunnerID: "r1", Harness: "claude", Account: "work", Method: v1.LoginByLink,
		State: hubapi.LoginState(v1.LoginWaiting), URL: "https://claude.example/authorize"}
	switch r.Method + " " + r.URL.Path {
	case "POST " + base:
	case "POST " + base + "/l1/code":
		var req hubapi.LoginCodeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h.code = req.Code
		l.CodeSent = true
	case "GET " + base + "/l1":
		if h.code != "" {
			l.State = hubapi.LoginState(v1.LoginSucceeded)
		}
	case "POST " + base + "/l1/cancel":
		h.cancelled = true
		l.State = hubapi.LoginState(v1.LoginCancelled)
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(l)
}

// newFakeLoginHub serves a fakeLoginHub on loopback for the test's length,
// with a profile of the test's own, an admin token file for it, and polling
// at test speed.
func newFakeLoginHub(t *testing.T) (h *fakeLoginHub, hubURL, tokFile string) {
	t.Helper()
	accountEnv(t)
	h = &fakeLoginHub{}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tokFile = filepath.Join(t.TempDir(), "admin-token")
	if err := os.WriteFile(tokFile, []byte("yad-admin-not-a-real-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := loginPoll
	loginPoll = 10 * time.Millisecond
	t.Cleanup(func() { loginPoll = old })
	return h, srv.URL, tokFile
}

// A terminal whose echo will not go off: the login is cancelled, nothing is
// read, and the next action is one a person can follow. Piping the code in
// is not — it exists only once this run's link is opened.
func TestHubLoginRefusesATerminalThatEchoes(t *testing.T) {
	h, hubURL, tokFile := newFakeLoginHub(t)
	ft := &fakeTerm{echo: true, r: strings.NewReader("not-a-real-login-code\n")}
	asTerminal(t, ft, errors.New("ioctl refused"))
	code, _, errs := yadIn(t, "hub", "login", "start", "--hub", hubURL, "--token-file", tokFile, "r1", "claude", "work", "--code", "-")
	if code == 0 {
		t.Fatalf("exit 0 at a terminal that echoes: %s", errs)
	}
	if len(ft.echoedAt) != 0 {
		t.Error("stdin was read with echo on")
	}
	h.mu.Lock()
	cancelled, sent := h.cancelled, h.code
	h.mu.Unlock()
	if !cancelled || sent != "" {
		t.Errorf("the login was cancelled = %v, sent %q", cancelled, sent)
	}
	if strings.Contains(errs, "pipe it in") {
		t.Errorf("told to pipe in a code that does not exist yet: %s", errs)
	}
	shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile default hub login start"), dirsEnv(t),
		"yad", "--profile", "default", "hub", "login", "start", "--hub", hubURL, "--token-file", tokFile, "r1", "claude", "work")
}

// YAD-177, through a real terminal: the code `yad hub login start` reads —
// with --code - or without — is pasted with echo off, after a prompt on
// stderr, and sent; it is on no screen and in no output. Ctrl-C at the
// prompt ends the command, cancels the login and leaves echo on.
func TestHubLoginTakesACodePastedAtATerminal(t *testing.T) {
	for _, c := range []struct {
		name  string
		flag  []string
		steps bool // the line saying how to get the code
		ctrlC bool
	}{
		{name: "--code -", flag: []string{"--code", "-"}},
		{name: "no --code", steps: true},
		{name: "Ctrl-C at the prompt", flag: []string{"--code", "-"}, ctrlC: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, hubURL, tokFile := newFakeLoginHub(t)
			master, slave := openPTY(t)
			defer master.Close()
			defer slave.Close()
			if !echoes(t, slave) {
				t.Fatal("a new terminal does not echo; the test cannot tell echo off from the default")
			}
			screen := onScreen(master)
			oldIn := stdin
			stdin = slave
			defer func() { stdin = oldIn }()

			const loginCode = "not-a-real-login-code#state"
			var out bytes.Buffer
			errs := watch("press Enter")
			done := make(chan int, 1)
			args := append([]string{"hub", "login", "start", "--hub", hubURL, "--token-file", tokFile, "r1", "claude", "work"}, c.flag...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { done <- run(ctx, args, &out, errs) }()
			select {
			case <-errs.seen:
			case code := <-done:
				t.Fatalf("exit %d before asking for the code: %s", code, errs)
			case <-time.After(10 * time.Second):
				t.Fatalf("no prompt for the code after 10s; stderr so far %q", errs)
			}
			if echoes(t, slave) {
				t.Error("the terminal echoes while the code is read")
			}
			if c.ctrlC {
				cancel()
				select {
				case code := <-done:
					if code == 0 {
						t.Errorf("exit 0 after Ctrl-C: %s", errs)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("Ctrl-C did not end the read after 10s")
				}
				if !echoes(t, slave) {
					t.Error("Ctrl-C left the terminal's echo off")
				}
				h.mu.Lock()
				defer h.mu.Unlock()
				if !h.cancelled || h.code != "" {
					t.Errorf("after Ctrl-C the login was cancelled = %v, sent %q", h.cancelled, h.code)
				}
				return
			}
			if _, err := master.Write([]byte(loginCode + "\n")); err != nil {
				t.Fatal(err)
			}
			var code int
			select {
			case code = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("a pasted code and Enter did not end the read after 10s")
			}
			if code != 0 {
				t.Fatalf("exit %d: %s\n%s", code, errs, out.String())
			}
			if !echoes(t, slave) {
				t.Error("the terminal's echo was left off")
			}
			h.mu.Lock()
			sent := h.code
			h.mu.Unlock()
			if sent != loginCode {
				t.Errorf("the hub was sent %q, want %q", sent, loginCode)
			}
			if !strings.Contains(errs.String(), "Paste the login code and press Enter") {
				t.Errorf("the prompt does not name a login code: %q", errs)
			}
			if got := strings.Contains(errs.String(), "open the link and sign in"); got != c.steps {
				t.Errorf("said how to get the code = %v, want %v: %q", got, c.steps, errs)
			}
			for name, s := range map[string]string{"stdout": out.String(), "stderr": errs.String(), "the terminal": screen.String()} {
				if strings.Contains(s, loginCode) {
					t.Errorf("the code reached %s: %q", name, s)
				}
			}
		})
	}
}
