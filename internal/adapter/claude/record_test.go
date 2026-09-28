//go:build realharness

// The fixture recorder. It drives the real claude through the real adapter and
// keeps what Claude wrote, scrubbed, as testdata/claude-<version>/<name>.jsonl.
// Every other test replays those files; none of them spends a token.
//
// Run by hand, on a machine where claude is logged in, from a clean tree:
//
//	YAD_REAL_HARNESS=1 go test -tags realharness -run TestRecord -v ./internal/adapter/claude/
//
// Each scenario is one short haiku turn. Read the diff before committing it:
// the scrub below removes what it knows about, not everything that could leak.

package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/adapter"
)

type scenario struct {
	name    string
	context string
	prompt  string
	model   string
	mode    string
	resume  bool // resume a session that does not exist
	// before, when set, is a first turn run in a new session; the scenario's
	// own turn resumes it, and only that turn is recorded.
	before string
	// fork makes the scenario's own turn fork the before session into a new
	// one rather than resume it; with no before, it forks a session that
	// does not exist.
	fork bool
	// act is called with each event and may steer or interrupt; it returns true
	// once it has acted.
	act func(t *testing.T, tr adapter.Turn, e v1.Event) bool
	// instrument points the turn at a local HTTP server that authors the
	// answer instead of at the API: "retry" for a 429 claude gets past by
	// itself, "limit" for one it never does. A scenario with it spends no
	// token and needs no login, and it is the only way to record an
	// exhausted account without exhausting one (DEV-24, DEV-27).
	instrument string
}

var scenarios = []scenario{
	{name: "plain", context: "You are terse.", prompt: "Reply with exactly: pong"},
	{name: "tool", prompt: "Use the Read tool to read note.txt, then reply with its contents only."},
	{name: "error", prompt: "Reply: hi", model: "claude-nonexistent-9"},
	// One tool call of each outcome, for a tool result's is_error: a shell
	// command that fails, one that succeeds, and a Read of no file.
	{name: "tool-outcomes", prompt: "Make exactly these three tool calls, one at a time, then reply done: the Bash command 'echo out; exit 3', then the Bash command 'echo ok', then the Read tool on missing.txt."},
	{name: "prompt-too-long", prompt: "Reply: ok. " + strings.Repeat("lorem ipsum dolor sit amet ", 45000)},
	{name: "resume-missing", prompt: "Reply: hi", resume: true},
	{name: "resume", before: "Remember this word: plum. Reply with exactly: ok",
		prompt: "Which word did I ask you to remember? Reply with the word only."},
	// A fork: the second turn is a new session opened from a copy of the
	// first's conversation, and the recorder checks the first's transcript
	// was not written to (decision 0065).
	{name: "fork", before: "Remember this word: plum. Reply with exactly: ok",
		prompt: "Which word did I ask you to remember? Reply with the word only.", fork: true},
	{name: "fork-missing", prompt: "Reply: hi", fork: true},
	{name: "interrupt", prompt: "Write the numbers from 1 to 200 as words, one per line.",
		act: func(t *testing.T, tr adapter.Turn, e v1.Event) bool {
			if e.Kind != v1.EventText {
				return false
			}
			if err := tr.Interrupt(); err != nil {
				t.Error(err)
			}
			return true
		}},
	{name: "steer-tool", prompt: "Run the bash command 'sleep 5', then read note.txt and reply with its contents.",
		act: func(t *testing.T, tr adapter.Turn, e v1.Event) bool {
			if e.Kind != v1.EventToolCall {
				return false
			}
			if err := tr.Steer("Also: end your final reply with the word STEERED."); err != nil {
				t.Error(err)
			}
			return true
		}},
	{name: "steer-followup", prompt: "Write the numbers from 1 to 40 as words, one per line.",
		act: func(t *testing.T, tr adapter.Turn, e v1.Event) bool {
			if e.Kind != v1.EventText {
				return false
			}
			if err := tr.Steer("Now reply with exactly: steered"); err != nil {
				t.Error(err)
			}
			return true
		}},
	{name: "permission-denied", mode: "default", prompt: "Run the bash command 'echo hi > out.txt' and then say done."},

	// The two rate-limit paths, against the instrument rather than the API.
	// They are here rather than hand-written because the difference between
	// them is the whole of DOMAIN.md's usage-limit/rate-limit distinction, and
	// a stream written by hand agrees with whatever the code already does.
	{name: "api-retry-then-success", instrument: "retry", prompt: "Reply with exactly: pong"},
	{name: "retries-exhausted-429", instrument: "limit", prompt: "Reply with exactly: pong"},
}

