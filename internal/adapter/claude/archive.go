package claude

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/skkap/yad/internal/adapter"
)

// Releases: a Claude conversation is files — <native>.jsonl and its companion
// directory <native>/ — under the project directory Claude names after the
// workdir, and a home that never wrote them resumes them by id (decision 0013,
// and 0080 for a copy in another home and another workdir, measured on
// 2.1.296).
func (Adapter) Releases() bool { return true }

var _ adapter.Releaser = Adapter{}

// projectsDir is the directory in a Claude home that holds its project
// directories; yad's shared transcripts directory is linked in its place.
const projectsDir = "projects"

// maxProjectName bounds a project directory's name: a file name is at most
// 255 bytes, and a workdir's path past that would leave nowhere to write.
const maxProjectName = 200

// projectName is the directory Claude keeps a workdir's conversations in:
// the path with every character but a letter or a digit made a dash, as
// 2.1.296 names it (/private/tmp/w becomes -private-tmp-w). Claude resumes a
// conversation by id from whichever project directory holds it, so a name
// that differed from Claude's own would cost nothing but tidiness; a path too
// long for one directory name is cut and given a hash, so it stays one.
func projectName(dir string) string {
	var b strings.Builder
	for _, r := range dir {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name := b.String()
	if len(name) <= maxProjectName {
		return name
	}
	sum := sha256.Sum256([]byte(dir))
	return name[:maxProjectName] + "-" + hex.EncodeToString(sum[:4])
}

// resolved is dir as Claude sees it from inside: its working directory is
// the resolved path (on macOS a temporary directory under /var is
// /private/var), and a workdir not made yet is resolved through its parent.
func resolved(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	if r, err := filepath.EvalSymlinks(filepath.Dir(dir)); err == nil {
		return filepath.Join(r, filepath.Base(dir))
	}
	return dir
}

// roots are the directories holding project directories that a conversation
// of c may be in: yad's shared transcripts, where accounts keep theirs, and
// the default home's. The first is where an import goes.
func roots(c adapter.Conversation) []string {
	var out []string
	if c.Transcripts != "" {
		out = append(out, c.Transcripts)
	}
	if c.DefaultHome != "" {
		out = append(out, filepath.Join(c.DefaultHome, projectsDir))
	}
	return out
}

// nativeID is what a session id must look like before it names a file:
// Claude's are UUIDs, and yad chooses them (newUUID).
var nativeID = regexp.MustCompile(`^[0-9a-fA-F-]{8,64}$`)

// find is the project directory holding the conversation, preferring the
// workdir's own: a conversation is in one place, and the workdir's is where
// Claude writes it.
func find(c adapter.Conversation) (string, error) {
	if !nativeID.MatchString(c.Native) {
		return "", fmt.Errorf("%q is not a Claude session id", c.Native)
	}
	file := c.Native + ".jsonl"
	own := projectName(resolved(c.Workdir))
	for _, root := range roots(c) {
		dir := filepath.Join(root, own)
		if regular(filepath.Join(dir, file)) {
			return dir, nil
		}
	}
	for _, root := range roots(c) {
		matches, err := filepath.Glob(filepath.Join(root, "*", file))
		if err != nil {
			return "", err
		}
		for _, m := range matches {
			if regular(m) {
				return filepath.Dir(m), nil
			}
		}
	}
	return "", adapter.ErrNoConversation
}

func regular(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode().IsRegular()
}

// Export lists <native>.jsonl and every regular file under <native>/: the
// transcript, and what Claude keeps beside it — a tool result too large for
// the transcript, a subagent's own. Nothing else in the home is the
// conversation's: file-history, tasks and session-env are Claude's working
// state on this machine, and a link is followed by nothing here.
func (Adapter) Export(c adapter.Conversation) (adapter.Exported, error) {
	dir, err := find(c)
	if err != nil {
		return adapter.Exported{}, err
	}
	out := adapter.Exported{Dir: dir}
	main := filepath.Join(dir, c.Native+".jsonl")
	fi, err := os.Lstat(main)
	if err != nil {
		return adapter.Exported{}, err
	}
	out.Files = append(out.Files, adapter.ConversationFile{Name: c.Native + ".jsonl", Path: main, Mode: fi.Mode().Perm()})
	companion := filepath.Join(dir, c.Native)
	err = filepath.WalkDir(companion, func(path string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && path == companion {
			return fs.SkipAll
		}
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out.Files = append(out.Files, adapter.ConversationFile{Name: filepath.ToSlash(rel), Path: path, Mode: info.Mode().Perm()})
		return nil
	})
	if err != nil {
		return adapter.Exported{}, err
	}
	return out, nil
}

// Import puts the conversation in the workdir's project directory under the
// first root, where the next run's home reads it, and points every path the
// transcript names under the exporting runner's directory at this one's: a
// tool result Claude saved beside the transcript is named by its absolute
// path, and is readable again only there (measured, decision 0080).
func (Adapter) Import(c adapter.Conversation, staged, from string) error {
	if !nativeID.MatchString(c.Native) {
		return fmt.Errorf("%q is not a Claude session id", c.Native)
	}
	rs := roots(c)
	if len(rs) == 0 {
		return errors.New("no Claude home to import the conversation into")
	}
	if _, err := find(c); err == nil {
		return adapter.ErrConversationExists
	} else if !errors.Is(err, adapter.ErrNoConversation) {
		return err
	}
	dir := filepath.Join(rs[0], projectName(resolved(c.Workdir)))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// The companion first and the transcript last: a transcript in place is
	// what makes the conversation one Claude resumes, so it never names a
	// file that is not there yet.
	if src := filepath.Join(staged, c.Native); dirExists(src) {
		if err := copyTree(src, filepath.Join(dir, c.Native)); err != nil {
			return err
		}
	}
	body, err := os.ReadFile(filepath.Join(staged, c.Native+".jsonl"))
	if err != nil {
		return err
	}
	if from != "" {
		body = repoint(body, from, dir, c.Native)
	}
	tmp := filepath.Join(dir, "."+c.Native+".jsonl.importing")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, c.Native+".jsonl")); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Forget deletes the conversation wherever this machine has it.
