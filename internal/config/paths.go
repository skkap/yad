// Package config is a runner's profile on disk: where it lives, what the owner
// configured, the credentials it holds and the identity it registers with.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// DefaultProfile is the profile used when none is named. Its directories are the
// bare ~/.config/yad and ~/.local/share/yad, so a single-runner machine never
// has to know profiles exist.
const DefaultProfile = "default"

var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Paths are one profile's directories. A profile is one runner: two profiles on
// one machine share nothing, which is how personal and work stay apart.
type Paths struct {
	Profile string
	Config  string // config.toml, runner-id, credentials/
	Data    string // state.db, workdirs/, repos/, accounts/, transcripts/, logs/, yad.sock
}

// Resolve returns the directories for a profile. $YAD_CONFIG_DIR and
// $YAD_DATA_DIR override both, for tests and for owners who keep state on a
// separate volume. The same resolution applies on macOS and Linux, because the
// scripts that read these paths do not care which one they are on.
func Resolve(profile string) (Paths, error) {
	if profile == "" {
		profile = DefaultProfile
	}
	if !profileName.MatchString(profile) {
		return Paths{}, fmt.Errorf("profile %q: use lowercase letters, digits and dashes, up to 32 characters", profile)
	}
	home, err := os.UserHomeDir()
	if err != nil && (os.Getenv("YAD_CONFIG_DIR") == "" || os.Getenv("YAD_DATA_DIR") == "") {
		return Paths{}, fmt.Errorf("no home directory: set YAD_CONFIG_DIR and YAD_DATA_DIR: %w", err)
	}
	p := Paths{Profile: profile}
	p.Config = pick("YAD_CONFIG_DIR", "XDG_CONFIG_HOME", filepath.Join(home, ".config"), profile)
	p.Data = pick("YAD_DATA_DIR", "XDG_DATA_HOME", filepath.Join(home, ".local", "share"), profile)
	return p, nil
}

func pick(override, xdg, fallback, profile string) string {
	if d := os.Getenv(override); d != "" {
		return d
	}
	base := fallback
	if d := os.Getenv(xdg); d != "" {
		base = d
	}
	dir := filepath.Join(base, "yad")
	if profile != DefaultProfile {
		dir = filepath.Join(dir, "profiles", profile)
	}
	return dir
}

// ConfigFile is config.toml.
func (p Paths) ConfigFile() string { return filepath.Join(p.Config, "config.toml") }

// StateDB is the SQLite file holding sessions, the event spool and the outbox.
func (p Paths) StateDB() string { return filepath.Join(p.Data, "state.db") }

// Socket is the control socket for this profile's CLI.
func (p Paths) Socket() string { return filepath.Join(p.Data, "yad.sock") }

// Ensure creates both directories, private to the owner: they hold credentials
// and harness transcripts.
func (p Paths) Ensure() error {
	return errors.Join(os.MkdirAll(p.Config, 0o700), os.MkdirAll(p.Data, 0o700))
}
