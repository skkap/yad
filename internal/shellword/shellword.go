// Package shellword writes the commands yad prints for an owner to paste: a
// next action in an error, a fix in a warning, a follow-up in a status line.
//
// Such a command is an executable artifact, not prose. It is pasted into a
// POSIX shell (sh, bash, zsh), and a value spliced into it bare — a label, a
// hub URL, a release tag, a path — is parsed by that shell: a space splits it,
// a $ or a backtick expands, a ; ends the command and starts another. Every
// command yad suggests is built here, from argv, so no caller formats one by
// hand and no value reaches a shell unquoted.
package shellword

import "strings"

// Quote is s as one POSIX shell word: a shell that reads it passes s through
// unchanged, as a single argument.
//
// A word made only of characters no shell treats specially is left bare, so
// the ordinary command stays readable — `yad account add claude work`, not
// 'yad' 'account' 'add' 'claude' 'work'. Anything else is single-quoted, not
// %q-style double-quoted: inside double quotes a shell still expands $, ` and
// \, so a value with a $ in it would be silently rewritten by the very paste
// the command exists for. Inside single quotes nothing is special, and the one
// character to handle is the single quote itself: close, escape it, reopen.
func Quote(s string) string {
	if s != "" && strings.IndexFunc(s, special) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Command is argv as one line a POSIX shell parses back into the same argv.
// argv[0] is the program. A placeholder the reader is meant to replace, such as
// "<new token>", is quoted like any other word: pasted unedited it arrives as
// one literal argument the program can refuse, rather than as a redirection
// the shell acts on. What the reader types over it is theirs to quote: a value
// typed between the quotes is literal unless it holds a single quote itself.
func Command(argv ...string) string {
	words := make([]string, len(argv))
	for i, a := range argv {
		words[i] = Quote(a)
	}
	return strings.Join(words, " ")
}

// Expand is a word the reader's shell replaces with the value of the variable
// name, whatever that value holds, as exactly one argument: "$NAME".
//
// It is for a value that must not be printed but is known to the reader: a
// binary a YAD_<ID>_PATH override names is under its owner's home, and a
// message that leaves the machine may not carry that path (DEV-67, DEV-108).
// Double quotes and not single, because the expansion is the point; inside
// them the expanded value is not split or globbed. The name is the caller's
// constant, never a value from outside, so one that is not a variable name
// is a bug and panics rather than printing a command that means something
// else.
func Expand(name string) string {
	if !isName(name) {
		panic("shellword.Expand: " + Quote(name) + " is not a variable name")
	}
	return `"$` + name + `"`
}

func isName(s string) bool {
	for i, r := range s {
		switch {
		case r == '_', 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z':
		case '0' <= r && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return s != ""
}

// special reports a rune that may not appear in a bare word. It is an allow
// list rather than a list of the characters shells are known to treat
// specially: a character missed from a deny list is a command that silently
// means something else, while one missed from here only costs a pair of quotes.
//
// Deliberately absent: = (a first word holding one is an assignment, not a
// program), ~ (expands at the start of a word and after = or :), and every
// non-ASCII rune (a shell's idea of which are word characters is its locale's).
func special(r rune) bool {
	switch {
	case 'a' <= r && r <= 'z', 'A' <= r && r <= 'Z', '0' <= r && r <= '9':
		return false
	}
	return !strings.ContainsRune("-_./:@%+,", r)
}
