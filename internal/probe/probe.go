// Package probe is what harness detection and host-tool detection share:
// finding the executable behind a catalog entry, and saying what is wrong with
// it in words that may leave the machine.
//
// The two used to do this each in their own way, and drifted: an override
// naming nothing made a harness present and broken and a host tool absent,
// and a binary that would not start sent a harness's owner to run it and a
// host tool's owner to `ls -l` a file already proved executable (DEV-68). A
// hub reads both from the same capability document and cannot know the rule
// differs by kind, so there is one rule, and it lives here.
//
// Every sentence here names a variable or a command and never a path or a
// word a child printed: the capability document reaches every connected hub,
// a path under /Users/<name> is the owner's name, and a child's stderr is
// unbounded text nobody vetted (DEV-60, DEV-67).
package probe

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/skkap/yad/internal/shellword"
)

// Found is where a catalog entry's binary is, and what finding it had to say.
//
// Path empty is absent. Warning and Error are never both set: Warning is an
// override that names nothing while PATH has the binary, which is still
// drivable; Error is the same override with PATH empty-handed, which is absent
// and worth explaining.
type Found struct {
	Path string
	// FromOverride says Path is what the override named, which is where a
	// binary that will not start sends its owner.
	FromOverride bool
	Warning      string
	Error        string

	envVar, name string
	versionArgs  []string
}

// Find locates name, preferring the file envVar names.
//
// An override "names nothing" when nothing is at that path or what is there is
// a directory (decided 2026-09-22, DEV-68). That is the owner's mistake rather
// than the machine's state, so it is reported — but it does not hide a binary
// PATH has: the override exists for a daemon started without a shell's PATH,
// and one left pointing at a version since uninstalled should not take a
// working machine off every hub. Anything else at the path is taken as the
// binary, and whether it runs is the version probe's to find out: a file
// without its execute bit is installed and broken, which is what the owner
// needs to hear, and is not the same as nothing there.
func Find(envVar, name string, versionArgs []string) Found {
	f := Found{envVar: envVar, name: name, versionArgs: versionArgs}
	dead := false
	if v := os.Getenv(envVar); v != "" {
		if !namesNothing(v) {
			f.Path, f.FromOverride = v, true
			return f
		}
		dead = true
	}
	// LookPath, unlike an override, has already checked that the file is there
	// and can be executed.
	if path, err := exec.LookPath(name); err == nil {
		f.Path = path
	}
	switch {
	case !dead:
	case f.Path != "":
		f.Warning = fmt.Sprintf("%s names no file (nothing is there, or it is a directory), so the %s on PATH is used — point it at the %s binary, or unset it", envVar, name, name)
	default:
		f.Error = fmt.Sprintf("%s names no file (nothing is there, or it is a directory), and there is no %s on PATH either — point it at the %s binary, or unset it and install %s on PATH", envVar, name, name, name)
	}
	return f
}

// namesNothing is the rule itself. A stat that fails for any other reason — a
// directory on the way that this user may not search — is not proof that
// nothing is there, so the path is kept and the probe reports what it finds.
func namesNothing(path string) bool {
	fi, err := os.Stat(path)
	switch {
	case err == nil:
		return fi.IsDir()
	// ENOTDIR is a path through a file, /usr/bin/git/git: nothing can be there.
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return true
	}
	return false
}

// Command is the binary that was probed, run with args, as its owner would
// paste it: built from argv, every word quoted, because the messages below
// set it apart as a command to run (AGENTS.md) whatever a catalog entry's
// name holds today.
//
// The program is the one detection found. Found on PATH, that is its name,
// which the owner's shell resolves the same way. Found through the override,
// the name would run PATH's copy, or nothing — the override exists because
// PATH lacks it — so the program is the variable instead, "$YAD_CODEX_PATH":
// the file itself, without printing a path that is under its owner's home
// and may not leave the machine (DEV-67, DEV-108). Such a command runs only
// where the variable is set, which Try says beside it.
func (f Found) Command(args ...string) string {
	program := shellword.Quote(f.name)
	if f.FromOverride {
		program = shellword.Expand(f.envVar)
	}
	if len(args) == 0 {
		return program
	}
	return program + " " + shellword.Command(args...)
}

// VersionCommand is Command for the version probe.
func (f Found) VersionCommand() string { return f.Command(f.versionArgs...) }

// Try is the advice to run command by hand to see why: "run `command` on this
// machine" and what it will show. Where command names the override's
// variable, it says the variable has to be set in the shell it is pasted into
// — the daemon's environment is not the reader's, and "$YAD_CODEX_PATH" unset
// is an empty program.
func (f Found) Try(command, toSee string) string { return f.run("`"+command+"`", toSee) }

// TryIt is Try for a message that has already set the command apart, which
// it names as "it".
func (f Found) TryIt(toSee string) string { return f.run("it", toSee) }

// Do is advice to run command by hand to put something right: "run `command`
// on this machine to purpose", with the variable noted as Try notes it.
func (f Found) Do(command, purpose string) string {
	return f.shell(fmt.Sprintf("run `%s` on this machine to %s", command, purpose))
}

func (f Found) run(what, toSee string) string {
	return f.shell(fmt.Sprintf("run %s on this machine to see %s", what, toSee))
}

func (f Found) shell(s string) string {
	if f.FromOverride {
		s += fmt.Sprintf(", with %s set in that shell to the path the runner has", f.envVar)
	}
	return s
}

// WontStart is a binary that was found and could not be started.
//
// Worded by where it came from, because what is still worth checking differs.
// An override has been proved to name a file and nothing more, so the file is
// the thing to fix. LookPath has already proved the other one executable, so
// telling its owner to check that would send them to `ls -l` and a dead end:
// what is left is a missing interpreter, a binary for another architecture, or
// this machine failing to fork, and running it by hand is what tells them
// which.
func (f Found) WontStart() string {
	if f.FromOverride {
		return fmt.Sprintf("%s does not name a %s this runner can start — point it at an executable %s, or unset it and let PATH decide", f.envVar, f.name, f.name)
	}
	return fmt.Sprintf("the %s on PATH will not start — %s", f.name, f.Try(f.VersionCommand(), "what stops it"))
}

// NoAnswer is a probe of this binary with args that never came back. It names
// the command and the wait and nothing else, and gives the action because this
// is the case where it is worth most: a CLI that hangs on its own version flag
// has stopped telling its owner anything at all.
func (f Found) NoAnswer(waited time.Duration, args ...string) string {
	return fmt.Sprintf("no answer to `%s` within %s — %s", f.Command(args...), waited, f.TryIt("what it waits on"))
}

// WontAnswer is a probe of this binary with args that ran and failed. Neither
// the exit status nor the child's stderr is quoted; running it by hand tells
// its owner more than a tail would, and tells a hub nothing it should have.
func (f Found) WontAnswer(args ...string) string {
	return fmt.Sprintf("`%s` exited with an error — %s", f.Command(args...), f.TryIt("why"))
}
