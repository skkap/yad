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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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
	// act is called with each event and may steer or interrupt; it returns true
	// once it has acted.
	act func(t *testing.T, tr adapter.Turn, e v1.Event) bool
}

var scenarios = []scenario{
	{name: "plain", context: "You are terse.", prompt: "Reply with exactly: pong"},
	{name: "tool", prompt: "Use the Read tool to read note.txt, then reply with its contents only."},
	{name: "error", prompt: "Reply: hi", model: "claude-nonexistent-9"},
	{name: "prompt-too-long", prompt: "Reply: ok. " + strings.Repeat("lorem ipsum dolor sit amet ", 45000)},
	{name: "resume-missing", prompt: "Reply: hi", resume: true},
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
			if err := os.WriteFile(filepath.Join(work, "note.txt"), []byte("hello from a small file\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			var raw bytes.Buffer
			a := Adapter{Raw: func(adapter.Spec) io.Writer { return &raw }}
			spec := adapter.Spec{
				RunID: "rec-" + s.name, Binary: bin, Workdir: work,
				Model:    firstNonEmpty(s.model, "haiku"),
				Brief:    v1.Brief{Context: s.context, Instruction: s.prompt},
				Settings: map[string]string{"permission_mode": firstNonEmpty(s.mode, "bypassPermissions")},
			}
			if s.resume {
				spec.NativeSessionID = newUUID()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
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
			scrubbed := scrub(raw.Bytes(), [][2]string{{resolved(work), "/work"}, {work, "/work"}, {home, "/home/user"}})
			if err := os.WriteFile(filepath.Join(dir, s.name+".jsonl"), scrubbed, 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
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
