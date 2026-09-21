package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// homeMachine is a machine whose profiles live where Resolve puts them with no
// variable set: under a fresh HOME.
func homeMachine(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, v := range []string{"YAD_CONFIG_DIR", "YAD_DATA_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		t.Setenv(v, "")
	}
	return home
}

func resolveEnsured(t *testing.T, profile string) Paths {
	t.Helper()
	p, err := Resolve(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	return p
}

// A command printed for the default profile names it whenever the reader's
// shell could pick another: pasted into a shell that exports YAD_PROFILE=work,
// a bare `yad daemon logs` reads work's log and answers as if it were the
// default's. A machine with one profile has no other to pick, and keeps the
// bare command a single-runner owner expects.
func TestDefaultProfileIsNamedWhenAnotherExists(t *testing.T) {
	bare := []string{"yad", "daemon", "logs"}
	named := []string{"yad", "--profile", "default", "daemon", "logs"}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, home string)
		want  []string
	}{
		{"only the default", func(*testing.T, string) {}, bare},
		{"another profile", func(t *testing.T, _ string) { resolveEnsured(t, "work") }, named},
		{"another profile's data alone", func(t *testing.T, home string) {
			mkdir(t, filepath.Join(home, ".local", "share", "yad", "profiles", "work"))
		}, named},
		{"another profile's config alone", func(t *testing.T, home string) {
			mkdir(t, filepath.Join(home, ".config", "yad", "profiles", "work"))
		}, named},
		{"an empty profiles directory", func(t *testing.T, home string) {
			mkdir(t, filepath.Join(home, ".config", "yad", "profiles"))
		}, bare},
		{"a file no profile could be named", func(t *testing.T, home string) {
			dir := filepath.Join(home, ".config", "yad", "profiles")
			mkdir(t, dir)
			if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, bare},
		{"a profiles listing that fails", func(t *testing.T, home string) {
			// A file where the directory should be: the listing fails, and
			// what it could not see counts as another profile.
			if err := os.WriteFile(filepath.Join(home, ".local", "share", "yad", "profiles"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, named},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := homeMachine(t)
			p := resolveEnsured(t, "")
			tc.setup(t, home)
			for _, line := range []string{p.Command("daemon", "logs"), p.RemoteCommand("daemon", "logs"), p.YadCommand("daemon", "logs")} {
				shellwordtest.Check(t, line, tc.want...)
			}
		})
	}
}

// A profile added after the command builder was first used — a daemon that
// has been up for weeks — is seen by the next command it prints.
func TestAProfileAddedLaterIsSeen(t *testing.T) {
	homeMachine(t)
	p := resolveEnsured(t, "")
	shellwordtest.Check(t, p.Command("status"), "yad", "status")
	resolveEnsured(t, "work")
	shellwordtest.Check(t, p.Command("status"), "yad", "--profile", "default", "status")
}

// A directory a YAD_ variable chose has no profiles/ inside it to read, and a
// Paths that never went through Resolve names no directory at all: neither can
// say the default is alone, so both name it. A non-default profile is named
// always.
func TestProfileIsNamedWhenItsSiblingsCannotBeSeen(t *testing.T) {
	homeMachine(t)
	t.Setenv("YAD_DATA_DIR", t.TempDir())
	relocated, err := Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	shellwordtest.CheckEnv(t, relocated.Command("status"), map[string]string{"YAD_DATA_DIR": os.Getenv("YAD_DATA_DIR")},
		"yad", "--profile", "default", "status")
	shellwordtest.Check(t, Paths{}.YadCommand("status"), "yad", "--profile", "default", "status")
	shellwordtest.Check(t, Paths{Profile: DefaultProfile}.YadCommand("status"), "yad", "--profile", "default", "status")

	t.Setenv("YAD_DATA_DIR", "")
	shellwordtest.Check(t, resolveEnsured(t, "work").Command("status"), "yad", "--profile", "work", "status")
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}
