package v1

import (
	"fmt"
	"regexp"
	"strings"
)

// A grant is a secret for a run to use: the runner puts it in the harness's
// environment as NAME=value, or writes it to a 0600 file in a directory of
// its own and puts the file's path in NAME. Either way the name becomes an
// environment variable, and for a file grant also the file's name — so a name
// that passes these rules is a plain file name too, never a path.
//
// The owner trusts the hubs it connects (decision 0038). A hub writes the
// brief, and a brief can tell the harness to read any file or send any secret
// anywhere, which the harness then auto-approves (0015) — so filtering
// variable names never protected the machine, and any valid environment
// variable name is accepted. Decision 0024's secret-shaped suffix rule and its
// namespace reservations are gone with that reasoning.
//
// What remains is not a security control: the four below would break the run
// rather than attack it, and they are refused so that a hub's mistake cannot
// unset PATH and make every run on the machine fail in a way nobody can trace
// back. A grant is still delivered to the harness process alone, kept out of
// the prompt, the logs and the events, and deleted when the run ends — that
// protects the secret from being recorded, not the machine from the hub.
//
// Where a variable is the owner's to set rather than the hub's, the code that
// owns it keeps its own guard, which is not a naming rule: IS_SANDBOX is an
// acceptable grant name here and the Claude adapter still strips it from the
// run's environment, because permission mode and sandbox are runner
// configuration (0015, and 0038 keeps it).

// grantNamePattern is an environment variable name as a POSIX shell accepts
// one, and nothing else: no '/', '.', '=' or space, so a file grant's name is
// a plain file name and never a path. ASCII only, which is also what keeps a
// lookalike out — Cyrillic "РАТН" does not match this pattern at all.
var grantNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// deniedGrantNames and deniedGrantPrefixes are matched case-insensitively.
// Environment variables are case-sensitive on linux and darwin, so "path" is
// not PATH to a process — but a file grant's name is a file name, and macOS
// folds case, so "path" beside the loader variables is the same mistake with
// the same untraceable end. A hub with a use for either name has another it
// can use instead.
var deniedGrantNames = map[string]string{
	"PATH": "chooses which executable every command runs, so a run carrying it may not find its harness at all",
	"HOME": "moves every tool's configuration and credentials, the harness's own among them",
}

var deniedGrantPrefixes = []struct{ prefix, why string }{
	{"LD_", "the Linux dynamic loader reads it, and LD_PRELOAD loads code into every process the run starts"},
	{"DYLD_", "the macOS dynamic loader reads it the same way"},
}

// Validate checks a grant's name and delivery. A run carrying a grant that
// fails is refused whole — never run with the grant stripped, since a run
// without the secret it was given would fail in ways nobody can trace back.
func (g Grant) Validate() error {
	if g.As != GrantEnv && g.As != GrantFile {
		return fmt.Errorf("grant %q is delivered as %q, not env or file", g.Name, g.As)
	}
	if !grantNamePattern.MatchString(g.Name) {
		return fmt.Errorf("grant name %q is not an environment variable name ([A-Za-z_][A-Za-z0-9_]*), which is also the file name a file grant is written to", g.Name)
	}
	for name, why := range deniedGrantNames {
		if strings.EqualFold(g.Name, name) {
			return fmt.Errorf("grant name %s is refused: %s %s — send the value under another name", g.Name, name, why)
		}
	}
	for _, d := range deniedGrantPrefixes {
		if len(g.Name) >= len(d.prefix) && strings.EqualFold(g.Name[:len(d.prefix)], d.prefix) {
			return fmt.Errorf("grant name %s is refused: %s* is denied because %s — send the value under another name", g.Name, d.prefix, d.why)
		}
	}
	return nil
}
