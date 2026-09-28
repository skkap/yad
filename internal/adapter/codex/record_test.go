//go:build realharness

// The fixture recorder. It drives the real codex through the real adapter and
// keeps the conversation, scrubbed, as testdata/codex-<version>/<name>.jsonl.
// Every other test replays those files; none of them spends a token.
//
// Run by hand, on a machine where codex is logged in, from a clean tree:
//
//	YAD_REAL_HARNESS=1 go test -tags realharness -run TestRecord -v ./internal/adapter/codex/
//
// Each scenario is one short turn on the cheapest model. Codex runs with a
// throwaway CODEX_HOME holding only a link to the login, so the recording
// machine's config, hooks and history stay out of it. Read the diff before
// committing: the scrub removes what it knows about, not everything that
// could leak.

package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

// recordModel is the cheapest model the recording account offers.
const recordModel = "gpt-5.6-luna"

type scenario struct {
	name     string
	context  string
	prompt   string
	model    string
	effort   string
	settings map[string]string
	resume   bool // resume a thread that does not exist
	// before, when set, is a first turn in a new thread; the scenario's own
	// turn resumes it, and only that turn is recorded.
	before string
	// fork makes the scenario's own turn fork the before thread into a new
	// one rather than resume it; with no before, it forks a thread that does
	// not exist.
	fork bool
	act  func(t *testing.T, tr adapter.Turn, e v1.Event) bool
}

var scenarios = []scenario{
	{name: "plain", context: "You are terse.", prompt: "Reply with exactly: pong"},
	{name: "tool", prompt: "Run the shell command `cat note.txt` and reply with its output only."},
	{name: "error", prompt: "Reply: hi", model: "gpt-nonexistent-9"},
	// One tool call of each outcome, for a tool result's is_error and
	// exit_code: a shell command that fails and one that succeeds.
	{name: "tool-outcomes", prompt: "Run exactly these two shell commands, one at a time, then reply done: 'echo out; exit 3', then 'echo ok'."},
	{name: "file-change", prompt: "Use apply_patch to create the file hello.txt containing the line hi, then reply done. Do not run any shell command."},
	{name: "effort", prompt: "Reply with exactly: pong", effort: "low"},
	{name: "effort-rejected", prompt: "Reply with exactly: pong", effort: "bogus"},
	{name: "resume-missing", prompt: "Reply: hi", resume: true},
	{name: "resume", before: "Remember this word: plum. Reply with exactly: ok",
		prompt: "Which word did I ask you to remember? Reply with the word only."},
	// A continuing run whose context the session's first run did not have:
	// the adapter injects it before the turn, and the answer shows it arrived.
	{name: "resume-context", before: "Say hello.",
		context: "The codeword is BLUE. If asked, give the codeword.",
		prompt:  "What is the codeword? Reply with the word only."},
	// A fork: the second turn runs in a new thread copied from the first's,
	// and the recorder checks the first's rollout was not written to
	// (decision 0065). With a context, which the adapter injects as it does
	// on a resume.
	{name: "fork", before: "Remember this word: plum. Reply with exactly: ok",
		context: "You are terse.",
		prompt:  "Which word did I ask you to remember? Reply with the word only.", fork: true},
	{name: "fork-missing", prompt: "Reply: hi", fork: true},
	{name: "interrupt", prompt: "Write the numbers from 1 to 200 as English words, one per line. No tools.",
		act: func(t *testing.T, tr adapter.Turn, e v1.Event) bool {
			if e.Kind != v1.EventText {
				return false
			}
			if err := tr.Interrupt(); err != nil {
				t.Error(err)
			}
			return true
		}},
	{name: "steer", prompt: "Run the shell command 'sleep 4', then read note.txt and reply with its contents.",
		act: func(t *testing.T, tr adapter.Turn, e v1.Event) bool {
			if e.Kind != v1.EventToolCall {
				return false
			}
			if err := tr.Steer("Also: end your final reply with the word STEERED."); err != nil {
				t.Error(err)
			}
			return true
		}},
	{name: "approval", settings: map[string]string{"approval": "untrusted", "sandbox": "read-only"},
		prompt: "Run the shell command 'touch out.txt' and then say done."},
}

