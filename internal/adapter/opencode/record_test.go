//go:build realharness

// The fixture recorder. It drives the real OpenCode through the real adapter
// and keeps the conversation, scrubbed, as
// testdata/opencode-<version>/<name>.jsonl. Every other test replays those
// files; none of them spends a token or reaches the network.
//
// Run by hand, from a clean tree, with the OpenCode to record on PATH:
//
//	YAD_REAL_HARNESS=1 go test -tags realharness -run TestRecord -v ./internal/adapter/opencode/
//
// It needs no login and spends nothing: every scenario runs on one of
// OpenCode Zen's free models, and OpenCode runs in a throwaway home — XDG
// directories and HOME both — so the recording machine's providers, config,
// skills and sessions stay out of it. Read the diff before committing: the
// scrub removes what it knows about, not everything that could leak.

package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

// recordModel is a free model on OpenCode Zen that answers quickly and
// streams its reasoning; effortModel is a free one with effort levels.
const (
	recordModel = "opencode/nemotron-3.5-lightning-free"
	effortModel = "opencode/longcat-2.5-preview-free"
)

// askBash makes OpenCode ask before a bash command, so a permission request
// is recorded: its default configuration asks for none.
const askBash = `{"permission":{"bash":"ask"}}`

type scenario struct {
	name     string
	context  string
	prompt   string
	model    string
	effort   string
	settings map[string]string
	env      []string
	resume   bool // resume a session that does not exist
	// before, when set, is a first turn in a new session; the scenario's own
	// turn resumes it, and only that turn is recorded.
	before string
	// fork makes the scenario's own turn fork the before session; with no
	// before, it forks one that does not exist.
	fork bool
	act  func(t *testing.T, tr adapter.Turn, e v1.Event) bool
}

var scenarios = []scenario{
	{name: "plain", context: "You are terse.", prompt: "Reply with exactly: pong"},
	{name: "tool", prompt: "Use the bash tool to run `cat note.txt`, then reply with its output only."},
	{name: "tool-outcomes", prompt: "Use the bash tool to run exactly these two commands, one at a time, then reply done: `echo out; exit 3`, then `echo ok`."},
	{name: "file-change", prompt: "Use the write tool to create the file hello.txt containing the line hi, then reply done. Do not run any shell command."},
	{name: "error", prompt: "Reply: hi", model: "opencode/no-such-model"},
	{name: "effort", prompt: "Reply with exactly: pong", model: effortModel, effort: "high"},
	{name: "effort-rejected", prompt: "Reply with exactly: pong", model: effortModel, effort: "bogus"},
	// The run's context reaches the model from the first turn on, through
	// OpenCode's instructions (decision 0050, 0072).
	{name: "context", context: "The codeword is BLUE. If asked for the codeword, give it.",
		prompt: "What is the codeword? Reply with the word only."},
	{name: "resume-missing", prompt: "Reply: hi", resume: true},
	{name: "resume", before: "Remember this word: plum. Reply with exactly: ok",
		prompt: "Which word did I ask you to remember? Reply with the word only."},
	// A continuing run whose context the session's first run did not have.
	{name: "resume-context", before: "Say hello.",
		context: "The codeword is BLUE. If asked for the codeword, give it.",
		prompt:  "What is the codeword? Reply with the word only."},
	{name: "fork", before: "Remember this word: plum. Reply with exactly: ok",
		context: "You are terse.",
		prompt:  "Which word did I ask you to remember? Reply with the word only.", fork: true},
	{name: "fork-missing", prompt: "Reply: hi", fork: true},
	{name: "interrupt", prompt: "Write the numbers from 1 to 300 as English words, one per line. No tools.",
		act: func(t *testing.T, tr adapter.Turn, e v1.Event) bool {
			if e.Kind != v1.EventText {
				return false
			}
			if err := tr.Interrupt(); err != nil {
				t.Error(err)
			}
			return true
		}},
	{name: "permission", env: []string{envConfig + "=" + askBash},
		prompt: "Use the bash tool to run `echo yad-probe`, then reply with its output only."},
	{name: "permission-rejected", env: []string{envConfig + "=" + askBash},
		settings: map[string]string{"permission_mode": "reject"},
		prompt:   "Use the bash tool to run `echo yad-probe`, then reply with its output only."},
}

func TestRecord(t *testing.T) {
	if os.Getenv("YAD_REAL_HARNESS") != "1" {
		t.Skip("set YAD_REAL_HARNESS=1 to record fixtures")
	}
	bin, version := installed(t)
	dir := filepath.Join("testdata", "opencode-"+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	realHome, _ := os.UserHomeDir()
	home := throwaway(t)
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
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			base := adapter.Spec{Binary: bin, Workdir: work, Model: recordModel, Env: append(home.env(), s.env...), Settings: s.settings}
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
				spec.NativeSessionID = missingSession
			}
			if s.fork {
				spec.NativeSessionID, spec.ForkFrom = "", native
				if native == "" {
					spec.ForkFrom = missingSession
				}
			}
			var raw bytes.Buffer
			started := time.Now()
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
			t.Logf("%s: %s %q %+v %+v in %s", s.name, o.State, o.FinalText, o.Error, o.Usage, time.Since(started).Round(time.Millisecond))
			if s.fork && native != "" && o.NativeSessionID == native {
				t.Errorf("the fork ran in the forked session %s", native)
			}
			scrubbed := scrub(raw.Bytes(), [][2]string{
				{resolved(work), "/work"}, {work, "/work"},
				{resolved(home.root), "/opencode-home"}, {home.root, "/opencode-home"},
				{realHome, "/home/user"},
			}, host)
			if err := os.WriteFile(filepath.Join(dir, s.name+".jsonl"), scrubbed, 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestRecordModels keeps what `opencode models` prints in a home with no
// login: OpenCode Zen's free models (list-models.txt). It starts no session
// and spends nothing.
func TestRecordModels(t *testing.T) {
	if os.Getenv("YAD_REAL_HARNESS") != "1" {
		t.Skip("set YAD_REAL_HARNESS=1 to record the model list")
	}
	bin, version := installed(t)
	home := throwaway(t)
	cmd := exec.Command(bin, "models")
	cmd.Dir, cmd.Env = home.root, append(os.Environ(), home.env()...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "opencode-"+version, "list-models.txt"), out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func installed(t *testing.T) (bin, version string) {
	t.Helper()
	bin, err := exec.LookPath("opencode")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	return bin, strings.TrimSpace(string(out))
}

// openCodeHome is a throwaway home for OpenCode: every directory it reads
// its configuration, credentials and sessions from, and HOME, from which it
// reads skills and instructions that are the recording machine's.
type openCodeHome struct{ root string }

func throwaway(t *testing.T) openCodeHome {
	t.Helper()
	return openCodeHome{root: t.TempDir()}
}

func (h openCodeHome) env() []string {
	var env []string
	for _, v := range []string{"HOME", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"} {
		dir := filepath.Join(h.root, strings.ToLower(v))
		os.MkdirAll(dir, 0o700)
		env = append(env, v+"="+dir)
	}
	return env
}

func resolved(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

// scrub removes the recording machine from a conversation: its paths and its
// host name. Every line must still be JSON afterwards.
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
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		if !json.Valid([]byte(line)) {
			panic("scrub broke a line: " + line)
		}
	}
	return []byte(s)
}
