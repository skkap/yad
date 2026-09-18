package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Credential files hold one runner credential per connection. They are the only
// secrets a runner keeps; registration tokens are never written anywhere.

func (p Paths) credentialPath(connection string) string {
	return filepath.Join(p.Config, "credentials", connection)
}

// SaveCredential writes a connection's runner credential at 0600.
func (p Paths) SaveCredential(connection, credential string) error {
	if err := validName(connection); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(p.Config, "credentials"), 0o700); err != nil {
		return err
	}
	return writePrivate(p.credentialPath(connection), []byte(credential+"\n"))
}

// Credential reads a connection's runner credential.
func (p Paths) Credential(connection string) (string, error) {
	c, err := ReadSecret(p.credentialPath(connection))
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("no credential for connection %q — run `yad connect` again", connection)
	}
	if errors.Is(err, errExposed) {
		return "", fmt.Errorf("credential for %q %w — revoke it at the hub, then `yad connect` again", connection, err)
	}
	return c, err
}

// CheckCredential is the local half of proving a connection can authenticate:
// the credential is there, 0600, readable, and shaped like something an
// Authorization header can carry. It is what `yad daemon restart` checks
// before it stops a working daemon, so a broken credential leaves the old
// process running rather than trading it for one that cannot sync.
//
// It is not proof the hub still accepts the credential. That needs a protocol
// call, and the only authenticated one is a sync — which, from a second
// process, would renew or drop the running daemon's leases and could be
// handed offers it then never lists. A real probe needs a call of its own.
func (p Paths) CheckCredential(connection string) error {
	c, err := p.Credential(connection)
	if err != nil {
		return err
	}
	if c == "" {
		return fmt.Errorf("the credential for %q is empty — run `yad connect` again", connection)
	}
	for _, r := range c {
		// Visible ASCII is what a bearer token may hold; anything else is a
		// file that was edited or truncated, and every sync would fail on it.
		if r <= ' ' || r > '~' {
			return fmt.Errorf("the credential for %q is not a single token (it holds whitespace or a non-ASCII character) — run `yad connect` again", connection)
		}
	}
	return nil
}

// HubAdminToken is where `yad hub submit` and `watch` find the admin token
// `yad hub admin-token create` saved for this profile.
func (p Paths) HubAdminToken() string { return filepath.Join(p.Config, "hub-admin-token") }

var errExposed = errors.New("is readable by others")

// ReadSecret reads a one-line secret from a file. A file readable by anyone
// but the owner is refused rather than used, because a token that has been
// world-readable must be assumed leaked. A missing file is fs.ErrNotExist.
func ReadSecret(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%w (%v)", errExposed, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// WriteSecret writes a one-line secret at 0600, creating its directory
// private to the owner.
func WriteSecret(path, secret string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writePrivate(path, []byte(secret+"\n"))
}

// DeleteCredential forgets a connection's credential. Missing is not an error.
func (p Paths) DeleteCredential(connection string) error {
	err := os.Remove(p.credentialPath(connection))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// writePrivate writes atomically at 0600: a crash mid-write must never leave a
// truncated credential or id that parses as a different one.
func writePrivate(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
