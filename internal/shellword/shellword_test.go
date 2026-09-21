package shellword_test

import (
	"testing"

	"github.com/skkap/yad/internal/shellword"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// Every value survives a real shell as exactly one argument, whatever it holds.
func TestCommandIsReadBackByARealShell(t *testing.T) {
	values := append([]string{"plain", "https://hub.example:8443/p", "v0.3.1", "/tmp/a/b.c", "a,b+c@d%e"}, shellwordtest.Hostile...)
	for _, v := range values {
		t.Run(v, func(t *testing.T) {
			line := shellword.Command("prog", "sub", v, "--flag", v)
			shellwordtest.Check(t, line, "prog", "sub", v, "--flag", v)
		})
	}
}

// The ordinary command stays readable: a word no shell treats specially is left
// bare, and only one that needs it is quoted.
func TestQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"work", "work"},
		{"v0.3.1-rc.1", "v0.3.1-rc.1"},
		{"https://hub.example/api", "https://hub.example/api"},
		{"", "''"},
		{"with space", "'with space'"},
		{"it's", `'it'\''s'`},
		{"$HOME", "'$HOME'"},
		{"~", "'~'"},
		{"x=y", "'x=y'"},
		{"café", "'café'"},
	} {
		if got := shellword.Quote(tc.in); got != tc.want {
			t.Errorf("Quote(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// The test helper itself: a line that escapes its quoting must be caught, or
// every test built on it proves nothing.
func TestRunCatchesAnEscapedValue(t *testing.T) {
	calls := shellwordtest.Run(t, "prog a;prog b $HOME", "prog")
	if len(calls) != 2 || calls[1][1] != "b" || calls[1][2] == "$HOME" {
		t.Errorf("an unquoted ; and $HOME were not visible in %q", calls)
	}
}

// The prose around a command repeats the raw value, backtick and apostrophe
// included; only the command is read as shell.
func TestCommands(t *testing.T) {
	msg := "label a`b's login — run `yad account add claude 'a`b'\\''s'` or `yad doctor`, it's fine"
	got := shellwordtest.Commands(msg, "yad ")
	if len(got) != 2 || got[0] != `yad account add claude 'a`+"`"+`b'\''s'` || got[1] != "yad doctor" {
		t.Errorf("Commands = %q", got)
	}
}
