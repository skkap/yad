//go:build unix

// This file reads a directory's owning uid, which needs syscall.Stat_t. The
// tag follows internal/control, which is unix for the same reason, and the
// binary is already unix-only through it — the release targets are linux and
// darwin.

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// shellArg quotes a path so the command a warning prints can be pasted and
// run. A profile directory comes from YAD_CONFIG_DIR, YAD_DATA_DIR, XDG_* or
// $HOME and none of those is constrained to shell-safe characters: under
// YAD_DATA_DIR="/Volumes/My Disk/yad" the unquoted advice runs
// `chmod 700 /Volumes/My` and leaves the exposure open. A next action that
// does not work is the defect this whole file is about.
func shellArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\v\\\"'`$&|;<>()*?[]{}!#~=%") {
		return s
	}
	// POSIX single quotes take everything literally; the only character that
	// cannot appear inside them is the quote itself.
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

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
// check above it, and the next action for that file.
//
// fix is per-file because the chmod is not the whole answer for the three that
// hold a secret — a connection's credential, the hub admin token and hub.db. A
// chmod stops the next reader; it does nothing about the one who already read
// it, and a secret that has been readable by others must be assumed leaked
// (the premise ReadSecret already refuses on). Telling an owner only to chmod
// a leaked credential leaves them feeling finished while still using it.
type privateFile struct {
	path string
	why  string
	fix  string
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
	runnerID := filepath.Join(p.Config, "runner-id")
	admin := p.HubAdminToken()
	files := []privateFile{
		// An identity rather than a secret. Reading it takes nothing over:
		// hub.Register refuses a generic token for an id already registered
		// and a ForRunner token for a different runner, so the id alone is
		// not enough. It is here because yad writes it 0600 and something
		// changed that, which is a fact about the directory it sits in.
		{runnerID, "yad writes this 0600 and something has changed it; the id itself is not a secret, but the credentials beside it are", "chmod 600 " + shellArg(runnerID)},
		// Not a secret either, and on this list for the reason config.Save
		// gives for writing it 0600: it names the hubs this runner connects to
		// and the accounts it holds, which is enough to be worth keeping
		// private. An owner who wrote it by hand under a normal umask is the
		// common case, so the fix is the mode and nothing more.
		{p.ConfigFile(), "it names the hubs this runner connects to and the accounts it holds, which is why yad writes it 0600", "chmod 600 " + shellArg(p.ConfigFile())},
		// The sequence `yad hub admin-token create` itself prints when it
		// refuses: revoke only touches hub.db, so create refuses while this
		// file is still here, and deleting it is the step between.
		{admin, "it is the admin token for this machine's hub", "chmod 600 " + shellArg(admin) + " stops the next reader, but not the one who already read it, so revoke it (`yad hub admin-token list`, then revoke), delete " + shellArg(admin) + ", and `yad hub admin-token create` again"},
		// The runner's own store holds no grant: Loop.record strips them
		// before writing, so a run is stored without the secrets it carried.
		{p.StateDB(), "it holds every run's events, which carry what the harness did", "chmod 600 " + shellArg(p.StateDB())},
		// The hub's store is the opposite, and it keeps them: a run's spec is
		// written once with its grants (hub/store/migrations/0001) and no
		// query clears it — FinishRun sets state, reason and the lease, and
		// nothing deletes a run. So every grant this hub was ever given is
		// still in the file, and the advice cannot be limited to the runs
		// waiting now.
		{p.HubDB(), "it holds every run this hub has been given, with its grants, in plaintext", "chmod 600 " + shellArg(p.HubDB()) + " stops the next reader, but not the one who already read it, so rotate every secret any run's grants have carried"},
	}
	// Credentials are one file per connection and named by the owner, so they
	// can only be found by reading the directory. ReadDir sorts by name, which
	// is what keeps two runs on an unchanged machine printing the same thing.
	// A directory that cannot be read is not a reason to check nothing else.
	entries, _ := os.ReadDir(filepath.Join(p.Config, "credentials"))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(p.Config, "credentials", e.Name())
		files = append(files, privateFile{
			path,
			fmt.Sprintf("it is the runner credential for the connection %q", e.Name()),
			// The same next action config.Credential gives when ReadSecret
			// refuses this file outright, so an owner who meets it here and
			// there is told to do one thing, not two.
			"chmod 600 " + shellArg(path) + " stops the next reader, but not the one who already read it, so revoke this credential at the hub and run `yad connect` again",
		})
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
		// environment (ARCHITECTURE.md §3). It is not the only way a Claude run
		// starts as root — the adapter refuses root only for
		// bypassPermissions, so a narrower permission_mode needs no
		// IS_SANDBOX at all — but it is the one the owner has declared here,
		// so it changes what there is to say rather than whether there is
		// anything to say. Every harness still runs as root either way, which
		// is the fact the owner is being told.
		if os.Getenv("IS_SANDBOX") == "1" {
			// What is settled here is the root question and nothing else: this
			// says a run is no longer refused *for being root*, never that one
			// would start. Exposures has not looked at whether a harness is
			// installed, answers its version probe, or was given a permission
			// mode the adapter accepts — and the harness table right above
			// this line is where that is reported.
			out = append(out, "running as root with IS_SANDBOX=1 — root alone no longer refuses a Claude run, but every harness this runner runs has root on this machine")
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
			out = append(out, fmt.Sprintf("the %s directory %s is %v — another user on this machine can reach what is in it; chmod 700 %s", d.what, d.path, fi.Mode().Perm(), shellArg(d.path)))
		}
		// A 0700 directory owned by somebody else passes the mode check and is
		// still theirs to read and replace. control.checkDir already refuses
		// this before binding the socket; reporting it here is the same rule
		// one step earlier, so an owner meets it in doctor rather than in a
		// daemon that will not start.
		//
		// The real uid, not the effective one the root check above reads:
		// this has to be the same comparison control.checkDir makes, or
		// doctor would call a profile clean that the daemon then refuses.
		if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
			out = append(out, fmt.Sprintf("the %s directory %s belongs to uid %d, not to you (%d) — run yad as its owner, or point %s at a directory of your own", d.what, d.path, st.Uid, os.Getuid(), map[string]string{"config": "YAD_CONFIG_DIR", "data": "YAD_DATA_DIR"}[d.what]))
		}
	}
	// Only the mode here, where the directories above are also checked for
	// their owner. That is not an omission: a file owned by somebody else
	// inside a directory of yours is reachable by nobody but you, because they
	// cannot traverse a 0700 directory to get to it — and a directory that is
	// not yours is already reported above. The two checks together cover it.
	for _, f := range privateFiles(p) {
		fi, err := os.Stat(f.path)
		if err != nil || fi.IsDir() {
			continue
		}
		if fi.Mode().Perm()&otherUsers != 0 {
			out = append(out, fmt.Sprintf("%s is %v — %s; %s", f.path, fi.Mode().Perm(), f.why, f.fix))
		}
	}
	return out
}
