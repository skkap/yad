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

	v1 "github.com/skkap/yad/protocol/v1"

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
	// cred is what an https URL's userinfo carried, taken out of url and key:
	// a credential for this run alone, like a grant (decision 0068).
	cred *credential
}

// helperURL is git's remote-helper syntax, <transport>::<address>. ext:: runs
// a command, and any other helper is a program the hub would be choosing.
var helperURL = regexp.MustCompile(`^[A-Za-z0-9+.-]+::`)

// parseRemote checks a git source's URL. https and ssh reach the network with
// the machine's own credentials (decision 0009), or for https with the one
// the URL's userinfo carries, which is taken out of the URL git is given and
// the cache is keyed by (decision 0068); a local repository — a path or
// file:// — only inside the owner's roots. Everything else is refused: plain
// http and git:// carry no integrity, and a remote helper is a program.
func parseRemote(raw string, reach reachFunc) (remote, error) {
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
		case "https":
			if err := checkHost(shown, u.Hostname(), ""); err != nil {
				return remote{}, err
			}
			if u.User != nil {
				cred, err := credentialOf(u)
				if err != nil {
					return remote{}, err
				}
				r.cred = cred
				r.url = withoutCredential(raw)
				r.key = strings.TrimRight(r.url, "/")
			}
			r.name = repoName(u.Path)
		case "ssh", "git+ssh", "ssh+git":
			if err := checkHost(shown, u.Hostname(), u.User.Username()); err != nil {
				return remote{}, err
			}
			if _, ok := u.User.Password(); ok {
				// ssh's user is the login name, and ssh takes no password
				// from a URL: git would put it in argv and the cache's config
				// for nothing. The machine's key reaches the repository.
				return remote{}, fmt.Errorf("git.url for %s carries a password, which ssh does not take from a URL — drop it; this runner reaches an ssh repository with its own key, or send an https URL with the credential in it", u.Host)
			}
			r.name = repoName(u.Path)
		case "file":
			if h := u.Host; h != "" && h != "localhost" {
				return remote{}, fmt.Errorf("git.url %s names a file on another host; use an https or ssh URL", shown)
			}
			return localRemote(raw, u.Path, reach)
		default:
			return remote{}, fmt.Errorf("git.url %s uses %s://, which this runner does not fetch from; use an https or ssh URL", shown, u.Scheme)
		}
	case strings.HasPrefix(raw, "/"):
		return localRemote(raw, raw, reach)
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

// credential is an https source URL's userinfo: a token as the user, or a user
// and a password. It is the run's, as a grant is (decision 0009): git is given
// the URL without it, and it reaches git only in the environment of the run's
// own commands that talk to the remote (credential.env) — never argv, which
// every user on the machine can read, and never the cache's config, which
// outlives the run and which every later run, from any hub, reads (decision
// 0068).
type credential struct {
	user, password string
	// userOnly is userinfo with no password: a token as the user, or a user's
	// name alone, which nothing in the URL tells apart (userEnv).
	userOnly bool
}

// credentialOf is u's userinfo as git's credential protocol will carry it:
// one key=value per line, so a line break or other control character — sent
// as %0A — would be read as a key of its own, a host or a URL git then trusts.
func credentialOf(u *url.URL) (*credential, error) {
	pass, hasPassword := u.User.Password()
	c := &credential{user: u.User.Username(), password: pass, userOnly: !hasPassword}
	if strings.IndexFunc(c.user+c.password, unicode.IsControl) >= 0 {
		return nil, fmt.Errorf("git.url for %s carries a credential with a control character in it, which git cannot be handed — send the credential as it is issued", u.Host)
	}
	return c, nil
}

// credentialHelper answers git's "get" with the credential from the two
// variables beside it and ignores "store" and "erase", so nothing is kept.
// Fixed text: no hub string is in it, and the values reach the shell only as
// quoted expansions. printf, not echo, so a value beginning with '-' or holding
// a backslash is printed as it is.
const credentialHelper = `!f() { test "$1" = get || { cat >/dev/null; exit 0; }; printf 'username=%s\npassword=%s\n' "$YAD_GIT_USERNAME" "$YAD_GIT_PASSWORD"; }; f`

// env is what a git command that talks to the remote at fetchURL runs with to
// present the credential: git's own config taken from the environment
// (GIT_CONFIG_COUNT, git 2.31 and later), which neither argv nor any file
// holds. Scoped to the remote's scheme, host and port, so a redirect to
// another host is never handed it. The empty helper first clears every helper
// the owner's config lists for that host: one of them would otherwise answer
// before this one, and on success git asks each to store what worked — a
// keychain would keep the hub's token for good.
func (c *credential) env(fetchURL string) []string {
	scope, ok := credentialScope(c, fetchURL)
	if !ok {
		return nil
	}
	return []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=" + scope + ".helper", "GIT_CONFIG_VALUE_0=",
		"GIT_CONFIG_KEY_1=" + scope + ".helper", "GIT_CONFIG_VALUE_1=" + credentialHelper,
		"YAD_GIT_USERNAME=" + c.user, "YAD_GIT_PASSWORD=" + c.password,
	}
}

