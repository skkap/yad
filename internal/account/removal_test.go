package account

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/skkap/yad/internal/config"
)

// The marker is a private file at the top of the home, and nothing else: it
// says a removal is under way, and going again says the removal is off.
func TestMarkWritesAPrivateMarkerInTheHome(t *testing.T) {
	data := t.TempDir()
	home, err := Ensure(data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if Marked(data, "claude", "work") {
		t.Fatal("a new home reads as marked")
	}
	for range 2 {
		if err := Mark(data, "claude", "work"); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Lstat(filepath.Join(home, removingMarker))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 || !fi.Mode().IsRegular() {
		t.Errorf("the marker is %v, want a regular 0600 file", fi.Mode())
	}
	if !Marked(data, "claude", "work") {
		t.Error("a marked home does not read as marked")
	}
	for range 2 {
		if err := Unmark(data, "claude", "work"); err != nil {
			t.Fatal(err)
		}
	}
	if Marked(data, "claude", "work") {
		t.Error("the marker outlived Unmark")
	}

	// No home is nothing to mark, and marking makes none.
	if err := Mark(data, "claude", "never"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(HomeDir(data, "claude", "never")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marking an account with no home made one: %v", err)
	}
	if err := Mark(data, "claude", "../x"); err == nil {
		t.Error("a label that walks out of the accounts directory was marked")
	}
}

// Whatever already holds the marker's name — a symlink, a hard link to
// another file, a FIFO — is replaced by a new marker, and what it pointed at
// or shared is left as it was: the write lands in the home or nowhere, and
// never waits on a reader.
func TestMarkReplacesWhateverHoldsItsName(t *testing.T) {
	for _, c := range []struct {
		name  string
		plant func(t *testing.T, target, at string)
	}{
		{"symlink", func(t *testing.T, target, at string) {
			if err := os.Symlink(target, at); err != nil {
				t.Fatal(err)
			}
		}},
		{"hard link", func(t *testing.T, target, at string) {
			if err := os.Link(target, at); err != nil {
				t.Fatal(err)
			}
		}},
		{"fifo", func(t *testing.T, _, at string) {
			if err := syscall.Mkfifo(at, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := t.TempDir()
			home, err := Ensure(data, "claude", "work")
			if err != nil {
				t.Fatal(err)
			}
			// Beside the home, so a hard link can reach it on any filesystem.
			target := filepath.Join(filepath.Dir(home), "elsewhere")
			if err := os.WriteFile(target, []byte("the owner's"), 0o644); err != nil {
				t.Fatal(err)
			}
			c.plant(t, target, filepath.Join(home, removingMarker))
			done := make(chan error, 1)
			go func() { done <- Mark(data, "claude", "work") }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Mark is still waiting on what held the marker's name")
			}
			if b, err := os.ReadFile(target); err != nil || string(b) != "the owner's" {
				t.Errorf("the file the name led to changed: %q, %v", b, err)
			}
			if !Marked(data, "claude", "work") {
				t.Error("the home is not marked")
			}
		})
	}
}

// The marker stays out of what a home shares with every other account and
// with the owner's own harness — the transcripts, the shared configuration —
// and making the home again, as every run does, leaves it where it is: only
// an add takes it off.
func TestTheMarkerStaysInItsOwnHome(t *testing.T) {
	for _, harness := range []string{"claude", "codex"} {
		t.Run(harness, func(t *testing.T) {
			data := t.TempDir()
			own := t.TempDir()
			t.Setenv(HomeVar(harness), own)
			for _, name := range sharedConfig[harness] {
				if err := os.MkdirAll(filepath.Join(own, name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Ensure(data, harness, "work"); err != nil {
				t.Fatal(err)
			}
			if err := Mark(data, harness, "work"); err != nil {
				t.Fatal(err)
			}
			if _, err := Ensure(data, harness, "work"); err != nil {
				t.Fatal(err)
			}
			if !Marked(data, harness, "work") {
				t.Error("making the home again took the marker off")
			}
			for _, dir := range []string{TranscriptDir(data, harness), own} {
				err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
					if err == nil && d.Name() == removingMarker {
						t.Errorf("the marker reached %s", path)
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// Unfinished names a marked home, and never an unmarked one: under a label
// nothing lists, a home with no marker is an add in progress.
func TestUnfinishedFindsMarkedHomesOnly(t *testing.T) {
	data := t.TempDir()
	for _, label := range []string{"pending", "doomed"} {
		if _, err := Ensure(data, "claude", label); err != nil {
			t.Fatal(err)
		}
	}
	if err := Mark(data, "claude", "doomed"); err != nil {
		t.Fatal(err)
	}
	refs, err := Unfinished(data)
	if err != nil || !slices.Equal(refs, []Ref{{Harness: "claude", Label: "doomed"}}) {
		t.Errorf("Unfinished = %v, %v; want the marked home alone", refs, err)
	}
}

// Finishing a marked home deletes it and, on macOS, its Keychain login, the
// Keychain first; an unmarked home is not the sweep's, and the Keychain is not
// asked about it.
func TestFinishingDeletesAMarkedHomeWithItsKeychainLogin(t *testing.T) {
	t.Setenv("USER", "owner")
	calls := useFakeSecurity(t, "found")
	data := t.TempDir()
	home, err := Ensure(data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(data, "claude", "pending"); err != nil {
		t.Fatal(err)
	}
	if err := Mark(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	calls()
	if err := finish(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if want := deletes("owner", KeychainServices(home)...); !sameCalls(calls(), want) {
		t.Errorf("security was not run as %q", want)
	}
	if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the marked home is still there: %v", err)
	}
	assertNoSetAside(t, data, "claude", "work")

	if err := finish(data, "claude", "pending"); err != nil {
		t.Fatal(err)
	}
	if got := calls(); len(got) > 0 {
		t.Errorf("the Keychain was asked about an unmarked home: %q", got)
	}
	if _, err := os.Stat(HomeDir(data, "claude", "pending")); err != nil {
		t.Errorf("finishing touched an unmarked home: %v", err)
	}
}

// A Keychain that will not let go keeps the marked home, set aside, and the
// next finish completes it: a home and its login still go together.
func TestFinishingKeepsAMarkedHomeTheKeychainHoldsOnTo(t *testing.T) {
	t.Setenv("USER", "owner")
	useFakeSecurity(t, "missing")
	data := t.TempDir()
	if _, err := Ensure(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if err := Mark(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ACCOUNT_TEST_SECURITY", "locked")
	var kerr *KeychainError
	if err := finish(data, "claude", "work"); !errors.As(err, &kerr) {
		t.Fatalf("finish = %v, want a KeychainError", err)
	}
	if n := setAside(t, data, "claude", "work"); n != 1 {
		t.Fatalf("%d copies set aside, want the one kept", n)
	}
	if refs, _ := Unfinished(data); !slices.Contains(refs, Ref{Harness: "claude", Label: "work"}) {
		t.Error("the kept copy is not unfinished")
	}
	t.Setenv("ACCOUNT_TEST_SECURITY", "found")
	if err := finish(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	assertNoSetAside(t, data, "claude", "work")
}

// finish is what the daemon's start sweep does with an unlisted account
// nothing holds (runner.Accounts.finish), without the lock it takes.
func finish(data, harness, label string) error {
	if _, err := SetAsideMarked(data, harness, label); err != nil {
		return err
	}
	return RemoveSetAside(data, harness, label)
}

// Unlist marks the home before config.toml drops the label, under the file's
// lock: a marker that cannot be written leaves the label listed, so there is
// no moment at which the label is gone and the home unmarked.
func TestUnlistMarksBeforeTheLabelGoes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes in a directory it may not write")
	}
	ctx := context.Background()
	p := config.Paths{Profile: "test", Config: t.TempDir(), Data: t.TempDir()}
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"work"}}}
	if err := config.Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	home, err := Ensure(p.Data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	listed := func() []string {
		t.Helper()
		c, err := config.Load(p)
		if err != nil {
			t.Fatal(err)
		}
		return c.Harness["claude"].Accounts
	}
	always := func(bool) bool { return true }

	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	_, err = Unlist(ctx, p, "claude", "work", always)
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("Unlist reported no error with no marker written")
	}
	if got := listed(); !slices.Equal(got, []string{"work"}) {
		t.Errorf("config.toml lists %v after a marker that could not be written", got)
	}

	was, err := Unlist(ctx, p, "claude", "work", always)
	if err != nil || !was {
		t.Fatalf("Unlist = %v, %v", was, err)
	}
	if !Marked(p.Data, "claude", "work") || len(listed()) != 0 {
		t.Errorf("marked %v, listed %v; want the home marked and the label gone", Marked(p.Data, "claude", "work"), listed())
	}

	// A hub asking about a label nothing lists marks nothing: the home may
	// be an add in progress.
	if _, err := Ensure(p.Data, "claude", "pending"); err != nil {
		t.Fatal(err)
	}
	was, err = Unlist(ctx, p, "claude", "pending", func(listed bool) bool { return listed })
	if err != nil || was || Marked(p.Data, "claude", "pending") {
		t.Errorf("Unlist of an unlisted label: listed %v, %v, marked %v", was, err, Marked(p.Data, "claude", "pending"))
	}
}
