package logfile

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "yad.log")
	l, err := Open(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	line := func(i int) string { return fmt.Sprintf("%039d\n", i) } // 40 bytes
	for i := range 10 {
		if _, err := l.Write([]byte(line(i))); err != nil {
			t.Fatal(err)
		}
	}
	// Two lines per file: 8,9 live, 6,7 in .1, 4,5 in .2, nothing older.
	for n, want := range []string{line(8) + line(9), line(6) + line(7), line(4) + line(5)} {
		b, err := os.ReadFile(Backup(path, n))
		if err != nil || string(b) != want {
			t.Errorf("file %d = %q, %v; want %q", n, b, err, want)
		}
	}
	if _, err := os.Stat(Backup(path, 3)); !os.IsNotExist(err) {
		t.Errorf("a third backup exists with backups=2: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v, %v; want 0600", fi.Mode(), err)
	}

	got, err := Tail(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{line(5), line(6), line(7), line(8), line(9)}
	for i := range want {
		want[i] = strings.TrimSuffix(want[i], "\n")
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("Tail 5 = %v", got)
	}
	if all, _ := Tail(path, 100); len(all) != 6 {
		t.Errorf("Tail 100 reached %d lines, want the 6 kept", len(all))
	}
}

// Reopening an existing log appends to it and counts what is there.
func TestReopenCountsExistingSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yad.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 90), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(path, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.Write([]byte("0123456789abc\n"))
	if b, _ := os.ReadFile(path); string(b) != "0123456789abc\n" {
		t.Errorf("live file %q: the write should have rotated the 90 bytes away", b)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestFollowAcrossRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "yad.log")
	l, err := Open(path, 30, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.Write([]byte("before follow\n"))

	var out syncBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Follow(ctx, path, &out) }()
	time.Sleep(2 * pollEvery) // Follow starts at the end

	// Four writes, each rotating, before Follow looks again: the file it
	// held is rotated out of the three kept, and the lines between it and
	// the live file are only in backups it never opened.
	for i := range 4 {
		fmt.Fprintf(l, "line %02d of the log\n", i) // 19 bytes: each one rotates
	}
	want := ""
	for i := range 4 {
		want += fmt.Sprintf("line %02d of the log\n", i)
	}
	deadline := time.Now().Add(5 * time.Second)
	for out.String() != want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if out.String() != want {
		t.Errorf("followed:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestRecentKeepsWarningsAndErrors(t *testing.T) {
	r := NewRecent(2)
	var buf bytes.Buffer
	log := slog.New(r.Handler(slog.NewJSONHandler(&buf, nil))).With("connection", "home")
	log.Info("synced")
	log.Warn("sync failed", "err", "connection refused")
	log.Error("connection stopped", "err", "unauthorized")
	log.Error("third")
	got := r.Records()
	if len(got) != 2 || got[0].Message != "connection stopped" || got[1].Message != "third" {
		t.Fatalf("records = %+v", got)
	}
	if got[0].Attrs != "connection=home err=unauthorized" {
		t.Errorf("attrs = %q", got[0].Attrs)
	}
	if strings.Count(buf.String(), "\n") != 4 {
		t.Errorf("the wrapped handler did not see every record:\n%s", buf.String())
	}
}

// A rotation that fails must not end the log. Renames refused: the lines go on
// into the one file. The file gone and its directory refusing a new one: the
// line is lost with an error, and the next write, once the directory is back,
// lands.
func TestRotationFailureKeepsLogging(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores the directory modes this test relies on")
	}
	dir := filepath.Join(t.TempDir(), "logs")
	path := filepath.Join(dir, "yad.log")
	l, err := Open(path, 20, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	defer os.Chmod(dir, 0o700)
	l.Write([]byte("0123456789abcdef\n")) // 17 bytes

	// Renames refused: the directory is read-only.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if _, err := fmt.Fprintf(l, "over the limit %d\n", i); err != nil {
			t.Fatalf("write %d with rotation refused: %v", i, err)
		}
	}
	if b, _ := os.ReadFile(path); strings.Count(string(b), "\n") != 4 {
		t.Errorf("with rotation refused, the live file holds:\n%s", b)
	}

	// The live file gone and no new one allowed: the reopen fails.
	os.Chmod(dir, 0o700)
	os.Remove(path)
	os.Chmod(dir, 0o500)
	l.mu.Lock()
	l.retryAt = 0
	l.mu.Unlock()
	if _, err := l.Write([]byte("lost, and said so\n")); err == nil {
		t.Error("a write with no file to go to reported success")
	}
	l.mu.Lock()
	if l.f != nil {
		t.Error("the file points at something after a failed reopen")
	}
	l.mu.Unlock()

	os.Chmod(dir, 0o700)
	if _, err := l.Write([]byte("back\n")); err != nil {
		t.Fatalf("the log did not recover once the directory was writable: %v", err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "back\n") {
		t.Errorf("after recovery the live file holds %q", b)
	}
	l.Close()
	if _, err := l.Write([]byte("after close\n")); err == nil {
		t.Error("a write after Close succeeded")
	}
}

func TestRecentBoundsEachRecord(t *testing.T) {
	r := NewRecent(1)
	log := slog.New(r.Handler(slog.DiscardHandler))
	huge := strings.Repeat("é", 1<<20)
	log.Error(huge, "err", huge)
	got := r.Records()[0]
	for name, s := range map[string]string{"message": got.Message, "attrs": got.Attrs} {
		if len(s) > maxRecordText+64 || !strings.HasSuffix(s, "(truncated; the log has it all)") || !utf8.ValidString(s) {
			t.Errorf("%s kept %d bytes, valid UTF-8 %v", name, len(s), utf8.ValidString(s))
		}
	}
}
