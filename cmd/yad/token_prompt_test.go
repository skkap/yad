package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTerm is stdin as a terminal: what is pasted into it, and whether echo
// was on at each read.
type fakeTerm struct {
	mu        sync.Mutex
	echo      bool
	echoedAt  []bool
	restored  int
	r         io.Reader // nil blocks every read, as a person who pastes nothing does
	blockRead chan struct{}
}

func (f *fakeTerm) Read(p []byte) (int, error) {
	f.mu.Lock()
	f.echoedAt = append(f.echoedAt, f.echo)
	r := f.r
	f.mu.Unlock()
	if r == nil {
		<-f.blockRead
		return 0, io.EOF
	}
	return r.Read(p)
}

// asTerminal makes stdin t and echoOff t's switch for the test's length.
func asTerminal(tb testing.TB, t *fakeTerm, fail error) {
	tb.Helper()
	oldIn, oldOff := stdin, echoOff
	tb.Cleanup(func() { stdin, echoOff = oldIn, oldOff })
	stdin = t
	echoOff = func(r io.Reader) (func(), bool, error) {
		ft, ok := r.(*fakeTerm)
		if !ok {
			return nil, false, nil
		}
		if fail != nil {
			return nil, true, fail
		}
		ft.mu.Lock()
		was := ft.echo
		ft.echo = false
		ft.mu.Unlock()
		return func() {
			ft.mu.Lock()
			ft.echo = was
			ft.restored++
			ft.mu.Unlock()
		}, true, nil
	}
}

// What readSecret does with each kind of stdin. The token never reaches
// stderr in any of them.
func TestReadSecret(t *testing.T) {
	const tok = "sk-ant-oat01-not-a-real-token"
	for _, c := range []struct {
		name     string
		what     string // "token" when empty
		term     bool
		in       string
		piped    func(io.Reader) (string, error)
		failEcho error
		want     string
		wantErr  string
		prompt   bool
	}{
		// The bug: at a terminal the token is one line, and Enter ends it —
		// what follows is not read, and nothing waits for end of input.
		{name: "terminal, whole-input command", term: true, in: tok + "\nand more the owner typed\n", piped: wholeInput, want: tok, prompt: true},
		{name: "terminal, first-line command", term: true, in: "  " + tok + "  \n", piped: firstLine, want: tok, prompt: true},
		// Ctrl-D at the prompt: an empty token, for the caller to refuse.
		{name: "terminal, end of input", term: true, in: "", piped: wholeInput, want: "", prompt: true},
		{name: "terminal whose echo stays on", term: true, in: tok + "\n", piped: wholeInput, failEcho: errors.New("ioctl refused"), wantErr: "pipe it in instead"},
		// Piped in, each command reads what it always did, and says nothing.
		{name: "pipe, whole-input command", in: tok + "\nsecond line\n", piped: wholeInput, want: tok + "\nsecond line\n"},
		{name: "pipe, first-line command", in: tok + "\nsecond line\n", piped: firstLine, want: tok},
		// `hub login start`'s code (YAD-177): the same at a terminal, one
		// line from a pipe, and refused rather than echoed.
		{name: "terminal, login code", what: "login code", term: true, in: tok + "\nmore\n", piped: oneLine, want: tok, prompt: true},
		{name: "terminal whose echo stays on, login code", what: "login code", term: true, in: tok + "\n", piped: oneLine, failEcho: errors.New("ioctl refused"), wantErr: "so the login code would be shown"},
		{name: "pipe, login code", what: "login code", in: "  " + tok + "  \nmore\n", piped: oneLine, want: tok},
	} {
		t.Run(c.name, func(t *testing.T) {
			ft := &fakeTerm{echo: true, r: strings.NewReader(c.in)}
			if c.term {
				asTerminal(t, ft, c.failEcho)
			} else {
				old := stdin
				t.Cleanup(func() { stdin = old })
				stdin = strings.NewReader(c.in)
			}
			what := c.what
			if what == "" {
				what = "token"
			}
			var errb bytes.Buffer
			got, err := readSecret(context.Background(), &errb, what, c.piped)
			switch {
			case c.wantErr != "":
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want one saying %q", err, c.wantErr)
				}
				if len(ft.echoedAt) != 0 {
					t.Error("stdin was read with echo on")
				}
			case err != nil:
				t.Fatal(err)
			case got != c.want:
				t.Errorf("read %q, want %q", got, c.want)
			}
			if prompted := strings.Contains(errb.String(), "Paste the "+what+" and press Enter"); prompted != c.prompt {
				t.Errorf("prompted = %v, want %v: stderr %q", prompted, c.prompt, errb.String())
			}
			if strings.Contains(errb.String(), tok) {
				t.Errorf("the token reached stderr: %q", errb.String())
			}
			if !c.term {
				return
			}
			for i, on := range ft.echoedAt {
				if on {
					t.Errorf("read %d ran with echo on", i)
				}
			}
			if c.failEcho == nil && (ft.restored != 1 || !ft.echo) {
				t.Errorf("echo restored %d times, on = %v; want once, on", ft.restored, ft.echo)
			}
		})
	}
}

// Ctrl-C reaches a command as ctx's end (main's signal.NotifyContext); a read
// blocked on stdin must give way to it, at a terminal or on a pipe nobody
// closes, and leave echo on.
func TestReadSecretEndsAtCtrlC(t *testing.T) {
	for _, term := range []bool{true, false} {
		ft := &fakeTerm{echo: true, blockRead: make(chan struct{})}
		if term {
			asTerminal(t, ft, nil)
		} else {
			// Not a *fakeTerm, so not a terminal: a pipe whose writer
			// never closes it.
			old := stdin
			stdin = struct{ io.Reader }{ft}
			defer func() { stdin = old }()
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := readSecret(ctx, io.Discard, "token", wholeInput)
			done <- err
		}()
		cancel()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "stopped before the token was read") {
				t.Errorf("terminal %v: err = %v", term, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("terminal %v: Ctrl-C did not end the read", term)
		}
		close(ft.blockRead)
		ft.mu.Lock()
		if term && (ft.restored != 1 || !ft.echo) {
			t.Errorf("after Ctrl-C echo was restored %d times, on = %v", ft.restored, ft.echo)
		}
		ft.mu.Unlock()
	}
}
