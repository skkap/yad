package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestScrubRewritesEveryFileAndTheLogGoesOn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	path := filepath.Join(dir, "yad.log")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const secret = "https://tok@host/r"
	files := map[string]string{
		// A backup past what this File keeps, and one past a gap.
		"yad.log": "a " + secret + "\n", "yad.log.1": "b " + secret + "\n", "yad.log.7": "c " + secret + "\n",
		// Not the log's: left as they are.
		"yad.log.bak": secret, "yad.logs": secret, "other.1": secret,
		// Nothing to take out: left untouched, not rewritten.
		"yad.log.2": "clean\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "yad.log.2"), old, old); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path, 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// Written through while the scrub runs: each line lands whole, before
	// or after the rewrite, and none is lost.
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 200 {
			fmt.Fprintf(l, "line %d\n", i)
		}
	})
	removed, err := l.Scrub("https://tok@", "https://")
	wg.Wait()
	if err != nil || len(removed) > 0 {
		t.Fatalf("Scrub = %v, %v", removed, err)
	}
	if _, err := l.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string]string{"yad.log.1": "b https://host/r\n", "yad.log.7": "c https://host/r\n", "yad.log.2": "clean\n",
		"yad.log.bak": secret, "yad.logs": secret, "other.1": secret} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", name, b, err, want)
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if lines[0] != "a https://host/r" {
		t.Errorf("the live file's line was not rewritten:\n%s", b)
	}
	if lines[len(lines)-1] != "after" || len(lines) != 202 {
		t.Errorf("the live file has %d lines ending %q; want the planted one, 200 written during the scrub and one after", len(lines), lines[len(lines)-1])
	}
	if strings.Contains(string(b), "tok@") {
		t.Errorf("the live file still holds the secret:\n%s", b)
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() != l.size {
		t.Errorf("the live file is %v bytes, the File counts %d (%v)", fi.Size(), l.size, err)
	}
	for _, name := range []string{"yad.log", "yad.log.1", "yad.log.7"} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, %v; want 0600", name, fi.Mode(), err)
		}
	}
	if fi, err := os.Stat(filepath.Join(dir, "yad.log.2")); err != nil || !fi.ModTime().Equal(old) {
		t.Errorf("a backup with nothing to take out was rewritten (%v)", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".*")); len(left) > 0 {
		t.Errorf("the rewrite left %v behind", left)
	}
}

// A credential whose start is another credential goes whole, whichever order
// the pairs come in.
func TestScrubLongestOldWins(t *testing.T) {
	for _, pairs := range [][]string{
		{"https://a@", "https://", "https://a@b@", "https://"},
		{"https://a@b@", "https://", "https://a@", "https://"},
	} {
		path := filepath.Join(t.TempDir(), "yad.log")
		if err := os.WriteFile(path, []byte("https://a@b@host https://a@host\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		l, err := Open(path, 1<<20, 3)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := l.Scrub(pairs...); err != nil {
			t.Fatal(err)
		}
		l.Close()
		if b, _ := os.ReadFile(path); string(b) != "https://host https://host\n" {
			t.Errorf("pairs %q left %q", pairs, b)
		}
	}
}

// A file that holds what must go and cannot be rewritten is removed; the
// live one is made again by the next write.
func TestScrubRemovesWhatItCannotRewrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "yad.log")
	for _, f := range []string{path, Backup(path, 1)} {
		if err := os.WriteFile(f, []byte("https://tok@host\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l, err := Open(path, 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, f := range []string{path, Backup(path, 1)} {
		if err := os.Chmod(f, 0); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := l.Scrub("https://tok@", "https://")
	if err != nil || len(removed) != 2 {
		t.Fatalf("Scrub = %v, %v; want both files removed", removed, err)
	}
	if _, err := os.Stat(Backup(path, 1)); !os.IsNotExist(err) {
		t.Errorf("the backup is still there: %v", err)
	}
	if _, err := l.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "after\n" {
		t.Errorf("live file = %q, %v; want the log going on in a new one", b, err)
	}
}