// userEnv is the second try for a URL whose userinfo was a user alone, once
// the remote has refused it as a token with no password: the user as the
// name git asks the owner's own helpers for, as git did with the user in the
// URL. Azure DevOps and Bitbucket put the account's name in the clone URLs
// they hand out, and a helper such as Git Credential Manager answers for that
// name. This is exactly how git treated the user in the URL before decision
// 0068, with its one exposure: a helper of the owner's that answers with a
// password alone leaves the user as the name, and on success git asks every
// helper to store that pair — a token as the user included. The first try,
// with the owner's helpers cleared, is what keeps that to a remote which
// refused the user as a token. The user reaches git in the environment still,
// never argv or the cache's config.
func (c *credential) userEnv(fetchURL string) []string {
	scope, ok := credentialScope(c, fetchURL)
	if !ok || !c.userOnly {
		return nil
	}
	return []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=" + scope + ".username", "GIT_CONFIG_VALUE_0=" + c.user}
}

// credentialScope is the config subsection that holds c for the remote at
// fetchURL: credential.<scheme>://<host[:port]>.
func credentialScope(c *credential, fetchURL string) (string, bool) {
	if c == nil {
		return "", false
	}
	u, err := url.Parse(fetchURL)
	if err != nil {
		return "", false
	}
	return "credential." + strings.ToLower(u.Scheme) + "://" + u.Host, true
}

// withoutCredential is a git URL as it may be stored, keyed by or handed to
// git: an https URL without its userinfo, an ssh URL without a password —
// its user is the login name, not a credential. Cut from the string rather
// than rebuilt by net/url, so nothing else in the URL changes spelling and a
// URL that does not parse loses its userinfo too. Anything that is not
// scheme://… comes back as given.
func withoutCredential(raw string) string {
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return raw
	}
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	at := strings.LastIndex(rest[:end], "@")
	if at < 0 {
		return raw
	}
	switch strings.ToLower(scheme) {
	case "ssh", "git+ssh", "ssh+git":
		user, _, hasPassword := strings.Cut(rest[:at], ":")
		if !hasPassword {
			return raw
		}
		return scheme + "://" + user + rest[at:]
	}
	return scheme + "://" + rest[at+1:]
}

// StoredSources is a copy of sources as they may be written anywhere — the
// run's spec and the session's sources in state.db — or compared: every git
// URL without the credential its userinfo carried (withoutCredential). The
// credential lives only in the claim, in memory, for the run that was sent it.
func StoredSources(sources []v1.Source) []v1.Source {
	if sources == nil {
		return nil
	}
	out := make([]v1.Source, len(sources))
	for i, s := range sources {
		out[i] = s
		if s.Git != nil {
			g := *s.Git
			g.URL = withoutCredential(g.URL)
			out[i].Git = &g
		}
	}
	return out
}

// CarriesCredential reports whether any git source's URL holds a credential
// StoredSources takes out: a run that cannot be rebuilt whole from what is
// stored of it.
func CarriesCredential(sources []v1.Source) bool {
	for _, s := range sources {
		if s.Git != nil && withoutCredential(s.Git.URL) != s.Git.URL {
			return true
		}
	}
	return false
}

// localRemote is a repository on this machine, taken only inside a root.
func localRemote(raw, path string, reach reachFunc) (remote, error) {
	dir, err := reach("git.url", path)
	if err != nil {
		return remote{}, err
	}
	return remote{url: dir, key: "file://" + dir, name: repoName(dir)}, nil
}

// reachFunc is Manager.reach: the one check every source on this machine
// passes, a path source and a local git URL alike.
type reachFunc func(field, path string) (string, error)

// reach is inRoots behind the owner's path_sources. Switched off, the refusal
// names the setting and not the roots, which are not what refused it — an
// owner reading "outside the roots" would widen them and change nothing.
func (m *Manager) reach(field, path string) (string, error) {
	if m.PathSourcesOff {
		return "", fmt.Errorf("%s %q is a directory on this machine, and this runner's owner has switched sources on the machine off (path_sources = false under [workdirs] in config.toml) — send the repository as an https or ssh git URL, or its owner removes that line and restarts the runner", field, path)
	}
	return inRoots(field, path, m.Roots)
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
// (decision 0064, as DEV-91 for a hub's own URL). A URL loses its userinfo,
// query and fragment to config.RedactURL; an scp-like user@host:path loses its
// user the same way; anything else holding an @ is not printed at all. A
// source with nothing to take out — a path, most URLs — comes back as given.
func shownURL(raw string) string {
	if strings.Contains(raw, "://") {
		return config.RedactURL(raw)
	}
	// A path is a directory on the machine, which a run's error may name
	// (decision 0064); an @ in one is a directory's name, not a user.
	if strings.HasPrefix(raw, "/") || !strings.Contains(raw, "@") {
		return raw
	}
	hostPart, path, ok := strings.Cut(raw, ":")
	_, host, _ := strings.Cut(hostPart, "@")
	if !ok || strings.Contains(hostPart, "/") || strings.Contains(host, "@") || strings.Contains(path, "@") {
		return config.UnprintableURL
	}
	return "redacted@" + host + ":" + path
}

// redactSource is msg with every copy of the source URL raw in it printed as
// shownURL prints it. git's reason quotes the remote as it was given, and
// redactURLs finds only what looks like a URL to a pattern: not an scp-like
// user@host:path, and not a URL that holds a quote where the pattern stops.
// The source itself is known here, so it is replaced whole.
func redactSource(msg, raw string) string {
	if shown := shownURL(raw); shown != raw {
		return strings.ReplaceAll(msg, raw, shown)
	}
	return msg
}

// ShownSources is a copy of sources with every git URL as shownURL prints it,
// for a message that names a run's or a session's sources.
func ShownSources(sources []v1.Source) []v1.Source {
	out := make([]v1.Source, len(sources))
	for i, s := range sources {
		out[i] = s
		if s.Git != nil {
			g := *s.Git
			g.URL = shownURL(g.URL)
			out[i].Git = &g
		}
	}
	return out
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
