package runner

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/adapter"
)

// An archive packs its manifest and the conversation's files and unpacks to
// the same, 0600; its sha256 is of the bytes on disk.
func TestAnArchiveRoundTrips(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "n", "tool-results"), 0o755)
	os.WriteFile(filepath.Join(src, "n.jsonl"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(src, "n", "tool-results", "x.txt"), []byte("out"), 0o644)
	m := manifest{Format: archiveFormat, Harness: "claude", HarnessVersion: "2.1.296", NativeID: "n", Dir: src,
		Sources: []carriedSource{{URL: "https://example.com/r.git", Branch: "yad/hub/s1"}}, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	file := filepath.Join(t.TempDir(), "a", "s.tar.gz")
	sha, size, err := packArchive(file, m, []adapter.ConversationFile{
		{Name: "n.jsonl", Path: filepath.Join(src, "n.jsonl")},
		{Name: "n/tool-results/x.txt", Path: filepath.Join(src, "n", "tool-results", "x.txt")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, n, err := fileSHA256(file); err != nil || got != sha || n != size {
		t.Fatalf("sha %s/%d, file %s/%d (%v)", sha, size, got, n, err)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o600 {
		t.Errorf("the archive is %v", fi.Mode().Perm())
	}
	out := filepath.Join(t.TempDir(), "x")
	got, err := unpackArchive(file, out)
	if err != nil {
		t.Fatal(err)
	}
	if got.NativeID != "n" || got.HarnessVersion != "2.1.296" || len(got.Sources) != 1 || got.Sources[0].Branch != "yad/hub/s1" {
		t.Errorf("manifest %+v", got)
	}
	b, err := os.ReadFile(filepath.Join(out, "n", "tool-results", "x.txt"))
	if fi, _ := os.Stat(filepath.Join(out, "n", "tool-results", "x.txt")); err != nil || string(b) != "out" || fi.Mode().Perm() != 0o600 {
		t.Errorf("unpacked %q, %v", b, err)
	}
	if _, _, err := packArchive(file, m, []adapter.ConversationFile{{Name: "../escape", Path: filepath.Join(src, "n.jsonl")}}); err == nil {
		t.Error("a file named out of the conversation was packed")
	}
}

// What a hub hands back is checked before anything is written outside the
// directory it unpacks to: a path climbing out, an absolute one, a link, an
// archive with no manifest or of a format this runner does not read.
func TestAnArchiveThatWouldEscapeIsRefused(t *testing.T) {
	type entry struct {
		name     string
		typeflag byte
		body     string
		link     string
	}
	manifestOK := entry{name: "manifest.json", typeflag: tar.TypeReg, body: `{"format":1,"harness":"claude","native_id":"n"}`}
	for _, tc := range []struct {
		name    string
		entries []entry
		want    string
	}{
		{"climbing out", []entry{manifestOK, {name: "conversation/../../evil", typeflag: tar.TypeReg, body: "x"}}, "outside its conversation"},
		{"absolute", []entry{manifestOK, {name: "/etc/evil", typeflag: tar.TypeReg, body: "x"}}, "outside its conversation"},
		{"outside conversation/", []entry{manifestOK, {name: "evil", typeflag: tar.TypeReg, body: "x"}}, "outside its conversation"},
		{"a link", []entry{manifestOK, {name: "conversation/l", typeflag: tar.TypeSymlink, link: "/etc/passwd"}}, "not a plain file"},
		{"no manifest", []entry{{name: "conversation/n.jsonl", typeflag: tar.TypeReg, body: "{}"}}, "no manifest"},
		{"another format", []entry{{name: "manifest.json", typeflag: tar.TypeReg, body: `{"format":2}`}}, "format 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "a.tar.gz")
			f, _ := os.Create(file)
			gz := gzip.NewWriter(f)
			tw := tar.NewWriter(gz)
			for _, e := range tc.entries {
				if err := tw.WriteHeader(&tar.Header{Name: e.name, Typeflag: e.typeflag, Size: int64(len(e.body)), Linkname: e.link, Mode: 0o644}); err != nil {
					t.Fatal(err)
				}
				tw.Write([]byte(e.body))
			}
			tw.Close()
			gz.Close()
			f.Close()
			parent := t.TempDir()
			_, err := unpackArchive(file, filepath.Join(parent, "x"))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one saying %q", err, tc.want)
			}
			if tc.name != "no manifest" && tc.name != "another format" && !strings.Contains(err.Error(), "archive") {
				t.Errorf("err %v is not an archive refusal", err)
			}
			if _, err := os.Stat(filepath.Join(parent, "evil")); err == nil {
				t.Error("a file was written outside the directory")
			}
		})
	}
}
