package config

import "github.com/skkap/yad/internal/shellword"

// YadCommand is the yad command an owner pastes to act on profile: args after
// the global --profile, every word quoted for a POSIX shell.
//
// The profile is carried whenever it is not the default, because the message
// offering the command was produced under it and the reader's shell may not
// be: `yad account add claude work` pasted bare adds the account to the
// default runner, and says nothing about having picked the wrong one. The
// global flag rather than a per-command one, because every command reads it.
func YadCommand(profile string, args ...string) string {
	argv := []string{"yad"}
	if profile != "" && profile != DefaultProfile {
		argv = append(argv, "--profile", profile)
	}
	return shellword.Command(append(argv, args...)...)
}
