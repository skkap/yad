package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// echoOff turns off echo on r when r is a terminal, and returns what turns it
// back on; isTerm is false, and nothing is touched, when r is anything else.
// A variable so a test can stand at a terminal without opening one.
var echoOff = func(r io.Reader) (restore func(), isTerm bool, err error) {
	f, ok := r.(*os.File)
	if !ok {
		return nil, false, nil
	}
	return terminalEchoOff(f)
}

// readSecret is a secret read from stdin: every `--token -`, and the login
// code `hub login start` reads. Piped in, it is read as piped says, as it always was.
// Typed at a terminal it is one line, asked for, kept off the screen, and
// ended by Enter — a person never sends the end of input a pipe does, so a
// read to the end looked hung (DEV-171), and a code read with echo on was on
// the screen (DEV-177). Ctrl-C ends it either way: main turns SIGINT into
// ctx's end, which a blocked read would never see.
//
// what names the secret in the prompt; it is never the secret itself.
func readSecret(ctx context.Context, stderr io.Writer, what string, piped func(io.Reader) (string, error)) (string, error) {
	// Read once: the read can outlive this call, which returns at Ctrl-C.
	in := stdin
	restore, isTerm, err := echoOff(in)
	if err != nil {
		// Read with echo on, the secret would be on the screen and in any
		// recording of it; refuse rather than show it. A login code too: it
		// is short-lived and bound to the login the runner holds open, so
		// worth less seen than a token, but no real terminal fails this,
		// and one rule for every secret is easier to trust than exceptions.
		return "", &echoStaysOn{what: what, err: err}
	}
	var s string
	if isTerm {
		defer restore()
		fmt.Fprintf(stderr, "Paste the %s and press Enter: ", what)
		s, err = readLine(ctx, in)
		// Enter was not echoed either; without this the next line printed
		// would start after the prompt.
		fmt.Fprintln(stderr)
	} else {
		s, err = awaitRead(ctx, func() (string, error) { return piped(in) })
	}
	if err != nil && ctx.Err() != nil {
		return "", fmt.Errorf("stopped before the %s was read", what)
	}
	return s, err
}

// echoStaysOn is readSecret's refusal at a terminal whose echo would not go
// off. Its next action, piping the secret in, fits a token, which exists
// before the command runs; a caller whose secret does not says what to do
// instead.
type echoStaysOn struct {
	what string
	err  error
}

func (e *echoStaysOn) Error() string { return e.shown() + " — pipe it in instead" }

func (e *echoStaysOn) Unwrap() error { return e.err }

// shown is the refusal without its next action.
func (e *echoStaysOn) shown() string {
	return fmt.Sprintf("stdin is a terminal whose echo could not be turned off (%v), so the %s would be shown as it is pasted", e.err, e.what)
}

// readLine is one line of r, trimmed, or ctx's end: a person may walk away
// from the prompt, and Ctrl-C must still end the command.
func readLine(ctx context.Context, r io.Reader) (string, error) {
	return awaitRead(ctx, func() (string, error) { return oneLine(r) })
}

// oneLine is the first line of r, trimmed; what `hub login start` reads of a
// piped code.
func oneLine(r io.Reader) (string, error) {
	s, err := bufio.NewReader(io.LimitReader(r, 64<<10)).ReadString('\n')
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return strings.TrimSpace(s), err
}

// awaitRead is read, or ctx's end if that comes first: a pipe whose writer
// never closes it must not outlast Ctrl-C.
func awaitRead(ctx context.Context, read func() (string, error)) (string, error) {
	type result struct {
		s   string
		err error
	}
	got := make(chan result, 1)
	go func() {
		s, err := read()
		got <- result{s, err}
	}()
	select {
	case r := <-got:
		return r.s, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// wholeInput is what `yad account add` and `yad hub login token` read from a
// pipe: all of it, so a token followed by more is refused by
// account.CheckToken rather than cut down to its first line.
func wholeInput(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil {
		return "", fmt.Errorf("read the token from stdin: %w", err)
	}
	return string(b), nil
}

// firstLine is what `yad connect` and `yad conformance` read from a pipe:
// the line `yad hub token create` prints.
func firstLine(r io.Reader) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read the registration token from stdin: %w", err)
	}
	return strings.TrimSpace(line), nil
}
