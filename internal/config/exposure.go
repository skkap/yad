package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// geteuid is swapped by tests, the same seam the Claude adapter uses for its
// own root check: no test suite may run as root to prove what happens there.
var geteuid = os.Geteuid

// otherUsers are the group and other permission bits. A file or directory with
// any of them set is reachable by someone who is not the owner, which is the
// condition ReadSecret refuses and control.checkDir refuses — not a literal
// "is it 0600", so a 0400 credential an owner tightened by hand is not scolded
// for it.
const otherUsers os.FileMode = 0o077

// privateFile is one of the profile's long-lived files, with the reason its
// mode is worth a warning of its own rather than being left to the directory
// check above it.
type privateFile struct {
	path string
	why  string
}

// privateFiles are the files this profile keeps between runs that no other user
// should reach: the secrets, the identity a hub keys its sessions by, and the
// two stores. Ephemeral files are deliberately absent — the control socket, the
// lock, the logs and a run's grant files are all inside the data directory
// whose reachability is checked as a whole, and a grant file is deleted when
// its run ends. An explicit list rather than a walk of the directory, because
// config.toml is legitimately world-readable and a check that cried wolf over
// it would be turned off.
func privateFiles(p Paths) []privateFile {
	files := []privateFile{
		{filepath.Join(p.Config, "runner-id"), "another machine claiming this id would take over this runner's sessions"},
		{p.HubAdminToken(), "it is the admin token for this machine's hub"},
		{p.StateDB(), "it holds every run's events, which carry what the harness did"},
		{p.HubDB(), "it holds this hub's runs and the hashes of its tokens"},
	}
	// Credentials are one file per connection and named by the owner, so they
	// can only be found by reading the directory.
	entries, err := os.ReadDir(filepath.Join(p.Config, "credentials"))
	if err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			files = append(files, privateFile{
				filepath.Join(p.Config, "credentials", n),
				fmt.Sprintf("it is the runner credential for the connection %q, and one that has been readable by others must be assumed leaked", n),
			})
		}
	}
	return files
}

// Exposures reports what about this process and this profile puts a runner's
// secrets, or the harnesses it starts, within reach of a user other than its
// owner. The order is fixed so two runs on an unchanged machine print the same
// thing, and a machine with nothing wrong returns nil.
//
// Every one is a warning and none is an error: `yad doctor` is what an owner
// runs to find out what is wrong with a machine, and a diagnostic that refuses
// to run because the machine is misconfigured tells them nothing they could act
// on. The places where an exposure would actually leak refuse on their own —
// ReadSecret will not hand out a credential others can read, and the control
// server will not bind a socket in a directory others can reach.
//
// A file that is not there is not an exposure: a fresh profile has no
// credentials and no store, and that is the machine this runs on most.
func Exposures(p Paths) []string {
	var out []string
	if geteuid() == 0 {
		// Only the owner may declare the sandbox, in the runner's own
		// environment (ARCHITECTURE.md §3), and it is the one thing that lets a
		// Claude run start as root at all — so it changes what there is to say,
		// not whether there is anything to say. Every harness still runs as
		// root either way, which is the fact the owner is being told.
		if os.Getenv("IS_SANDBOX") == "1" {
			out = append(out, "running as root with IS_SANDBOX=1 — Claude Code will start, but every harness this runner runs has root on this machine")
		} else {
			out = append(out, "running as root — every harness this runner runs would have root, and Claude Code refuses the default permission mode there; run yad as an ordinary user")
		}
	}
	for _, d := range []struct{ what, path string }{
		{"config", p.Config},
		{"data", p.Data},
	} {
		fi, err := os.Stat(d.path)
		if err != nil || !fi.IsDir() {
			continue
		}
		if fi.Mode().Perm()&otherUsers != 0 {
			out = append(out, fmt.Sprintf("the %s directory %s is %v — another user on this machine can reach what is in it; chmod 700 %s", d.what, d.path, fi.Mode().Perm(), d.path))
		}
	}
	for _, f := range privateFiles(p) {
		fi, err := os.Stat(f.path)
		if err != nil || fi.IsDir() {
			continue
		}
		if fi.Mode().Perm()&otherUsers != 0 {
			out = append(out, fmt.Sprintf("%s is %v — %s; chmod 600 %s", f.path, fi.Mode().Perm(), f.why, f.path))
		}
	}
	return out
}
