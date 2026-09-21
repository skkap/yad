// Package shellwordtest proves a printed command by running it, not by reading
// it.
//
// A test that asserts Go's idea of quoting agrees with itself passes while the
// command is broken — which is how quoting POSIX does not read once shipped.
// Here a real /bin/sh parses the line, and every program it names is a stub
// that records the argv it was handed, so the assertion is about what an owner
// who pastes the line would actually run. A value that escapes its quoting
// shows up as a split, rewritten, extra or missing argument.
package shellwordtest

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Commands are the commands a message prints between backticks that start with
// prefix, the way every yad message sets a command apart from its prose.
//
// The prefix is what makes this readable at all: the prose around a command
// repeats the raw values the command quotes — a path with a backtick or an
// apostrophe in it — so where a command starts can only be found by what it
// starts with. Where it ends is read the way sh reads it: a backtick inside a
// single-quoted word, or escaped by a backslash, is part of the word, and
// shellword.Quote quotes every word holding one, so the first bare backtick
// is the end. Variable assignments ahead of the program — NAME=value, as
// config.Paths.Command writes the profile's directories — are part of the
// command and are skipped before the prefix is matched.
func Commands(msg, prefix string) []string {
	var out []string
	for i := 0; i < len(msg); i++ {
		if msg[i] != '`' {
			continue
		}
		start := i + 1
		at := start
		for assignment.MatchString(msg[at:]) {
			n := wordEnd(msg[at:], ' ')
			if n < 0 {
				break
			}
			at += n + 1
		}
		if !strings.HasPrefix(msg[at:], prefix) {
			continue
		}
		end := wordEnd(msg[start:], '`')
		if end < 0 {
			return out
		}
		out = append(out, msg[start:start+end])
		i = start + end
	}
	return out
}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// wordEnd is the offset of the first stop outside single quotes and not
// escaped by a backslash, read as sh reads it; -1 when there is none.
func wordEnd(s string, stop rune) int {
	quoted, escaped := false, false
	for i, r := range s {
		switch {
		case escaped:
			escaped = false
		case r == '\\' && !quoted:
			escaped = true
		case r == '\'':
			quoted = !quoted
		case r == stop && !quoted:
			return i
		}
	}
	return -1
}

// Run has /bin/sh run line with a stub standing in for each of programs, and
// returns every invocation the stubs saw, each as its argv with the program's
// name first. HOME is a fresh directory, so a $HOME that escapes its quoting
// expands to something no test value contains.
//
// The stubs are shell functions defined ahead of the line, not scripts on
// PATH: a simple command finds a function before it searches PATH, so the
// shell parses the line exactly as it would for the real program, and macOS
// spends seconds assessing every freshly written executable before its first
// run — a test per hostile value would take minutes.
//
// sh failing is not a test failure by itself: a command that exits non-zero
// may still have been parsed right, and one parsed wrong is caught by the argv.
func Run(t testing.TB, line string, programs ...string) [][]string {
	t.Helper()
	var argvs [][]string
	for _, c := range run(t, line, nil, programs) {
		argvs = append(argvs, c.argv)
	}
	return argvs
}

type call struct {
	argv []string
	env  map[string]string
}

// unset stands for a variable the program did not see; no test value is it.
const unset = "\x01unset"

func run(t testing.TB, line string, vars, programs []string) []call {
	t.Helper()
	dir := t.TempDir()
	var script strings.Builder
	for _, p := range programs {
		// Length-prefixed and NUL-separated: an argument may hold a newline
		// or be empty, and neither may blur where one argument or invocation
		// ends. The variables follow, a fixed number of them.
		script.WriteString(p + `() { printf '%s\0' "$#" ` + p + ` "$@"`)
		for _, v := range vars {
			script.WriteString(` "${` + v + `-` + unset + `}"`)
		}
		script.WriteString(` >> "$STUB_LOG"; }` + "\n")
	}
	script.WriteString(line)
	log := filepath.Join(dir, "argv")
	cmd := exec.Command("/bin/sh", "-c", script.String())
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + filepath.Join(dir, "home"), "STUB_LOG=" + log}
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("sh could not run %s: %v", line, err)
		}
	}
	b, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
	var calls []call
	for len(fields) > 0 {
		n, err := strconv.Atoi(fields[0])
		if err != nil || len(fields) < n+2+len(vars) {
			t.Fatalf("the stub log is not one this package wrote: %q", b)
		}
		c := call{argv: fields[1 : n+2], env: map[string]string{}}
		for i, v := range vars {
			if val := fields[n+2+i]; val != unset {
				c.env[v] = val
			}
		}
		calls = append(calls, c)
		fields = fields[n+2+len(vars):]
	}
	return calls
}

// Check fails t unless line, run by sh, invokes exactly one program with
// exactly want as its argv. want[0] is the program's name and the one stub.
func Check(t testing.TB, line string, want ...string) {
	t.Helper()
	CheckEnv(t, line, nil, want...)
}

// CheckEnv is Check, and also fails t unless the program saw exactly env for
// the variables env names — one mapped to "" must not be set at all. The shell
// starts with none of them, so what the program sees is what the line set.
func CheckEnv(t testing.TB, line string, env map[string]string, want ...string) {
	t.Helper()
	var vars []string
	for v := range env {
		vars = append(vars, v)
	}
	slices.Sort(vars)
	calls := run(t, line, vars, want[:1])
	if len(calls) != 1 || !slices.Equal(calls[0].argv, want) {
		var argvs [][]string
		for _, c := range calls {
			argvs = append(argvs, c.argv)
		}
		t.Errorf("sh ran\n  %s\nas %q, want one call of %q", line, argvs, want)
		return
	}
	for _, v := range vars {
		got, set := calls[0].env[v]
		switch {
		case env[v] == "" && set:
			t.Errorf("sh ran\n  %s\nwith %s=%q, want it unset", line, v, got)
		case env[v] != "" && got != env[v]:
			t.Errorf("sh ran\n  %s\nwith %s=%q, want %q", line, v, got, env[v])
		}
	}
}

// Hostile are values a label, a tag, a URL or a path could carry that change
// what a shell does with a command when spliced into it bare: a word split, a
// variable, a command substitution either way, a quote, a command separator, a
// glob, a comment, a tilde and a newline. Harmless if one does escape: what
// they would run is id or echo.
var Hostile = []string{
	"with space",
	"$HOME",
	"`id`",
	"$(id)",
	"it's",
	"a;echo injected",
	"a&&echo injected",
	"a|b",
	"*",
	"#comment",
	"~",
	"x=y",
	`back\slash`,
	`dq"uote`,
	"line\nbreak",
	"",
}
