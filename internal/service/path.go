package service

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ExecRunner runs commands on this machine.
type ExecRunner struct{}

// shellTimeout caps how long the owner's login shell may take to print PATH.
// rc files that update plugins or prompt on a missing tty can hang; install
// then falls back to its own PATH rather than waiting.
const shellTimeout = 10 * time.Second

// pipeGrace bounds the wait for output after the command is killed: an rc
// file that backgrounds something (an agent, a version-manager daemon) leaves
// a process holding stdout open long after the shell has gone.
const pipeGrace = 2 * time.Second

func (ExecRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = pipeGrace
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if msg != "" {
			return out.Bytes(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return out.Bytes(), fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out.Bytes(), nil
}

// pathMarker brackets PATH in the shell's output, because rc files print
// banners, greetings and warnings to stdout before and after it.
const pathMarker = "__YAD_LOGIN_PATH__"

// loginShells are the shells asked for PATH: they share a POSIX `printf` and
// `$PATH`. fish, nushell and friends fall back to the installing PATH.
var loginShells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// LoginPATH is the PATH the owner's login shell builds. A service starts with
// launchd's or systemd's minimal PATH, which holds none of ~/.local/bin,
// Homebrew or a version manager's shims, so a runner started without this
// finds no harness — and a harness finds no git or node.
//
// The shell is run both interactive and login, because owners set PATH in
// either kind of file; when it cannot be read, the installing process's own
// PATH is used, and the returned note says so.
func LoginPATH(ctx context.Context, run Runner, shell, fallback string) (path, note string) {
	if loginShells[filepath.Base(shell)] && filepath.IsAbs(shell) {
		for _, flags := range []string{"-ilc", "-lc"} {
			sctx, cancel := context.WithTimeout(ctx, shellTimeout)
			out, err := run.Run(sctx, shell, flags, `printf '\n`+pathMarker+`%s`+pathMarker+`\n' "$PATH"`)
			cancel()
			if p := cleanPATH(markedPATH(out)); p != "" {
				return p, ""
			}
			if err != nil && ctx.Err() != nil {
				break
			}
		}
		fallback = cleanPATH(fallback)
		return fallback, fmt.Sprintf("could not read PATH from %s; the service uses this shell's PATH instead", shell)
	}
	fallback = cleanPATH(fallback)
	if shell == "" {
		return fallback, "SHELL is not set; the service uses this shell's PATH"
	}
	return fallback, fmt.Sprintf("%s is not a shell yad reads PATH from; the service uses this shell's PATH", shell)
}

// markedPATH is the last marked PATH in a shell's output.
func markedPATH(out []byte) string {
	s := string(out)
	end := strings.LastIndex(s, pathMarker)
	if end < 0 {
		return ""
	}
	start := strings.LastIndex(s[:end], pathMarker)
	if start < 0 {
		return ""
	}
	return s[start+len(pathMarker) : end]
}

// cleanPATH keeps the absolute, distinct entries of a PATH in order. An empty
// or relative entry resolves against the service's working directory — the
// owner's home — so a service would run whatever `git` a file there is named.
func cleanPATH(p string) string {
	seen := map[string]bool{}
	var keep []string
	for _, dir := range strings.Split(p, ":") {
		if !filepath.IsAbs(dir) || strings.ContainsAny(dir, "\n\r\x00") || seen[dir] {
			continue
		}
		seen[dir] = true
		keep = append(keep, dir)
	}
	return strings.Join(keep, ":")
}
