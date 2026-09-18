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

// Credential reads a connection's runner credential. A file readable by anyone
// but the owner is refused rather than used, because a token that has been
// world-readable must be assumed leaked.
func (p Paths) Credential(connection string) (string, error) {
	path := p.credentialPath(connection)
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("no credential for connection %q — run `yad connect` again", connection)
	}
	if err != nil {
		return "", err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("credential for %q is readable by others (%v) — revoke it at the hub, then `yad connect` again", connection, fi.Mode().Perm())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
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