// instrumentServer authors a harness's answers locally: a 429 the client sees
// as throttling, and in "retry" mode a plain success once it has retried.
//
// Claude 2.1.278 reached over an API key emits no rate_limit_event at all, no
// matter what rate-limit headers the answer carries: the unified windows are a
// subscription's, and an API key has none. So this records the 429 paths and
// not the subscription rejection; see testdata/README.md.
func instrumentServer(t *testing.T, mode string) string {
	t.Helper()
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		attempt := n
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		// One retry is enough to prove the path and keeps the recording
		// short; claude's own backoff makes each further one slower.
		if mode == "retry" && attempt > 2 {
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":1}}`)
			return
		}
		w.Header().Set("retry-after", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"You've exceeded your account's rate limit."}}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRecord(t *testing.T) {
	if os.Getenv("YAD_REAL_HARNESS") != "1" {
		t.Skip("set YAD_REAL_HARNESS=1 to spend tokens recording fixtures")
	}
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatal(err)
	}
	version := strings.Fields(string(out))[0]
	dir := filepath.Join("testdata", "claude-"+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	only := os.Getenv("YAD_RECORD_ONLY")

	for _, s := range scenarios {
		if only != "" && only != s.name {
			continue
		}
		t.Run(s.name, func(t *testing.T) {
			work := t.TempDir()
			var env []string
			if s.instrument != "" {
				// The key is passed in Spec.Env because Scrub removes it from
				// the inherited environment on purpose; nothing about it is a
				// credential, and it never reaches the API.
				env = []string{
					"ANTHROPIC_BASE_URL=" + instrumentServer(t, s.instrument),
					"ANTHROPIC_API_KEY=sk-ant-instrument-not-a-real-key",
				}
			}
			if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte("hello from a small file\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var raw bytes.Buffer
			a := Adapter{Raw: func(adapter.Spec) io.Writer { return &raw }}
			// Two minutes is a short turn's budget. An instrument scenario is
			// claude's own retry ladder instead: ten attempts with a backoff
			// that reaches forty seconds, about three minutes of waiting in
			// which nothing is asked of a model.
			budget := 2 * time.Minute
			if s.instrument != "" {
				budget = 6 * time.Minute
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			var native string
			if s.before != "" {
				first, err := Adapter{}.Start(ctx, adapter.Spec{
					RunID: "rec-" + s.name + "-before", Binary: bin, Workdir: work, Model: "haiku",
					Brief:    v1.Brief{Instruction: s.before},
					Settings: map[string]string{"permission_mode": "bypassPermissions"},
				})
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
			spec := adapter.Spec{
				RunID: "rec-" + s.name, Binary: bin, Workdir: work,
				Model:    firstNonEmpty(s.model, "haiku"),
				Brief:    v1.Brief{Context: s.context, Instruction: s.prompt},
				Settings: map[string]string{"permission_mode": firstNonEmpty(s.mode, "bypassPermissions")},
				Env:      env,
			}
			spec.NativeSessionID = native
			if s.resume {
				spec.NativeSessionID = newUUID()
			}
			var source []byte
			if s.fork {
				spec.NativeSessionID, spec.ForkFrom = "", native
				if native == "" {
					spec.ForkFrom = newUUID()
				} else {
					source = transcript(t, home, native)
				}
			}
			tr, err := a.Start(ctx, spec)
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
			t.Logf("%s: %s %+v", s.name, o.State, o.Error)
			if source != nil {
				if o.NativeSessionID == native {
					t.Errorf("the fork ran in the forked session %s", native)
				}
				if after := transcript(t, home, native); !bytes.Equal(after, source) {
					t.Errorf("the fork wrote to the forked session's transcript: %d bytes before, %d after", len(source), len(after))
				}
			}
			scrubbed := scrub(raw.Bytes(), [][2]string{{resolved(work), "/work"}, {work, "/work"}, {home, "/home/user"}})
			if err := os.WriteFile(filepath.Join(dir, s.name+".jsonl"), scrubbed, 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// transcript is a session's transcript as Claude keeps it, found by id in
// whichever project directory holds it.
func transcript(t *testing.T, home, id string) []byte {
	t.Helper()
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(home, ".claude")
	}
	found, err := filepath.Glob(filepath.Join(dir, "projects", "*", id+".jsonl"))
	if err != nil || len(found) != 1 {
		t.Fatalf("the transcript of session %s: %v %v", id, found, err)
	}
	b, err := os.ReadFile(found[0])
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
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

// initNoise is what the init frame says about the recording machine — its
// tools, servers, plugins, skills and paths. The adapter reads none of it.
var initNoise = []string{"tools", "mcp_servers", "slash_commands", "terminal_slash_commands", "skills", "plugins", "agents", "memory_paths", "messaging_socket_path"}

var emailish = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

// scrub removes the recording machine from a stream: its paths, the init
// frame's inventory, thinking signatures (opaque, long, useless here), and the
// body of any oversized echoed prompt.
func scrub(raw []byte, paths [][2]string) []byte {
	replace := func(s string) string {
		for _, p := range paths {
			if p[0] == "" {
				continue
			}
			s = strings.ReplaceAll(s, p[0], p[1])
			// Claude encodes paths into directory names with dashes.
			s = strings.ReplaceAll(s, strings.ReplaceAll(p[0], "/", "-"), strings.ReplaceAll(p[1], "/", "-"))
		}
		return emailish.ReplaceAllString(s, "someone@example.com")
	}
	var frames []map[string]any
	var rawLines [][]byte
	for _, line := range bytes.Split(bytes.TrimRight(raw, "\n"), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) != nil {
			m = nil
		}
		frames = append(frames, m)
		rawLines = append(rawLines, line)
	}
	joinToolInput(frames, replace)

	var out bytes.Buffer
	for i, m := range frames {
		if m == nil {
			out.WriteString(replace(string(rawLines[i])))
			out.WriteByte('\n')
			continue
		}
		if m["type"] == "system" && m["subtype"] == "init" {
			for _, k := range initNoise {
				delete(m, k)
			}
		}
		if m["type"] == "user" && m["isReplay"] == true {
			if msg, ok := m["message"].(map[string]any); ok {
				if c, ok := msg["content"].(string); ok && len(c) > 4096 {
					msg["content"] = "<elided: a " + strconv.Itoa(len(c)) + "-byte prompt>"
				}
			}
		}
		dropSignatures(m)
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		enc.Encode(m)
		out.WriteString(replace(strings.TrimRight(b.String(), "\n")))
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// joinToolInput reassembles each streamed tool input, scrubs it whole, and
// puts it back in the block's first delta. Fragments split a path anywhere —
// "/var/folders" in one line, the rest in the next — so no per-line
// replacement can catch it.
func joinToolInput(frames []map[string]any, replace func(string) string) {
	var first map[string]any // the delta holding the block's joined input
	var joined strings.Builder
	finish := func() {
		if first != nil {
			first["partial_json"] = replace(joined.String())
		}
		first = nil
		joined.Reset()
	}
	for _, m := range frames {
		ev, _ := m["event"].(map[string]any)
		if m == nil || m["type"] != "stream_event" || ev == nil {
			continue
		}
		switch ev["type"] {
		case "content_block_start", "content_block_stop", "message_stop":
			finish()
		case "content_block_delta":
			d, _ := ev["delta"].(map[string]any)
			if d == nil || d["type"] != "input_json_delta" {
				continue
			}
			s, _ := d["partial_json"].(string)
			joined.WriteString(s)
			if first == nil {
				first = d
			} else {
				d["partial_json"] = ""
			}
		}
	}
	finish()
}

func dropSignatures(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if k == "signature" {
				x[k] = ""
				continue
			}
			dropSignatures(val)
		}
	case []any:
		for _, val := range x {
			dropSignatures(val)
		}
	}
}
