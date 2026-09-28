package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// config.toml has more than one writer: `yad connect` and `yad account` at the
// terminal, and the daemon for an account a hub adds or removes (decision
// 0057). Each would otherwise read the file, change it and write it back over
// whatever another wrote in between — and the daemon's copy is from when it
// started, so writing that back would also undo every hand edit since. So every
// write is a read-modify-write under one lock: the file read fresh, one thing
// changed, and written back before the lock is let go.

// updateLockWait bounds the wait for another writer. Each holds the lock for
// one read and one write of a small file; one held this long is wedged, and
// the caller is better told than left hanging — the daemon's sync loop among
// them.
const updateLockWait = 10 * time.Second

// lockFile is beside config.toml rather than config.toml itself: Save
// replaces the file by rename, and a lock on the file replaced would be a lock
// on nothing the next writer opens.
func (p Paths) lockFile() string { return filepath.Join(p.Config, "config.toml.lock") }

// Update reads config.toml under its lock, lets change edit it, and saves it
// if change says it changed anything. It returns the config as it now stands,
// saved or not.
func Update(ctx context.Context, p Paths, change func(*Config) (bool, error)) (Config, error) {
	if p.Config == "" {
		// A zero Paths would lock and write in the working directory.
		return Config{}, errors.New("no config directory to write config.toml in")
	}
	unlock, err := lockConfig(ctx, p)
	if err != nil {
		return Config{}, err
	}
	defer unlock()
	c, err := Load(p)
	if err != nil {
		return c, err
	}
	changed, err := change(&c)
	if err != nil || !changed {
		return c, err
	}
	return c, Save(p, c)
}

// UpdateAccounts is Update for one harness's account list, the one part of
// config.toml a hub may change. change gets the list as the file has it now,
// and returns the list to write.
func UpdateAccounts(ctx context.Context, p Paths, harness string, change func([]string) []string) (Config, error) {
	return Update(ctx, p, func(c *Config) (bool, error) {
		h := c.Harness[harness]
		next := change(slices.Clone(h.Accounts))
		if slices.Equal(next, h.Accounts) {
			return false, nil
		}
		h.Accounts = next
		if c.Harness == nil {
			c.Harness = map[string]HarnessConfig{}
		}
		c.Harness[harness] = h
		return true, nil
	})
}

// WithAccount is a change for UpdateAccounts that lists label, last, unless
// it is listed already: the order breaks ties, and a new account starts last
// (decision 0039).
func WithAccount(label string) func([]string) []string {
	return func(labels []string) []string {
		if slices.Contains(labels, label) {
			return labels
		}
		return append(labels, label)
	}
}

// WithoutAccount is a change for UpdateAccounts that takes label off.
func WithoutAccount(label string) func([]string) []string {
	return func(labels []string) []string {
		return slices.DeleteFunc(labels, func(l string) bool { return l == label })
	}
}

func lockConfig(ctx context.Context, p Paths) (func(), error) {
	if err := os.MkdirAll(p.Config, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p.lockFile(), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, updateLockWait)
	defer cancel()
	// Polled, because flock has no wait a context can end.
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			break
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("%s is being written by another yad command, and was still locked after %s — run this again once it has finished", p.ConfigFile(), updateLockWait)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
