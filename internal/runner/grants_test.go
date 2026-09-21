package runner

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testGrantName  = "AWS_SECRET_ACCESS_KEY"
	testGrantValue = "file-secret"
)

// secretLeft reports whether any file under root still holds value. A file
// the test cannot read is a failure, not a pass: unreadable to the test is not
// the same as gone.
func secretLeft(t *testing.T, root, value string) bool {
	t.Helper()
	found := false
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if !os.IsNotExist(err) {
				t.Errorf("walking %s: %v", p, err)
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("reading %s: %v", p, err)
			return nil
		}
		if strings.Contains(string(b), value) {
			found = true
		}
		return nil
	})
	return found
}

// unlockTree puts the owner's bits back on everything under root when the
// test ends, so t.TempDir can remove it whatever the test left. Registered
// before anything is locked, so it runs after every freeze is thawed.
func unlockTree(t *testing.T, root string) {
	t.Cleanup(func() {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil {
				if d.IsDir() {
					os.Chmod(p, 0o700)
				} else if d.Type().IsRegular() {
					os.Chmod(p, 0o600)
				}
			}
			return nil
		})
	})
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// A grant directory the plain removal cannot get through is still rid of its
// secret, and whatever cannot be dealt with is reported — the directory and
// counts, never a grant's name or value.
func TestDestroyGrants(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits these cases take away")
	}
	for _, tc := range []struct {
		name string
		// lock takes away what the case is about, given the run's grant
		// directory and the grant file in it.
		lock       func(t *testing.T, dir, file string)
		frozen     bool
		wantGone   bool
		wantSecret bool
		wantLog    string
	}{
		{
			name:     "a directory without its write bit",
			lock:     func(t *testing.T, dir, _ string) { chmod(t, dir, 0o500) },
			wantGone: true,
		},
		{
			name:     "a directory without its write bit, holding a read-only file",
			lock:     func(t *testing.T, dir, file string) { chmod(t, file, 0o400); chmod(t, dir, 0o500) },
			wantGone: true,
		},
		{
			name:     "a directory with no bits at all",
			lock:     func(t *testing.T, dir, _ string) { chmod(t, dir, 0o000) },
			wantGone: true,
		},
		{
			name:    "an immutable directory",
			frozen:  true,
			lock:    func(t *testing.T, dir, _ string) { freeze(t, dir) },
			wantLog: "their contents were destroyed and the empty files remain",
		},
		{
			name:       "an immutable directory holding a read-only file",
			frozen:     true,
			lock:       func(t *testing.T, dir, file string) { chmod(t, file, 0o400); freeze(t, dir) },
			wantSecret: true,
			wantLog:    "the secrets a hub sent are still on disk",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.frozen && !canFreeze {
				t.Skip("no immutable flag an unprivileged user may set on this OS")
			}
			root := filepath.Join(t.TempDir(), "grants")
			dir := filepath.Join(root, "hub", "a")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, testGrantName)
			if err := os.WriteFile(file, []byte(testGrantValue), 0o600); err != nil {
				t.Fatal(err)
			}
			unlockTree(t, root)
			tc.lock(t, dir, file)

			var logged strings.Builder
			gone := destroyGrants(dir, slog.New(slog.NewTextHandler(&logged, nil)))
			got := logged.String()

			if gone != tc.wantGone {
				t.Errorf("destroyGrants = %v, want %v", gone, tc.wantGone)
			}
			if _, err := os.Lstat(dir); (err == nil) == tc.wantGone {
				t.Errorf("directory present = %v, want %v", err == nil, !tc.wantGone)
			}
			if left := secretLeft(t, root, testGrantValue); left != tc.wantSecret {
				t.Errorf("secret left on disk = %v, want %v", left, tc.wantSecret)
			}
			if tc.wantLog == "" && got != "" {
				t.Errorf("a removal that succeeded logged:\n%s", got)
			}
			if tc.wantLog != "" && (!strings.Contains(got, tc.wantLog) || !strings.Contains(got, "dir="+dir)) {
				t.Errorf("the log does not say %q about %s:\n%s", tc.wantLog, dir, got)
			}
			if strings.Contains(got, testGrantName) || strings.Contains(got, testGrantValue) {
				t.Errorf("the log names a grant:\n%s", got)
			}
		})
	}
}

// linkKinds are the two ways a harness, running as the owner, can put an
// owner's file at a grant's path. Both are regular-looking to a walk that
// does not ask, and emptying through either would destroy the owner's file.
var linkKinds = []struct {
	name string
	link func(target, at string) error
}{
	{"a symbolic link", os.Symlink},
	{"a hard link", os.Link},
}

// Emptying the grants destroys the grant and never what a link reaches.
func TestEmptyingGrantsWritesThroughNoLink(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this case takes away")
	}
	for _, lk := range linkKinds {
		t.Run(lk.name, func(t *testing.T) {
			base := t.TempDir()
			outside := filepath.Join(base, "owners-file")
			if err := os.WriteFile(outside, []byte("keep me"), 0o600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(base, "grants", "hub", "a")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, testGrantName), []byte(testGrantValue), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := lk.link(outside, filepath.Join(dir, "LINKED")); err != nil {
				t.Fatal(err)
			}
			unlockTree(t, filepath.Join(base, "grants"))
			// Without the directory's write bit, so emptying is all that
			// can happen.
			chmod(t, dir, 0o500)

			err := emptyGrantFiles(dir)

			if b, rerr := os.ReadFile(outside); rerr != nil || string(b) != "keep me" {
				t.Errorf("the file a link reached is %q, %v", b, rerr)
			}
			if b, rerr := os.ReadFile(filepath.Join(dir, testGrantName)); rerr != nil || len(b) != 0 {
				t.Errorf("the grant file holds %q, %v", b, rerr)
			}
			if lk.name == "a hard link" && err == nil {
				t.Error("a hard link it refused to empty was not reported")
			}
		})
	}
}

// Emptying is the fallback for a tree that cannot be removed, never a step
// before removal: a missing write bit is got past by restoring it, so a link
// the harness left there is unlinked with the tree and what it reached is
// never written.
func TestDestroyGrantsRemovesALinkRatherThanEmptyingIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this case takes away")
	}
	for _, lk := range linkKinds {
		t.Run(lk.name, func(t *testing.T) {
			base := t.TempDir()
			outside := filepath.Join(base, "owners-file")
			if err := os.WriteFile(outside, []byte("keep me"), 0o600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(base, "grants", "hub", "a")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := lk.link(outside, filepath.Join(dir, testGrantName)); err != nil {
				t.Fatal(err)
			}
			unlockTree(t, filepath.Join(base, "grants"))
			chmod(t, dir, 0o500)

			if !destroyGrants(dir, slog.New(slog.DiscardHandler)) {
				t.Error("a tree the write bit could be restored on was not removed")
			}
			if b, err := os.ReadFile(outside); err != nil || string(b) != "keep me" {
				t.Errorf("the file a link reached is %q, %v", b, err)
			}
		})
	}
}