func (Adapter) Forget(c adapter.Conversation) error {
	if !nativeID.MatchString(c.Native) {
		return fmt.Errorf("%q is not a Claude session id", c.Native)
	}
	var errs []error
	for {
		dir, err := find(c)
		if errors.Is(err, adapter.ErrNoConversation) {
			break
		}
		if err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(dir, c.Native)); err != nil {
			errs = append(errs, err)
		}
		if err := os.Remove(filepath.Join(dir, c.Native+".jsonl")); err != nil {
			// Left where find would find it again.
			return errors.Join(append(errs, err)...)
		}
	}
	return errors.Join(errs...)
}

func dirExists(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.IsDir()
}

// copyTree copies a staged directory of regular files, 0600 and 0700 whatever
// they were: a conversation holds whole tool outputs, and the home it lands in
// is the owner's alone.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(to, 0o700)
		case d.Type().IsRegular():
			return copyFile(path, to)
		}
		return nil
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// repoint rewrites, line by line, every path the transcript names in the
// conversation's companion directory on the exporting runner to the same file
// here. Claude names one through the home a run used — an account's
// projects/, a link to yad's shared directory, or the default home's — so the
// path is recognised by its end, the project directory and the session, and
// replaced up to there whatever led to it. A line naming none is kept byte
// for byte. One that does is rewritten through JSON, and a thinking block in
// it is left alone: Claude signs its reasoning, and a signature over text
// that changed is one the API refuses.
func repoint(body []byte, from, to, native string) []byte {
	tail := "/" + filepath.Base(from) + "/" + native + "/"
	now := strings.TrimSuffix(to, string(filepath.Separator)) + "/" + native + "/"
	if from == to {
		return body
	}
	// A path is a run of anything but space, a quote or a backslash, which is
	// where a path in JSON text stops; the shortest one from its first slash
	// that ends in tail.
	pattern := regexp.MustCompile(`/[^\s"'\\]*?` + regexp.QuoteMeta(tail))
	var out bytes.Buffer
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(nil, 256<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.Contains(line, []byte(tail)) {
			line = repointLine(line, pattern, now)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	if sc.Err() != nil {
		// A line past the scanner's limit: the transcript is kept as it came,
		// and a tool result it names stays where it was.
		return body
	}
	return out.Bytes()
}

func repointLine(line []byte, pattern *regexp.Regexp, now string) []byte {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return line
	}
	v = walkStrings(v, func(s string) string { return pattern.ReplaceAllLiteralString(s, now) })
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return line
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// walkStrings applies f to every string in v, but inside a thinking block.
func walkStrings(v any, f func(string) string) any {
	switch t := v.(type) {
	case string:
		return f(t)
	case []any:
		for i := range t {
			t[i] = walkStrings(t[i], f)
		}
		return t
	case map[string]any:
		if kind, _ := t["type"].(string); kind == "thinking" || kind == "redacted_thinking" {
			return t
		}
		for k, e := range t {
			t[k] = walkStrings(e, f)
		}
		return t
	}
	return v
}
