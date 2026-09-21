package config

import (
	"os"
	"path/filepath"

	"github.com/skkap/yad/internal/shellword"
)

// YadCommand is the yad command an owner pastes to act on profile: args after
// the global --profile, every word quoted for a POSIX shell.
//
// The profile is carried whenever it is not the default, because the message
// offering the command was produced under it and the reader's shell may not
// be: `yad account add claude work` pasted bare adds the account to the
// default runner, and says nothing about having picked the wrong one. The
// global flag rather than a per-command one, because every command reads it.
//
// It names the profile and nothing else, which makes it the one for a command
// that leaves the machine — in a run's error to a hub, or a hub's answer to a
// runner. A command read on the machine that printed it is Paths.Command,
// which also carries the directories the profile resolved to; those are paths
// under the owner's home, and none of them may travel (DEV-67).
func YadCommand(profile string, args ...string) string {
	return shellword.Command(yadArgv(profile, args)...)
}

func yadArgv(profile string, args []string) []string {
	argv := []string{"yad"}
	if profile != "" && profile != DefaultProfile {
		argv = append(argv, "--profile", profile)
	}
	return append(argv, args...)
}

// dirEnv are the variables that decided a profile's directories (Resolve), in
// the order they are tried: the YAD_ override first, then the XDG base it
// falls back to. service.NewSpec carries the same four into a unit, for the
// same reason.
var dirEnv = [2][2]string{
	{"YAD_CONFIG_DIR", "XDG_CONFIG_HOME"},
	{"YAD_DATA_DIR", "XDG_DATA_HOME"},
}

// resolvedEnv is, for each directory, the one variable that decided it and
// its value as an absolute path; empty where neither was set.
func resolvedEnv() [2][2]string {
	var out [2][2]string
	for i, pair := range dirEnv {
		for _, name := range pair {
			v := os.Getenv(name)
			if v == "" {
				continue
			}
			// Absolute, because the command is pasted later and maybe from
			// another directory, where a relative value names another,
			// empty profile.
			if abs, err := filepath.Abs(v); err == nil {
				v = abs
			}
			out[i] = [2]string{name, v}
			break
		}
	}
	return out
}

// Command is the yad command to paste on this machine to act on this
// profile: YadCommand, preceded by the directory variables Resolve was given.
//
// A profile is its directories as much as its name. Under YAD_DATA_DIR on
// another volume, or a service unit carrying it (0028), `yad --profile work
// daemon logs` pasted into a shell without the variable reads a different,
// empty profile — and answers as if that were the one asked about. A variable
// already exported in the reader's shell is repeated harmlessly.
func (p Paths) Command(args ...string) string {
	cmd := YadCommand(p.Profile, args...)
	prefix := ""
	for _, kv := range p.env {
		if kv[0] != "" {
			prefix += kv[0] + "=" + shellword.Quote(kv[1]) + " "
		}
	}
	return prefix + cmd
}

// RemoteCommand is Command for a reader somewhere else — a run's error, read
// at a hub by whoever then walks to the machine. The directories are paths
// under the owner's home and may not travel (DEV-67), but a command without
// them is pasted into a shell that resolves the default and reads another,
// empty profile. So each variable Resolve was given is kept, its value a
// placeholder naming it: the reader sees that it is needed and what to put.
func (p Paths) RemoteCommand(args ...string) string {
	cmd := YadCommand(p.Profile, args...)
	prefix := ""
	for _, kv := range p.env {
		if kv[0] != "" {
			prefix += kv[0] + "=" + shellword.Quote("<the runner's "+kv[0]+">") + " "
		}
	}
	return prefix + cmd
}

// DataRelocated says a variable, not the default under $HOME, decided where
// the profile's data lives. A command that cannot carry the variable — one
// that leaves the machine — has to say so some other way, or it is pasted
// into a shell that resolves the default and acts on another profile's data.
func (p Paths) DataRelocated() bool { return p.env[1][0] != "" }
