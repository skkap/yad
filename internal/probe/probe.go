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
	"strings"
	"syscall"
	"time"
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

// Command is the version probe as its owner would type it.
func (f Found) Command() string {
	return strings.TrimSpace(f.name + " " + strings.Join(f.versionArgs, " "))
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
	return fmt.Sprintf("the %s on PATH will not start — run `%s` on this machine to see what stops it", f.name, f.Command())
}

// NoAnswer is a probe the binary never came back from. It names the command
// and the wait and nothing else, and gives the action because this is the case
// where it is worth most: a CLI that hangs on its own version flag has stopped
// telling its owner anything at all.
func NoAnswer(command string, waited time.Duration) string {
	return fmt.Sprintf("no answer to `%s` within %s — run it on this machine to see what it waits on", command, waited)
}

// WontAnswer is a probe that ran and failed. Neither the exit status nor the
// child's stderr is quoted; running it by hand tells its owner more than a
// tail would, and tells a hub nothing it should have.
func WontAnswer(command string) string {
	return fmt.Sprintf("`%s` exited with an error — run it on this machine to see why", command)
}
