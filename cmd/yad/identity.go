package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// runnerID returns this machine's stable runner identity, creating it on first
// use.
//
// It is deliberately *not* the hostname. Hostnames are reassigned, duplicated
// across a fleet of cloned VMs, and changed by DHCP — and a control plane that
// keys sessions by runner cannot survive two machines claiming to be
// "ubuntu-01". The id is random, written once, and the file is the only place
// it lives; deleting it makes this machine a new runner, which is the correct
// way to retire one.
func runnerID() string {
	path := filepath.Join(configDir(), "runner-id")
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id
		}
	}

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing means the OS entropy source is gone; there is no
		// sensible fallback identity, and a runner without one must not
		// register.
		panic("yad: cannot generate a runner id: " + err.Error())
	}
	id := hex.EncodeToString(buf)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err == nil {
		// A write failure is survivable — the runner works, it just forgets who
		// it was on restart — so it is not worth refusing to start over.
		_ = os.WriteFile(path, []byte(id+"\n"), 0o600)
	}
	return id
}

// configDir is $YAD_CONFIG_DIR, else $XDG_CONFIG_HOME/yad, else ~/.config/yad —
// the same resolution on macOS and Linux, because a runner's config is read by
// scripts that do not care which one they are on.
func configDir() string {
	if d := os.Getenv("YAD_CONFIG_DIR"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "yad")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".yad"
	}
	return filepath.Join(home, ".config", "yad")
}