func TestRecord(t *testing.T) {
	if os.Getenv("YAD_REAL_HARNESS") != "1" {
		t.Skip("set YAD_REAL_HARNESS=1 to spend tokens recording fixtures")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(out))
	version := fields[len(fields)-1]
	dir := filepath.Join("testdata", "codex-"+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	only := os.Getenv("YAD_RECORD_ONLY")

	for _, s := range scenarios {
		if only != "" && only != s.name {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			work := t.TempDir()
			if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte("hello from a small file\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			codexHome := t.TempDir()
			if err := os.Symlink(filepath.Join(home, ".codex", "auth.json"), filepath.Join(codexHome, "auth.json")); err != nil {
				t.Fatal(err)
			}
			env := []string{"CODEX_HOME=" + codexHome}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			base := adapter.Spec{Binary: bin, Workdir: work, Model: recordModel, Env: env, Settings: s.settings}
			var native string
			if s.before != "" {
				spec := base
				spec.RunID, spec.Brief = "rec-"+s.name+"-before", v1.Brief{Instruction: s.before}
				first, err := Adapter{}.Start(ctx, spec)
				if err != nil {
					t.Fatal(err)
				}
				for range first.Events() {
				}
				if o := first.Wait(); o.State != v1.RunSucceeded {
					t.Fatalf("the first turn: %s %+v", o.State, o.Error)
				}
				native = first.NativeSessionID()
			}
			spec := base
			spec.RunID = "rec-" + s.name
			spec.Brief = v1.Brief{Context: s.context, Instruction: s.prompt}
			if s.model != "" {
				spec.Model = s.model
			}
			spec.Effort = s.effort
			spec.NativeSessionID = native
			if s.resume {
				spec.NativeSessionID = "01a0b86e-0000-7000-8000-000000000000"
			}
			var source []byte
			if s.fork {
				spec.NativeSessionID, spec.ForkFrom = "", native
				if native == "" {
					spec.ForkFrom = "01a0b86e-0000-7000-8000-000000000000"
				} else {
					source = rollout(t, codexHome, native)
				}
			}
			var raw bytes.Buffer
			tr, err := Adapter{Raw: func(adapter.Spec) io.Writer { return &raw }}.Start(ctx, spec)
			if err != nil {
				t.Fatal(err)
			}
			acted := s.act == nil
			for e := range tr.Events() {
				if !acted {
					acted = s.act(t, tr, e)
				}
			}
			o := tr.Wait()
			t.Logf("%s: %s %q %+v %+v", s.name, o.State, o.FinalText, o.Error, o.Usage)
			if source != nil {
				if o.NativeSessionID == native {
					t.Errorf("the fork ran in the forked thread %s", native)
				}
				if after := rollout(t, codexHome, native); !bytes.Equal(after, source) {
					t.Errorf("the fork wrote to the forked thread's rollout: %d bytes before, %d after", len(source), len(after))
				}
			}
			scrubbed := scrub(raw.Bytes(), [][2]string{
				{resolved(work), "/work"}, {work, "/work"},
				{resolved(codexHome), "/codex-home"}, {codexHome, "/codex-home"},
				{home, "/home/user"},
			}, host)
			if err := os.WriteFile(filepath.Join(dir, s.name+".jsonl"), scrubbed, 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestRecordSchema keeps the schema the installed codex generates, which
// schema_test.go pins.
func TestRecordSchema(t *testing.T) {
	if os.Getenv("YAD_REAL_HARNESS") != "1" {
		t.Skip("set YAD_REAL_HARNESS=1 to record the schema")
	}
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(out))
	version := fields[len(fields)-1]
	b, f := generateSchema(context.Background(), bin)
	if f != "" {
		t.Fatalf("%s — run `codex app-server generate-json-schema --out DIR` to see why", f)
	}
	dir := filepath.Join("testdata", "codex-"+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, schemaFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := SchemaHash(b)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("codex %s: the adapter's surface hashes to %s — pin it in schema.go", version, sum)
}

// rollout is a thread's rollout as Codex keeps it, found by id under the
// home's sessions directory, which is dated.
func rollout(t *testing.T, codexHome, id string) []byte {
	t.Helper()
	var found []string
	filepath.WalkDir(filepath.Join(codexHome, "sessions"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, id+".jsonl") {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 1 {
		t.Fatalf("the rollout of thread %s: %v", id, found)
	}
	b, err := os.ReadFile(found[0])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func resolved(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

var (
	emailish = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	agent    = regexp.MustCompile(`"userAgent":"[^"]*"`)
	install  = regexp.MustCompile(`"installationId":"[^"]*"`)
	// Codex 0.157.1's rate-limit snapshot names the ChatGPT account it
	// belongs to.
	account = regexp.MustCompile(`"accountId":"[^"]*"`)
)

// scrub removes the recording machine from a conversation: its paths, its
// host name, its installation id, what the user agent says about the
// terminal it ran in, its account id, and any address.
func scrub(raw []byte, paths [][2]string, host string) []byte {
	s := string(raw)
	for _, p := range paths {
		if p[0] != "" {
			s = strings.ReplaceAll(s, p[0], p[1])
		}
	}
	if host != "" {
		s = strings.ReplaceAll(s, host, "host")
	}
	s = agent.ReplaceAllString(s, `"userAgent":"yad/0 (recorded)"`)
	s = install.ReplaceAllString(s, `"installationId":"00000000-0000-0000-0000-000000000000"`)
	s = account.ReplaceAllString(s, `"accountId":"00000000-0000-0000-0000-000000000000"`)
	s = emailish.ReplaceAllString(s, "someone@example.com")
	// Every line must still be JSON: a scrub that broke one would make the
	// fixture test something Codex never wrote.
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if !json.Valid([]byte(line)) {
			panic("scrub broke a line: " + line)
		}
	}
	return []byte(s)
}
