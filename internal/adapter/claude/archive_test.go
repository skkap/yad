package claude

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/adapter"
)

const testNative = "61df8b38-c652-4ed6-9575-90d8a16e3daa"

// The project directory Claude names after a workdir, as 2.1.296 named it in
// the measurement of decision 0080: every character but a letter or a digit
// a dash, and a path too long for one name cut and hashed.
func TestProjectName(t *testing.T) {
	if got, want := projectName("/private/tmp/claude-501/-Users-x/phase0/runnerA/workdirs/conn/sess-1"),
		"-private-tmp-claude-501--Users-x-phase0-runnerA-workdirs-conn-sess-1"; got != want {
		t.Errorf("projectName = %q, want %q", got, want)
	}
	if got := projectName("/a/b.c_d é"); got != "-a-b-c-d--" {
		t.Errorf("projectName of dots, underscores and a non-ASCII letter = %q", got)
	}
	long := "/" + strings.Repeat("x", 300)
	if got := projectName(long); len(got) != maxProjectName+9 || got == projectName(long+"y") {
		t.Errorf("a long path's name %q (%d bytes) is not cut and hashed apart", got, len(got))
	}
}

// writeConversation writes a conversation as Claude does: the transcript and
// a tool result saved beside it, named by its absolute path through home.
func writeConversation(t *testing.T, root, workdir, seenAs string) string {
	t.Helper()
	dir := filepath.Join(root, projectName(resolved(workdir)))
	if err := os.MkdirAll(filepath.Join(dir, testNative, "tool-results"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, testNative, "tool-results", "b1.txt"), []byte("1\n2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(seenAs, projectName(resolved(workdir)), testNative, "tool-results", "b1.txt")
	lines := []string{
		`{"type":"user","message":{"role":"user","content":"remember PERIWINKLE"}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"Output too large. Full output saved to: ` + saved + `"}]},"toolUseResult":{"stdout":"saved to ` + saved + ` ok"}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"it is in ` + saved + `","signature":"c2ln"},{"type":"text","text":"read ` + saved + `"}]}}`,
	}
	if err := os.WriteFile(filepath.Join(dir, testNative+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return saved
}

// A conversation goes from one runner's home to another's, under another
// workdir: Export lists the transcript and its companion, Import puts them in
// the new workdir's project directory, and the path of a tool result Claude
// saved now names the file where it is — but inside its signed reasoning,
// which is left as it was.
func TestAConversationMovesBetweenHomes(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	aShared, aHome := filepath.Join(a, "transcripts"), filepath.Join(a, "accounts", "work")
	wa := filepath.Join(a, "workdirs", "hub", "s1")
	// Claude names the file through the account home's projects/ link.
	old := writeConversation(t, aShared, wa, filepath.Join(aHome, "projects"))

	from := adapter.Conversation{Native: testNative, Workdir: wa, Transcripts: aShared, DefaultHome: filepath.Join(a, "default")}
	exp, err := Adapter{}.Export(from)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range exp.Files {
		names = append(names, f.Name)
	}
	if strings.Join(names, " ") != testNative+".jsonl "+testNative+"/tool-results/b1.txt" {
		t.Fatalf("exported %v", names)
	}

	// The runner stages the files by their names, as unpacking does.
	staged := t.TempDir()
	for _, f := range exp.Files {
		body, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		to := filepath.Join(staged, filepath.FromSlash(f.Name))
		os.MkdirAll(filepath.Dir(to), 0o700)
		os.WriteFile(to, body, 0o600)
	}
	bShared := filepath.Join(b, "transcripts")
	wb := filepath.Join(b, "workdirs", "other", "moved")
	if err := os.MkdirAll(wb, 0o700); err != nil {
		t.Fatal(err)
	}
	to := adapter.Conversation{Native: testNative, Workdir: wb, Transcripts: bShared, DefaultHome: filepath.Join(b, "default")}
	if err := (Adapter{}).Import(to, staged, exp.Dir); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(bShared, projectName(resolved(wb)))
	body, err := os.ReadFile(filepath.Join(dir, testNative+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	now := filepath.Join(dir, testNative, "tool-results", "b1.txt")
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if lines[0] != `{"type":"user","message":{"role":"user","content":"remember PERIWINKLE"}}` {
		t.Errorf("a line naming no path changed: %s", lines[0])
	}
	if !strings.Contains(lines[1], now) || strings.Contains(lines[1], old) || !strings.Contains(lines[1], "saved to "+now+" ok") {
		t.Errorf("the tool result still names %s:\n%s", old, lines[1])
	}
	var asst struct {
		Message struct {
			Content []map[string]string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &asst); err != nil {
		t.Fatal(err)
	}
	if asst.Message.Content[0]["thinking"] != "it is in "+old || asst.Message.Content[0]["signature"] != "c2ln" {
		t.Errorf("the signed thinking changed: %v", asst.Message.Content[0])
	}
	if asst.Message.Content[1]["text"] != "read "+now {
		t.Errorf("the text names %q", asst.Message.Content[1]["text"])
	}
	if b, err := os.ReadFile(now); err != nil || string(b) != "1\n2\n" {
		t.Errorf("the tool result at its new path: %q, %v", b, err)
	}

	// The same conversation again is refused, and left as it was.
	if err := (Adapter{}).Import(to, staged, exp.Dir); !errors.Is(err, adapter.ErrConversationExists) {
		t.Errorf("a second import: %v", err)
	}
	// Forget takes it away from the home that released it.
	if err := (Adapter{}).Forget(from); err != nil {
		t.Fatal(err)
	}
	if _, err := (Adapter{}).Export(from); !errors.Is(err, adapter.ErrNoConversation) {
		t.Errorf("after Forget, Export says %v", err)
	}
}

// A conversation in the default home is found as well: a session that ran
// before its runner had accounts kept it there.
func TestExportFindsTheDefaultHome(t *testing.T) {
	root := t.TempDir()
	def := filepath.Join(root, "claude-home")
	w := filepath.Join(root, "w")
	writeConversation(t, filepath.Join(def, projectsDir), w, filepath.Join(def, projectsDir))
	exp, err := Adapter{}.Export(adapter.Conversation{Native: testNative, Workdir: filepath.Join(root, "elsewhere"), Transcripts: filepath.Join(root, "none"), DefaultHome: def})
	if err != nil || len(exp.Files) != 2 {
		t.Fatalf("export from the default home: %+v, %v", exp, err)
	}
	if _, err := (Adapter{}).Export(adapter.Conversation{Native: "../../etc", Workdir: w, DefaultHome: def}); err == nil {
		t.Error("an id that is no Claude session id named a file")
	}
}
