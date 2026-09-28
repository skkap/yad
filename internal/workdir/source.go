//go:build unix

package workdir

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/skkap/yad/internal/config"
)

// A source is hub input, which is data and never an instruction (decision
// 0038: the owner trusts the hubs it connects). Everything here turns one into
// something git can be handed as argv — never through a shell — or refuses it.
// These guards prevent bugs rather than attacks: a URL that reaches a remote
// helper or an argument that begins with '-' is wrong whoever sent it. The
// rules are decision 0033's, as 0038 amends them.

// remote is a git source's repository, checked.
type remote struct {
	// url is what git fetches from: the hub's URL, or for a local repository
	// the path it resolved to.
	url string
	// key names the bare cache: one per repository per runner.
	key string
	// name is the repository's name, WT_REPO, and the start of its cache's
	// directory name.
	name string
}

// helperURL is git's remote-helper syntax, <transport>::<address>. ext:: runs
// a command, and any other helper is a program the hub would be choosing.
var helperURL = regexp.MustCompile(`^[A-Za-z0-9+.-]+::`)

// parseRemote checks a git source's URL. https and ssh reach the network with
// the machine's own credentials (decision 0009); a local repository — a path
// or file:// — only inside the owner's roots. Everything else is refused:
// plain http and git:// carry no integrity, and a remote helper is a program.
func parseRemote(raw string, roots []string) (remote, error) {
	shown := quotedURL(raw)
	if err := plain("git.url", raw, shown); err != nil {
		return remote{}, err
	}
	if helperURL.MatchString(raw) {
		return remote{}, fmt.Errorf("git.url %s names a git remote helper (<transport>::…); use an https or ssh URL", shown)
	}
	r := remote{url: raw, key: strings.TrimRight(raw, "/")}
	switch {
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			// Not url.Parse's error: it quotes the URL whole, and an
			// invalid port is quoted from beside the password (DEV-91).
			return remote{}, fmt.Errorf("git.url %s is not a URL — check it for a character that needs escaping or a port that is not a number", shown)
		}
		switch strings.ToLower(u.Scheme) {
		case "https", "ssh", "git+ssh", "ssh+git":
			if err := checkHost(shown, u.Hostname(), u.User.Username()); err != nil {
				return remote{}, err
			}
			if _, ok := u.User.Password(); ok {
				// A secret in the URL lands in git's argv, its config and every
				// error message. The machine's own credentials reach the
				// repository instead (decision 0009).
				return remote{}, fmt.Errorf("git.url for %s carries a password; this runner reaches repositories with its own credentials — drop it from the URL", u.Host)
			}
			r.name = repoName(u.Path)
		case "file":
			if h := u.Host; h != "" && h != "localhost" {
				return remote{}, fmt.Errorf("git.url %s names a file on another host; use an https or ssh URL", shown)
			}
			return localRemote(raw, u.Path, roots)
		default:
			return remote{}, fmt.Errorf("git.url %s uses %s://, which this runner does not fetch from; use an https or ssh URL", shown, u.Scheme)
		}
	case strings.HasPrefix(raw, "/"):
		return localRemote(raw, raw, roots)
	default:
		// scp-like: [user@]host:path. A colon after a slash makes it a
		// relative path, which git would read against the runner's own
		// directory.
		hostPart, path, ok := strings.Cut(raw, ":")
		if !ok || strings.Contains(hostPart, "/") || path == "" {
			return remote{}, fmt.Errorf("git.url %s is neither a URL, an scp-like user@host:path, nor an absolute path", shown)
		}
		user, host, hasUser := strings.Cut(hostPart, "@")
		if !hasUser {
			user, host = "", hostPart
		}
		if err := checkHost(shown, host, user); err != nil {
			return remote{}, err
		}
		r.name = repoName(path)
	}
	return r, nil
}

// checkHost refuses what ssh would read as an option: a host or a user that
// begins with a dash is the ProxyCommand injection git once shipped. shown is
// the URL as quotedURL prints it.
func checkHost(shown, host, user string) error {
	if host == "" {
		return fmt.Errorf("git.url %s names no host", shown)
	}
	if strings.HasPrefix(host, "-") || strings.HasPrefix(user, "-") {
		return fmt.Errorf("git.url %s has a host or user beginning with '-', which ssh would read as an option", shown)
	}
	return nil
}

// localRemote is a repository on this machine, taken only inside a root.
func localRemote(raw, path string, roots []string) (remote, error) {
	dir, err := inRoots("git.url", path, roots)
	if err != nil {
		return remote{}, err
	}
	return remote{url: dir, key: "file://" + dir, name: repoName(dir)}, nil
}

