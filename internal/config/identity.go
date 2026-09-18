package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RunnerID returns this profile's stable runner identity, creating it on first
// use.
//
// It is deliberately not the hostname: hostnames are reassigned, duplicated
// across cloned VMs and changed by DHCP, and a hub that keys sessions by runner
// cannot survive two machines claiming to be "ubuntu-01". The id is random,
// written once, and the file is the only place it lives; deleting it makes this
// profile a new runner, which is the supported way to retire one.
func (p Paths) RunnerID() (string, error) {
	path := filepath.Join(p.Config, "runner-id")
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	case !errors.Is(err, fs.ErrNotExist):
		// Unreadable is not absent: replacing it would give this profile a new
		// identity and orphan every session the old one held.
		return "", fmt.Errorf("read %s: %w — fix its ownership or permissions; deleting it retires this runner", path, err)
	}
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("cannot generate a runner id — the OS entropy source failed: %w", err)
	}
	id := hex.EncodeToString(buf)
	if err := os.MkdirAll(p.Config, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", p.Config, err)
	}
	// Refuse to start rather than run unsaved: a runner that forgets its id on
	// restart registers as a new runner and orphans every session it held.
	if err := writePrivate(path, []byte(id+"\n")); err != nil {
		return "", err
	}
	return id, nil
}
