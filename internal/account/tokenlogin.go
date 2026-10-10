package account

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// credentialsFile is where Claude looks for a home's login when its secure
// storage has none: on Linux it is the store, and on macOS Claude reads it
// when the Keychain holds no item for the home (decision 0074, read from
// claude 2.1.296).
const credentialsFile = ".credentials.json"

// credentialsKey is the entry in credentialsFile that holds a claude.ai
// login. Claude keeps other things beside it — an MCP server's OAuth, a
// trusted device — which yad never touches.
const credentialsKey = "claudeAiOauth"

// markerKey marks a credentials file whose login yad wrote, with a sentence
// for whoever opens it. Claude keeps entries it does not know when it writes
// the file, so the marker can outlive the login it marked — a login Claude
// made over it, which has a refresh token. Only a marked file whose login has
// none is yad's: anything else is a login yad never wrote, and is not
// touched.
const markerKey = "yadTokenLogin"

const tokenMarkerText = "written by yad from this account's yad-oauth-token, for the claudes its runs start; yad replaces it when the token changes and removes it with the token"

// maxCredentials bounds what is read to decide whose the file is. Claude's
// own is a few kilobytes; one larger than this is not one yad wrote, and is
// left alone rather than read whole.
const maxCredentials = 1 << 20

// tokenLogin is a token account's token as Claude holds it in memory when it
// is handed CLAUDE_CODE_OAUTH_TOKEN: no refresh token, no expiry, inference
// only. Written in the home, it is the same login for a claude the run
// starts, which Claude Code never hands the variable to (DEV-172).
type tokenLogin struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     *string  `json:"refreshToken"`
	ExpiresAt        *int64   `json:"expiresAt"`
	Scopes           []string `json:"scopes"`
	SubscriptionType *string  `json:"subscriptionType"`
	RateLimitTier    *string  `json:"rateLimitTier"`
}

func loginFor(tok string) tokenLogin {
	return tokenLogin{AccessToken: tok, Scopes: []string{"user:inference"}}
}

// credentials is the home's credentialsFile as yad sees it.
type credentials struct {
	// doc is the file's entries, kept as written so that a rewrite changes
	// only credentialsKey. Nil when there is no file.
	doc map[string]json.RawMessage
	// ours says the login in it is the one yad wrote: marked, and with no
	// refresh token, which Claude's own login always has — it does not save
	// an inference-only login at all. Only such a login is yad's to replace
	// or remove.
	ours bool
	// accessToken is the token in a login that is ours.
	accessToken string
	// theirs says the file is not yad's to touch: a login Claude made, or
	// anything yad cannot read as Claude's format.
	theirs bool
	// shared says others can read the file: one of ours that drifted from
	// 0600 is written again.
	shared bool
}

func readCredentials(home string) (credentials, error) {
	f := filepath.Join(home, credentialsFile)
	info, err := os.Lstat(f)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return credentials{}, nil
	case err != nil:
		return credentials{}, err
	case !info.Mode().IsRegular(), info.Size() > maxCredentials:
		return credentials{theirs: true}, nil
	}
	r, err := os.OpenFile(f, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return credentials{}, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, maxCredentials+1))
	if err != nil {
		return credentials{}, err
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(b, &doc) != nil || doc == nil {
		return credentials{theirs: true}, nil
	}
	raw, ok := doc[credentialsKey]
	if !ok || string(raw) == "null" {
		return credentials{doc: doc}, nil
	}
	if _, marked := doc[markerKey]; !marked {
		return credentials{theirs: true}, nil
	}
	var login map[string]json.RawMessage
	if json.Unmarshal(raw, &login) != nil || login == nil {
		return credentials{theirs: true}, nil
	}
	if rt, ok := login["refreshToken"]; ok && string(rt) != "null" {
		return credentials{theirs: true}, nil
	}
	var tok string
	if json.Unmarshal(login["accessToken"], &tok) != nil {
		return credentials{theirs: true}, nil
	}
	return credentials{doc: doc, ours: true, accessToken: tok, shared: info.Mode().Perm()&0o077 != 0}, nil
}