// inRoots resolves an absolute path, symlinks included, and returns it only
// when it is a directory inside one of the owner's roots. With no roots,
// nothing on the machine is reachable — where the owner has configured none,
// their home directory is the root (config.WorkdirsConfig.EffectiveRoots), so
// a manager reaching this line is one on a machine with no home to fall back
// to.
func inRoots(field, path string, roots []string) (string, error) {
	if len(roots) == 0 {
		return "", fmt.Errorf("%s %q is a directory on this machine, and this runner may reach none — with no [workdirs] roots in config.toml the owner's home directory is used, and this runner has none; list the directories runs may use there", field, path)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s %q is not an absolute path", field, path)
	}
	// Resolved before the check, so neither "..", nor a symlink inside a root
	// that points out of it, gets past.
	dir, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", field, path, errors.Unwrap(err))
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", field, path, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s %q is not a directory", field, path)
	}
	for _, root := range roots {
		r, err := filepath.EvalSymlinks(filepath.Clean(root))
		if err != nil {
			// A root that is gone allows nothing.
			continue
		}
		if within(r, dir) {
			return dir, nil
		}
	}
	return "", fmt.Errorf("%s %q is outside the directories this runner's owner allows (%s) — they add one under [workdirs] roots in config.toml", field, path, strings.Join(roots, ", "))
}

// within reports whether path is root or below it. Both are clean and
// absolute.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// plain refuses what has no business in a git argument: nothing, a leading
// dash that git would read as an option, whitespace and control characters.
// shown is s as a refusal may quote it, which for a URL is quotedURL's.
func plain(field, s, shown string) error {
	switch {
	case s == "":
		return fmt.Errorf("%s is empty", field)
	case strings.HasPrefix(s, "-"):
		return fmt.Errorf("%s %s begins with '-', which git would read as an option", field, shown)
	case strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0:
		return fmt.Errorf("%s %s contains whitespace or a control character", field, shown)
	}
	return nil
}

// shownURL is a hub-sent git URL as a refusal, an event or a run's error may
// print it. It goes back only to the hub that sent it, which already holds it
// — but a URL is where people put a token, and a token is never transmitted
// (decision 0063, as DEV-91 for a hub's own URL). A URL loses its userinfo,
// query and fragment to config.RedactURL; an scp-like user@host:path loses its
// user the same way; anything else holding an @ is not printed at all. A
// source with nothing to take out — a path, most URLs — comes back as given.
func shownURL(raw string) string {
	if strings.Contains(raw, "://") {
		return config.RedactURL(raw)
	}
	if !strings.Contains(raw, "@") {
		return raw
	}
	hostPart, path, ok := strings.Cut(raw, ":")
	_, host, _ := strings.Cut(hostPart, "@")
	if !ok || strings.Contains(hostPart, "/") || strings.Contains(host, "@") || strings.Contains(path, "@") {
		return config.UnprintableURL
	}
	return "redacted@" + host + ":" + path
}

// quotedURL is shownURL in quotes, as a refusal names a field's value — except
// for a URL that cannot be printed, which is already a phrase.
func quotedURL(raw string) string {
	s := shownURL(raw)
	if s == config.UnprintableURL {
		return s
	}
	return strconv.Quote(s)
}

// checkRef enforces git's own rules for a ref name (git-check-ref-format),
// and the ones it leaves to the caller: no leading dash, and not HEAD.
func checkRef(field, s string) error {
	if err := plain(field, s, strconv.Quote(s)); err != nil {
		return err
	}
	bad := func(why string) error { return fmt.Errorf("%s %q is not a valid git ref name: %s", field, s, why) }
	switch {
	case len(s) > 255:
		return bad("longer than 255 bytes")
	case s == "@" || s == "HEAD":
		return bad("it names the current commit, not a branch")
	case strings.ContainsAny(s, "~^:?*[\\"):
		return bad(`it contains one of ~ ^ : ? * [ \`)
	case strings.Contains(s, "..") || strings.Contains(s, "@{") || strings.Contains(s, "//"):
		return bad(`it contains "..", "@{" or "//"`)
	case strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.HasSuffix(s, "."):
		return bad("it begins or ends with '/', or ends with '.'")
	}
	for _, part := range strings.Split(s, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return bad("a component begins with '.' or ends with .lock")
		}
	}
	return nil
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// repoName is the last element of a repository path without ".git", made safe
// to be one path component: WT_REPO, and a hook may name containers after it.
func repoName(path string) string {
	name := strings.TrimSuffix(filepath.Base(strings.TrimRight(path, "/")), ".git")
	name = strings.Trim(unsafeName.ReplaceAllString(name, "-"), ".-")
	if name == "" {
		return "repo"
	}
	return name
}

// digest is a short stable hash, for directory names derived from hub input.
func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}
