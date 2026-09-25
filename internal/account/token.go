package account

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// tokenFile is where a token account keeps its token, inside its own home
// (decision 0054). Claude reads no such file: yad hands the token to each of
// the account's runs as tokenVar, and to its login check, and to nothing else.
const tokenFile = "yad-oauth-token"

// tokenVar is the variable Claude Code takes a `claude setup-token` token
// from. supervise.Scrub removes it from the runner's own environment with
// every CLAUDE_CODE_ variable, so the only way it reaches a run is here.
const tokenVar = "CLAUDE_CODE_OAUTH_TOKEN"

// TokenLife is how long a `claude setup-token` token lasts, in Anthropic's
// words "one year". yad does not see the expiry itself; it counts from when
// the token was stored.
const TokenLife = 365 * 24 * time.Hour

// TokenWarnAt is how old a stored token is when the owner is told to make a
// new one: a month's notice before TokenLife.
const TokenWarnAt = TokenLife - 30*24*time.Hour

// CanUseToken says whether yad can run this harness on a stored token.
func CanUseToken(harness string) bool { return harness == "claude" }

// CheckToken says whether token, as read, is one yad will store: one word,
// surrounding whitespace aside. The CLI asks before it makes the account's
// home, so a bad paste leaves nothing behind.
func CheckToken(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("the token is empty — paste the one `claude setup-token` printed")
	}
	if strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		// A token is one word. Two lines pasted, or a prompt copied with it,
		// would reach Claude as a credential that fails every run.
		return errors.New("the token holds a space or a control character — paste only the token `claude setup-token` printed, on one line")
	}
	return nil
}

// SetToken stores a token as the account's credential, replacing any before
// it. The file is 0600, written beside and renamed over, so a run starting
// meanwhile reads the old token or the new one and never half of either.
func SetToken(home, token string) error {
	if err := CheckToken(token); err != nil {
		return err
	}
	token = strings.TrimSpace(token)
	tmp, err := os.CreateTemp(home, "."+tokenFile+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(token); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(home, tokenFile))
}

// ClearToken removes an account's stored token, so it logs in the harness's
// own way again: `yad account add` without --token on a token account. Left
// in place, the token would outrank the new login in every run and be
// refused in every run (decision 0054).
func ClearToken(home string) error {
	if home == "" {
		return nil
	}
	err := os.Remove(filepath.Join(home, tokenFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// SetTokenAside moves an account's token out of the way for a login in the
// harness's own way, which the token would otherwise outrank. restore puts it
// back — the login did not complete, and the account keeps the credential it
// had — and drop deletes it once the login has taken. Either is safe to call
// when there was no token.
func SetTokenAside(home string) (restore, drop func() error, err error) {
	from := filepath.Join(home, tokenFile)
	aside := filepath.Join(home, "."+tokenFile+".aside")
	none := func() error { return nil }
	if err := os.Rename(from, aside); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return none, none, nil
		}
		return nil, nil, err
	}
	restore = func() error { return os.Rename(aside, from) }
	drop = func() error {
		if err := os.Remove(aside); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return restore, drop, nil
}

// readToken is the account's token file read the one way every caller reads
// it: the token when it is usable, and otherwise why not — "" for no file at
// all. A file others can read is not used: it is a secret that must be
// assumed leaked, as config.ReadSecret has it.
func readToken(home string) (tok string, at time.Time, problem string, exists bool) {
	if home == "" {
		return "", time.Time{}, "", false
	}
	f := filepath.Join(home, tokenFile)
	info, err := os.Lstat(f)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", time.Time{}, "", false
	case err != nil:
		return "", time.Time{}, "its token file cannot be read", true
	case !info.Mode().IsRegular():
		return "", time.Time{}, "its token file is not a plain file", true
	case info.Mode().Perm()&0o077 != 0:
		return "", time.Time{}, "its token file can be read by others, so yad does not use it — the token must be taken as leaked: revoke it at claude.ai and store a new one", true
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return "", time.Time{}, "its token file cannot be read", true
	}
	tok = strings.TrimSpace(string(b))
	if CheckToken(tok) != nil {
		return "", time.Time{}, "its token file does not hold one token, so yad does not use it — store the token again", true
	}
	return tok, info.ModTime(), "", true
}

// token is the account's stored token, or "" when it has none yad will use.
func token(home string) string {
	tok, _, _, _ := readToken(home)
	return tok
}

// TokenFileExists says whether the account's home holds a token file at all,
// used or not.
func TokenFileExists(home string) bool {
	_, _, _, exists := readToken(home)
	return exists
}

// HasToken says whether the account runs on a stored token yad will use.
func HasToken(home string) bool { return token(home) != "" }

// TokenStored says when the account's token was stored, when it has one yad
// will use.
func TokenStored(home string) (time.Time, bool) {
	tok, at, _, _ := readToken(home)
	return at, tok != ""
}

// TokenProblem is why an account's token file is not used, or "" when it is
// used or there is none — so `yad account list` can say what is wrong instead
// of calling the token fine or the account logged out.
func TokenProblem(home string) string {
	_, _, problem, _ := readToken(home)
	return problem
}

// TokenID stands for the token a turn was handed, so a refusal that arrives
// after the owner stored a new one parks nothing (decision 0054). A hash,
// never the token; "" for none.
func TokenID(home string) string {
	return idOf(token(home))
}

func idOf(tok string) string {
	if tok == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:8])
}

// AddArgs is the yad command, after `yad`, that logs an account in again: with
// --token - for an account that runs on a stored token, which a plain login
// could not replace, and the plain login otherwise.
func AddArgs(harness, label, home string) []string {
	if HasToken(home) {
		return []string{"account", "add", harness, label, "--token", "-"}
	}
	return []string{"account", "add", harness, label}
}

// TokenWarning is what to tell the owner about an account's token, or "".
// Its words travel to hubs, so it names the account by label and never the
// home, and it carries no yad command: one that leaves the machine would have
// to name the profile, which only the CLI knows.
func TokenWarning(label, home string, now time.Time) string {
	at, ok := TokenStored(home)
	if !ok || now.Sub(at) < TokenWarnAt {
		return ""
	}
	left := int(at.Add(TokenLife).Sub(now).Hours() / 24)
	when := fmt.Sprintf("in about %d days", left)
	if left <= 0 {
		when = "about now, if it has not already"
	}
	return fmt.Sprintf("account %q runs on a `claude setup-token` token stored %s, which expires %s — make a new one with `claude setup-token` and store it in the account again with yad account add and --token", label, at.Format("2006-01-02"), when)
}
