package account

import (
	"errors"
	"fmt"
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

// SetToken stores a token as the account's credential, replacing any before
// it. The file is 0600, written beside and renamed over, so a run starting
// meanwhile reads the old token or the new one and never half of either.
func SetToken(home, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("the token is empty — paste the one `claude setup-token` printed")
	}
	if strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		// A token is one word. Two lines pasted, or a prompt copied with it,
		// would reach Claude as a credential that fails every run.
		return errors.New("the token holds a space or a control character — paste only the token `claude setup-token` printed, on one line")
	}
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

// token is the account's stored token, or "" when it logs in the harness's
// own way. A token file others can read is not used: it is a secret that must
// be assumed leaked, as config.ReadSecret has it.
func token(home string) string {
	if home == "" {
		return ""
	}
	f := filepath.Join(home, tokenFile)
	info, err := os.Stat(f)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		return ""
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// TokenStored says when the account's token was stored, and whether it has
// one at all.
func TokenStored(home string) (time.Time, bool) {
	if home == "" {
		return time.Time{}, false
	}
	info, err := os.Stat(filepath.Join(home, tokenFile))
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
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
