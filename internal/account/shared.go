package account

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// sharedConfig is what an account's home takes from the harness's own default
// home (decision 0054): the owner's instructions and settings for every run
// on the machine, which a harness reads from whichever home it is pointed at.
// Without it a run on an account never sees the machine's CLAUDE.md or
// AGENTS.md, its skills or its permission rules — the same machine would
// behave differently by which subscription happened to be free.
//
// Only what the owner writes by hand. Never a credential, never the
// transcripts (linked separately, to yad's own directory), and never a file
// the harness keeps its own state in: two homes writing one file through a
// link would be two logins' state in one place.
var sharedConfig = map[string][]string{
	"claude": {"CLAUDE.md", "settings.json", "skills", "commands", "agents"},
	"codex":  {"AGENTS.md", "prompts"},
}

// DefaultHome is where a harness keeps its home when no account is in play:
// its home variable in the runner's environment, or its own directory under
// the user's home. Empty for a harness yad knows no home for.
func DefaultHome(harness string) string {
	v, ok := homeVar[harness]
	if !ok {
		return ""
	}
	if h := os.Getenv(v); h != "" {
		return h
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(user, "."+harness)
}

// shareConfig links each piece of shared configuration the default home has
// into the account's home, and removes a link it made whose target is gone.
// It is run every time a home is prepared, so a skill the owner adds later
// reaches every account at its next run.
//
// Something real already in the account's home is the owner's choice for
// that account and is left alone, as is a link pointing anywhere else.
// Several runs prepare one home at once, so losing a race to create the same
// link is success.
func shareConfig(harness, home string) error {
	from := DefaultHome(harness)
	if from == "" || from == home {
		return nil
	}
	var errs []error
	for _, name := range sharedConfig[harness] {
		src, dst := filepath.Join(from, name), filepath.Join(home, name)
		_, srcErr := os.Stat(src)
		at, linkErr := os.Readlink(dst)
		switch {
		case linkErr == nil && at == src:
			if errors.Is(srcErr, fs.ErrNotExist) {
				if err := os.Remove(dst); err != nil && !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, err)
				}
			}
		case srcErr != nil:
			// Nothing to share, or nothing yad can see; either way no link.
		default:
			if _, err := os.Lstat(dst); err == nil {
				continue // the owner's own, or a link of theirs
			}
			if err := os.Symlink(src, dst); err != nil && !errors.Is(err, fs.ErrExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}