// syncTokenLogin makes the home's credentials file hold the account's token
// as its login, or no token login when the account has no token yad will
// use — a token file others can read included, which must be assumed leaked
// and so reaches nothing. Run wherever the token changes and every time the
// home is prepared, so an account stored before this existed gets the file
// at its next run.
//
// A login yad did not write is never touched: an account logged in its own
// way before it got a token keeps that login, which a nested claude can use
// as well as the token. The token is never in an error.
func syncTokenLogin(home string) error {
	if home == "" {
		return nil
	}
	c, err := readCredentials(home)
	if err != nil {
		return fmt.Errorf("%s could not be read: %w", filepath.Join(home, credentialsFile), err)
	}
	if c.theirs {
		return nil
	}
	tok := token(home)
	switch {
	case tok == "" && !c.ours:
		return nil
	case tok == "":
		return dropLogin(home, c)
	case c.ours && c.accessToken == tok && !c.shared:
		return nil
	}
	login, err := json.Marshal(loginFor(tok))
	if err != nil {
		return err
	}
	marker, err := json.Marshal(tokenMarkerText)
	if err != nil {
		return err
	}
	if c.doc == nil {
		c.doc = map[string]json.RawMessage{}
	}
	c.doc[credentialsKey] = login
	c.doc[markerKey] = marker
	return writeCredentials(home, c.doc)
}

// forgetTokenCopy deletes the Keychain items Claude keeps for a token
// account's home, on macOS, saying what was not done when one will not go.
// Claude copies the credentials file there, token and all, the first time it
// saves anything else in its store, and the copy outranks the file from then
// on (decision 0074).
func forgetTokenCopy(home, so string) error {
	_, err := forgetKeychain("claude", home, false)
	if ke, ok := errors.AsType[*KeychainError](err); ok {
		ke.So = so
	}
	return err
}

// BeforeLoginBesideToken readies a token account's home for a login of
// Claude's own (`yad account add` without --token, a hub's link login): the
// token's copies are taken out of the home — its login in the credentials
// file, and on macOS whatever Claude copied of it into the Keychain — so the
// check after the login (OwnLogin) can only find a login the login made. The
// token itself stays stored, for the runs still on the account, until that
// check says yes; a login walked away from gets its copy back at the next
// run. Nothing is done for a home with no token file.
func BeforeLoginBesideToken(harness, home string) error {
	if !CanUseToken(harness) || !TokenFileExists(home) {
		return nil
	}
	if err := forgetTokenCopy(home, "the login does not start: its check would find the token's copy there and call the login made"); err != nil {
		return err
	}
	return dropTokenLogin(home)
}

// dropTokenLogin takes a token's login out of the home's credentials file,
// leaving the token itself stored: for a check of whether a login made
// beside the token took, which claude would otherwise answer with the token.
// The next preparation of the home puts it back while the token is stored.
func dropTokenLogin(home string) error {
	if home == "" {
		return nil
	}
	c, err := readCredentials(home)
	if err != nil {
		return fmt.Errorf("%s could not be read: %w", filepath.Join(home, credentialsFile), err)
	}
	if !c.ours {
		return nil
	}
	return dropLogin(home, c)
}

// dropLogin removes the login from a credentials file that is ours, and the
// file when nothing else is in it.
func dropLogin(home string, c credentials) error {
	delete(c.doc, credentialsKey)
	delete(c.doc, markerKey)
	if len(c.doc) == 0 {
		err := os.Remove(filepath.Join(home, credentialsFile))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return writeCredentials(home, c.doc)
}

// writeCredentials writes the file 0600, beside and renamed over, as SetToken
// writes the token: a claude starting meanwhile reads the old login or the
// new one, never half of either.
func writeCredentials(home string, doc map[string]json.RawMessage) error {
	b, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(home, "."+credentialsFile+"-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(home, credentialsFile))
}
