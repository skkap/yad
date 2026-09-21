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

// closeAnd is a next action: the chmod, plus the rotation when what is behind
// the file is a secret. The rotation is passed in rather than derived from the
// path, so a file that holds a secret and one that does not cannot be told
// apart by accident.
func closeAnd(path, rotate string) string {
	fix := "chmod 600 " + shellArg(path)
	if rotate == "" {
		return fix
	}
	return fix + " stops the next reader, but not the one who already read it, so " + rotate
}

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
// fix is per-file, and deliberately not derived from a flag, because the chmod
// is not the whole answer for an entry that holds a secret: it stops the next
// reader and does nothing about the one who already read it, and a secret that
// has been readable by others must be assumed leaked. An owner told only to
// chmod a leaked credential feels finished while still using it. Which entries
// those are is the list below and not this comment — a count here is one more
// thing to leave stale.
type privateFile struct {
	path string
	why  string
	fix  string
}

// privateFiles are the files this profile keeps between runs that no other user
// should reach. Each entry says what is at stake and what to do about it,
// because those differ: an identity is closed, a secret is closed and then
// retired.
//
// Ephemeral files are deliberately absent. The control socket, the lock, the
// logs and a run's grant files live inside the data directory whose
// reachability is checked as a whole, and a grant file is deleted when its run
// ends. SQLite's -wal and -shm are the exception and are here: they take the
// database's own mode, so a database that drifted to 0644 hands the same bits
// to the file holding the pages it has not taken yet — and a clean close
// removes them, but an uncleaned exit does not. A killed runner leaves them on
// disk, which is the same state the start-up grant sweep exists for.
//
// An explicit list rather than a walk, so that adding a file to the profile is
// a decision about whether an owner should hear about it rather than something
// a directory listing decides.
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
		// Not necessarily this machine's hub: the same file is the default
		// token for submitting to any --hub or $YAD_HUB_URL (cmd_hub_run.go,
		// ARCHITECTURE.md §4), so naming `yad hub admin-token` alone would
		// point a remote hub's owner at a local hub.db that never issued it.
		// The delete is named because revoke only touches the database, and
		// create refuses while the file is still there.
		{admin, "it is an admin token for a hub's service API", "chmod 600 " + shellArg(admin) + " stops the next reader, but not the one who already read it, so revoke it at the hub that issued it — for a hub this machine serves that is `yad hub admin-token list` then revoke, delete " + shellArg(admin) + ", and `yad hub admin-token create` again"},
	}
	// Each store contributes its own entry and its two sidecars together, from
	// one description, so a sidecar cannot end up saying something different
	// from the database it belongs to — which is what happened when the two
	// were written apart.
	//
	// What each store gives up differs, so each names its own rotation rather
	// than sharing one or going without.
	//
	// The runner's holds no grant — Loop.record strips them before writing —
	// but its events carry tool results, which are whatever the harness
	// printed, and no query ever deletes an event body: AckEvents and
	// DropEvents only flip `acked`. So a run that printed a credential left it
	// there. That is conditional, and the advice says so rather than telling
	// an owner to rotate something that may not exist.
	//
	// The hub's is unconditional: a run's spec carries its grants in
	// plaintext until the run ends, when the schema blanks their values
	// (hub/store/migrations/0001, decision 0041). So an exposed hub.db gives
	// up the grants of every run not yet ended — queued, offered, held or
	// waiting — and not the whole history, with two widenings. In WAL mode
	// the blanking reaches hub.db only at a checkpoint, and until SQLite
	// reuses them the -wal keeps frames written while a run was live, so
	// between them they hold the grants of runs that ended since the hub last
	// stopped cleanly. And the hub stores every event its runners upload,
	// which nothing blanks — the same conditional exposure as state.db's.
	for _, st := range []struct{ path, holds, rotate string }{
		{p.StateDB(), "every run's events, which carry what the harness did and anything it printed", "if a run ever printed a credential, its events still hold it — treat that one as exposed too"},
		{p.HubDB(), "the grants of every run this hub has not seen end, in plaintext, and every run's events, which carry anything the harness printed", "rotate every secret carried by a grant of a run that has not ended or that ended since this hub last stopped cleanly — until a checkpoint, the database and its -wal still hold those — and if a run ever printed a credential, its events still hold it too"},
	} {
		base := filepath.Base(st.path)
		files = append(files, privateFile{st.path, "it holds " + st.holds, closeAnd(st.path, st.rotate)})
		// SQLite creates both with the database's own mode — measured in
		// store.TestSidecarsTakeTheDatabaseMode, and not narrowed by the
		// umask. The -wal carries the pages the database has not taken yet, so
		// it gives up whatever the database would; the -shm is the index into
		// the -wal and holds no run data, and is reported because its mode is
		// the database's and says so.
		files = append(files,
			privateFile{st.path + "-wal", "SQLite gave it " + base + "'s mode, and it holds the pages " + base + " has not yet checkpointed — so it gives up " + st.holds,
				closeAnd(st.path+"-wal", st.rotate) + ", and fix " + shellArg(st.path) + " too or the next start hands the mode straight back"},
			privateFile{st.path + "-shm", "SQLite gave it " + base + "'s mode; it indexes " + base + "-wal and holds no run data itself, so the mode is what to fix",
				"chmod 600 " + shellArg(st.path+"-shm") + ", and fix " + shellArg(st.path) + " too or the next start hands the mode straight back"},
		)
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
// on.
//
// Some of what it reports is refused elsewhere and some is not. ReadSecret will
// not hand out a credential or an admin token others can read, and the control
// server will not bind its socket in a data directory others can reach. Nothing
// refuses an exposed state.db, hub.db or config.toml, and nothing looks at the
// config directory at all — which is the reason to report them here rather than
// a reason not to.
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
		// still theirs to read and replace. control.checkDir makes the same
		// comparison, but only ever on the data directory — control.Claim is
		// its one caller — so for the data directory this is the daemon's
		// refusal met earlier, and for the config directory it is the only
		// place an owner hears it at all.
		//
		// The real uid, not the effective one the root check above reads,
		// because that is the comparison checkDir makes: doctor must not call
		// a data directory clean that the daemon then refuses.
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
