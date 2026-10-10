package account

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

// sharedConfig is what an account's home takes from the harness's own default
// home (decision 0054): the owner's instructions and settings for every run
// on the machine, which a harness reads from whichever home it is pointed at.
// Without it a run on an account never sees the machine's CLAUDE.md or
// AGENTS.md, its skills or its permission rules — the same machine would
// behave differently by which subscription happened to be free.
//
// Only what the owner chooses for the machine. Never a credential, never the
// transcripts (linked separately, to yad's own directory), and never a file
// the harness keeps a login's own state in: two homes writing one file
// through a link would be two logins' state in one place. Claude's plugins/
// is state, but the machine's and not a login's — what is installed, and
// where — which every session on the default home already shares, and which
// settings.json, shared here, enables by name (decision 0074).
var sharedConfig = map[string][]string{
	"claude": {"CLAUDE.md", "settings.json", "skills", "commands", "agents", "plugins"},
	"codex":  {"AGENTS.md", "prompts"},
}

// madeEmpty names, per harness, the shared directories the harness makes for
// itself in a home that has none, and the files it puts in them when there
// is nothing to record. An account that ran before such a directory was
// shared has the harness's empty one where the link belongs; holding nothing
// but these, recording nothing, it is replaced by the link (DEV-174).
// Anything else in it is the account's own, as a real skills/ is.
var madeEmpty = map[string]map[string][]string{
	"claude": {"plugins": {"installed_plugins.json", "known_marketplaces.json"}},
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
// that account and is left alone, as is a link pointing anywhere else — but
// for a directory the harness made empty itself (madeEmpty). Several runs
// prepare one home at once, so losing a race to create the same link is
// success.
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
			if fi, err := os.Lstat(dst); err == nil {
				if files, ok := madeEmpty[harness][name]; ok && fi.IsDir() {
					if err := replaceEmpty(dst, src, files); err != nil {
						errs = append(errs, err)
					}
				}
				continue // the owner's own, or a link of theirs
			}
			if err := os.Symlink(src, dst); err != nil && !errors.Is(err, fs.ErrExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// replaceEmpty puts the link to src where the harness made an empty dst of
// its own: the files it writes when there is nothing to record, and empty
// directories, are removed, and link does the rest — rmdir, which refuses a
// directory something was written into meanwhile, and a link renamed into
// place, so a run starting then never finds the path missing. A directory
// holding anything more is left as it is.
func replaceEmpty(dst, src string, files []string) error {
	entries, err := os.ReadDir(dst)
	if err != nil {
		return nil // gone, or not readable as a directory: not one to replace
	}
	var remove, rmdir []string
	for _, e := range entries {
		p := filepath.Join(dst, e.Name())
		switch {
		case e.IsDir():
			if sub, err := os.ReadDir(p); err != nil || len(sub) > 0 {
				return nil
			}
			rmdir = append(rmdir, p)
		case e.Type().IsRegular() && slices.Contains(files, e.Name()) && recordsNothing(p):
			remove = append(remove, p)
		default:
			return nil
		}
	}
	for _, p := range remove {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, p := range rmdir {
		// A directory written into since it was read is the harness's now;
		// the link is left for a later run.
		if err := syscall.Rmdir(p); err != nil && !errors.Is(err, syscall.ENOENT) {
			return nil
		}
	}
	return link(dst, src, func(string, string) error { return nil })
}

// recordsNothing says whether a file the harness writes when there is nothing
// to record holds just that: a JSON object whose every entry but its format
// version is empty. Anything else — a plugin installed, a marketplace added,
// a file that is not JSON — is something recorded, and is kept.
func recordsNothing(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	// The empty file is a handful of bytes; one larger is not empty.
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil || len(b) == 4096 {
		return false
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil || doc == nil {
		return false
	}
	for k, v := range doc {
		if k == "version" {
			continue
		}
		var empty map[string]json.RawMessage
		var list []json.RawMessage
		switch {
		case string(v) == "null":
		case json.Unmarshal(v, &empty) == nil && len(empty) == 0:
		case json.Unmarshal(v, &list) == nil && len(list) == 0:
		default:
			return false
		}
	}
	return true
}
