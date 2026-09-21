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

// A harness runs as the owner and can put a link where a grant file was.
// Emptying the grants must destroy the grant and never what a link points at
// — an owner's file would be the casualty.
func TestEmptyingGrantsFollowsNoLink(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this case takes away")
	}
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
	if err := os.Symlink(outside, filepath.Join(dir, "LINKED")); err != nil {
		t.Fatal(err)
	}
	unlockTree(t, filepath.Join(base, "grants"))
	// Without the directory's write bit, so emptying is all that can happen.
	chmod(t, dir, 0o500)

	emptyGrantFiles(dir)

	if b, err := os.ReadFile(outside); err != nil || string(b) != "keep me" {
		t.Errorf("the file a link pointed at is %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, testGrantName)); err != nil || len(b) != 0 {
		t.Errorf("the grant file holds %q, %v", b, err)
	}
}
