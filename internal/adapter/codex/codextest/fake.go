// Package codextest is a fake codex for tests: the test binary, re-executed
// under the name codex, plays the app-server from a recorded conversation.
// Only tests import it, so it is never part of yad.
package codextest

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// play is the app-server. Re-executed with CODEX_TEST_FIXTURE set, the test
// binary plays a recorded conversation (testdata/codex-<version>/*.jsonl) as the
// app-server, so every test drives the real adapter against a real process —
// pipes, process group, exit status and all — without a token.
//
// A fixture holds both directions: what Codex wrote, and what YAD wrote
// wrapped as {">": …}. The fake writes Codex's lines in order and, at each of
// YAD's, waits for the adapter to send the same thing — a request by its
// method, an answer to one of Codex's requests by being an answer. Its own
// answers carry the ids the adapter actually used. A resume's thread id is
// rewritten to the one the adapter asked for, as Codex would have used it,
// unless CODEX_TEST_KEEP_THREAD is set (and threads are not kept, below).
//
// A thread/inject_items the fixture did not record is answered at once with an
// empty result, as Codex answers one, so a fixture recorded without a context
// still plays for a resumed run that has one.
//
// CODEX_TEST_HOLD names a file the fake waits for before it writes anything;
// CODEX_TEST_WAIT is how long it waits for each message it expects (10s).
//
// Once the conversation is played it exits when its input closes, as the
// app-server does. CODEX_TEST_MODE picks another ending: "died" exits 1 at
// once with something on stderr; "exit" exits 0 at once; "linger" ignores
// the closed input and SIGTERM; "mute" closes its output and then lingers the
// same way; "silent" reads its input and never writes a line; "deaf" never
// takes an interrupt, and dies only by signal or, at a gate, once the gate
// opens.
//
// The rest serve the end-to-end tests in cmd/yad, which drive the whole runner
// and need the fake to behave as codex does across runs, not only within one:
//
//   - CODEX_TEST_THREADS names a directory where the fake keeps a rollout per
//     thread, as codex keeps them in CODEX_HOME. thread/start mints a new
//     thread; thread/resume continues one with a rollout and is refused, as
//     codex refuses it (resume-missing.jsonl), without one; thread/fork mints
//     a new thread holding a copy of one with a rollout, and is refused the
//     same way without one (fork-missing.jsonl); each answers the fixture's
//     thread request, and KEEP_THREAD has no effect. Each turn's
//     instruction is appended to its thread's rollout.
//   - CODEX_TEST_RECALL, with threads kept, makes the answer name what the
//     thread was asked before this turn ("earlier: a | b"), which is how a
//     test sees that a run had its session's context.
//   - CODEX_TEST_READ names a file in the fake's working directory whose
//     contents replace the recorded answer, as if the recorded command had
//     read it there.
//   - CODEX_TEST_GATE names a file the fake waits for, up to a minute, before
//     the turn completes, with every other event out; CODEX_TEST_AT_GATE is a
//     file it creates on reaching it. A turn/interrupt at the gate ends the
//     turn interrupted, as codex does, unless the mode is "deaf".
//   - CODEX_TEST_PID is a file the fake writes its pid to.
//   - A device-code login (login-device*.jsonl) is held before its
//     account/login/completed at CODEX_TEST_GATE, as the owner takes a while
//     to type the code, and a completion saying success writes the stand-in
//     credential into CODEX_HOME first, as codex writes auth.json —
//     unless CODEX_TEST_LOGIN is "nocred", a codex whose login said yes and
//     left nothing its own check finds.
//   - CODEX_TEST_STARTS is a file the fake appends a line to per thread
//     request: its working directory, its arguments, and the request with the
//     thread it named.
func play() {
	logf := openLog()
	defer logf.Close()
	log := func(kind string, v any) {
		b, _ := json.Marshal(map[string]any{kind: v})
		logf.Write(append(b, '\n'))
	}
	log("argv", os.Args[1:])
	mode := os.Getenv("CODEX_TEST_MODE")
	if f := os.Getenv("CODEX_TEST_PID"); f != "" {
		os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())), 0o600)
	}
	threads := os.Getenv("CODEX_TEST_THREADS")

	in := make(chan clientMsg, 64)
	eof := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(nil, 64<<20)
		for sc.Scan() {
			log("stdin", sc.Text())
			var m clientMsg
			json.Unmarshal(sc.Bytes(), &m)
			in <- m
		}
		close(eof)
	}()
	if hold := os.Getenv("CODEX_TEST_HOLD"); hold != "" {
		// Nothing is written until the test says, so what the adapter does
		// before Codex answers is not a race.
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if _, err := os.Stat(hold); err == nil {
				break
			}
		}
	}
	if mode == "silent" {
		<-eof
		time.Sleep(time.Hour)
	}

	raw, err := os.ReadFile(os.Getenv("CODEX_TEST_FIXTURE"))
	if err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(2)
	}
	out := bufio.NewWriter(os.Stdout)
	ids := map[string]string{} // recorded request id → the adapter's
	var recordedThread, askedThread string
	if threads != "" {
		recordedThread = firstThread(string(raw))
	}
	answer := finalAnswer(string(raw))
	var override string
	if name := os.Getenv("CODEX_TEST_READ"); name != "" {
		b, err := os.ReadFile(name)
		if err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(2)
		}
		override = strings.TrimSpace(string(b))
	}
	var early []clientMsg // what the adapter sent before the fixture expected it
	take := func(want clientMsg) (clientMsg, bool) {
		match := func(m clientMsg) bool {
			if want.Method == "" {
				return m.Method == "" && len(m.ID) > 0
			}
			if threads != "" && isThreadRequest(want.Method) {
				return isThreadRequest(m.Method)
			}
			return m.Method == want.Method
		}
		for i, m := range early {
			if match(m) {
				early = append(early[:i], early[i+1:]...)
				return m, true
			}
		}
		wait := 10 * time.Second
		if d, err := time.ParseDuration(os.Getenv("CODEX_TEST_WAIT")); err == nil {
			wait = d
		}
		timeout := time.After(wait)
		for {
			select {
			case m := <-in:
				if match(m) {
					return m, true
				}
				if m.Method == "thread/inject_items" {
					// A fixture recorded without a context has no injection
					// to match; Codex answers one with an empty result.
					out.WriteString(`{"id":` + string(m.ID) + `,"result":{}}` + "\n")
					out.Flush()
					continue
				}
				early = append(early, m)
			case <-timeout:
				return clientMsg{}, false
			}
		}
	}
	deltaSent := false
play:
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var ours struct {
			Msg *clientMsg `json:">"`
		}
		if json.Unmarshal([]byte(line), &ours) == nil && ours.Msg != nil {
			out.Flush()
			if ours.Msg.Method == "turn/interrupt" && mode == "deaf" {
				time.Sleep(time.Hour)
			}
			got, ok := take(*ours.Msg)
			if !ok {
				os.Stderr.WriteString("the adapter never sent " + ours.Msg.Method + "\n")
				os.Exit(1)
			}
			if len(ours.Msg.ID) > 0 && ours.Msg.Method != "" {
				ids[string(ours.Msg.ID)] = string(got.ID)
			}
			if isThreadRequest(got.Method) {
				var p struct {
					ThreadID string `json:"threadId"`
				}
				json.Unmarshal(got.Params, &p)
				started(got.Method, p.ThreadID)
			}
			if threads != "" && isThreadRequest(got.Method) {
				var p struct {
					ThreadID string `json:"threadId"`
				}
				json.Unmarshal(got.Params, &p)
				earlier, err := os.ReadFile(filepath.Join(threads, p.ThreadID))
				switch {
				case got.Method == "thread/start":
					askedThread, earlier = newThread(), nil
					os.WriteFile(filepath.Join(threads, askedThread), nil, 0o600)
				case got.Method == "thread/fork" && err == nil && p.ThreadID != "":
					// The copy starts with everything the forked thread was
					// asked, and the forked thread's rollout is left alone.
					askedThread = newThread()
					os.WriteFile(filepath.Join(threads, askedThread), earlier, 0o600)
				case p.ThreadID == "" || err != nil:
					// As recorded in resume-missing.jsonl.
					out.WriteString(`{"id":` + string(got.ID) + `,"error":{"code":-32600,"message":"no rollout found for thread id ` + p.ThreadID + `"}}` + "\n")
					break play
				default:
					askedThread = p.ThreadID
				}
				if os.Getenv("CODEX_TEST_RECALL") != "" {
					override = "earlier: nothing"
					if s := strings.TrimSpace(string(earlier)); s != "" {
						override = "earlier: " + strings.Join(strings.Split(s, "\n"), " | ")
					}
				}
				continue
			}
			if threads != "" && got.Method == "turn/start" {
				var p struct {
					Input []struct {
						Text string `json:"text"`
					} `json:"input"`
				}
				json.Unmarshal(got.Params, &p)
				if f, err := os.OpenFile(filepath.Join(threads, askedThread), os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
					for _, in := range p.Input {
						f.WriteString(strings.ReplaceAll(in.Text, "\n", " ") + "\n")
					}
					f.Close()
				}
			}
			if got.Method == "thread/resume" && os.Getenv("CODEX_TEST_KEEP_THREAD") == "" {
				var p struct {
					ThreadID string `json:"threadId"`
				}
				json.Unmarshal(got.Params, &p)
				askedThread = p.ThreadID
				json.Unmarshal(ours.Msg.Params, &p)
				recordedThread = p.ThreadID
			}
			continue
		}
		var theirs struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.Unmarshal([]byte(line), &theirs)
		if theirs.Method == "" && len(theirs.ID) > 0 {
			if id, ok := ids[string(theirs.ID)]; ok {
				for _, end := range []string{",", "}"} {
					if old := `"id":` + string(theirs.ID) + end; strings.Contains(line, old) {
						line = strings.Replace(line, old, `"id":`+id+end, 1)
						break
					}
				}
			}
		}
		if recordedThread != "" && askedThread != "" {
			line = strings.ReplaceAll(line, recordedThread, askedThread)
		}
		if override != "" && answer.text != "" {
			var n struct {
				Method string `json:"method"`
				Params struct {
					ItemID string `json:"itemId"`
				} `json:"params"`
			}
			json.Unmarshal([]byte(line), &n)
			if n.Method == "item/agentMessage/delta" && n.Params.ItemID == answer.item {
				// The answer arrives whole, in the first of its deltas.
				if deltaSent {
					continue
				}
				deltaSent = true
				line = withDelta(line, override)
			} else {
				line = strings.ReplaceAll(line, quoted(answer.text), quoted(override))
			}
		}
		if strings.HasPrefix(line, `{"method":"account/login/completed"`) {
			out.Flush()
			loginGate(in, &early, eof)
			if strings.Contains(line, `"success":true`) && os.Getenv("CODEX_TEST_LOGIN") != "nocred" {
				if home := os.Getenv("CODEX_HOME"); home != "" {
					if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte(`{"stand_in":true}`), 0o600); err != nil {
						os.Exit(4)
					}
				}
			}
		}
		if strings.HasPrefix(line, `{"method":"turn/completed"`) {
			out.Flush()
			if id, ok := awaitGate(in, &early, mode == "deaf"); ok {
				out.WriteString(`{"id":` + string(id) + `,"result":{}}` + "\n")
				out.WriteString(strings.Replace(line, `"status":"completed"`, `"status":"interrupted"`, 1) + "\n")
				break play
			}
		}
		out.WriteString(line + "\n")
	}
	out.Flush()

	switch mode {
	case "died":
		os.Stderr.WriteString("Error: something went badly wrong\n")
		os.Exit(1)
	case "exit":
		os.Exit(0)
	case "linger":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
	case "mute":
		os.Stdout.Close()
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
	}
	select {
	case <-eof:
	case <-time.After(30 * time.Second): // never outlive a broken test by much
	}
	code, _ := strconv.Atoi(os.Getenv("CODEX_TEST_EXIT"))
	os.Exit(code)
}

// clientMsg is one message from the adapter.
type clientMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// awaitGate holds the turn before it completes until the test opens the gate,
// or until the adapter interrupts it, whose request id it returns. Whatever
// else the adapter sends meanwhile is kept for later.
func awaitGate(in <-chan clientMsg, early *[]clientMsg, deaf bool) (json.RawMessage, bool) {
	gate := os.Getenv("CODEX_TEST_GATE")
	if gate == "" {
		return nil, false
	}
	if at := os.Getenv("CODEX_TEST_AT_GATE"); at != "" {
		os.WriteFile(at, nil, 0o600)
	}
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); {
		select {
		case m := <-in:
			if m.Method == "turn/interrupt" {
				if !deaf {
					return m.ID, true
				}
				continue
			}
			*early = append(*early, m)
		case <-time.After(10 * time.Millisecond):
		}
		if _, err := os.Stat(gate); err == nil {
			return nil, false
		}
	}
	os.Exit(1)
	return nil, false
}

// loginGate holds a login's completion until the test opens the gate. What
// the adapter sends meanwhile is kept for later; input closing ends the fake,
// as it ends the app-server.
func loginGate(in <-chan clientMsg, early *[]clientMsg, eof <-chan struct{}) {
	gate := os.Getenv("CODEX_TEST_GATE")
	if gate == "" {
		return
	}
	if at := os.Getenv("CODEX_TEST_AT_GATE"); at != "" {
		os.WriteFile(at, nil, 0o600)
	}
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); {
		select {
		case m := <-in:
			*early = append(*early, m)
		case <-eof:
			os.Exit(0)
		case <-time.After(10 * time.Millisecond):
		}
		if _, err := os.Stat(gate); err == nil {
			return
		}
	}
	os.Exit(1)
}

// started notes a thread request in CODEX_TEST_STARTS.
func started(method, thread string) {
	f := os.Getenv("CODEX_TEST_STARTS")
	if f == "" {
		return
	}
	wd, _ := os.Getwd()
	line := wd + " " + strings.Join(os.Args[1:], " ") + " " + method
	if thread != "" {
		line += " " + thread
	}
	if w, err := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		w.WriteString(line + "\n")
		w.Close()
	}
}

func isThreadRequest(method string) bool {
	return method == "thread/start" || method == "thread/resume" || method == "thread/fork"
}

// newThread is a thread id of codex's shape, fresh each time, as codex mints
// one per thread/start.
func newThread() string {
	var b [16]byte
	rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// firstThread is the thread a recorded conversation ran in.
func firstThread(raw string) string {
	_, rest, _ := strings.Cut(raw, `"thread":{"id":"`)
	id, _, _ := strings.Cut(rest, `"`)
	return id
}

type recordedAnswer struct{ item, text string }

// finalAnswer is the last agent message a recorded conversation completed:
// the answer a run of it reports.
func finalAnswer(raw string) recordedAnswer {
	var a recordedAnswer
	for _, line := range strings.Split(raw, "\n") {
		var n struct {
			Method string `json:"method"`
			Params struct {
				Item struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Text string `json:"text"`
				} `json:"item"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &n) == nil && n.Method == "item/completed" && n.Params.Item.Type == "agentMessage" {
			a = recordedAnswer{n.Params.Item.ID, n.Params.Item.Text}
		}
	}
	return a
}

// withDelta rewrites a delta notification to carry text.
func withDelta(line, text string) string {
	var n map[string]any
	if json.Unmarshal([]byte(line), &n) != nil {
		return line
	}
	if p, ok := n["params"].(map[string]any); ok {
		p["delta"] = text
	}
	b, _ := json.Marshal(n)
	return string(b)
}

// quoted is s as a JSON string, the way it appears in a recorded line.
func quoted(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func openLog() *os.File {
	path := os.Getenv("CODEX_TEST_LOG")
	if path == "" {
		path = os.DevNull
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(3)
	}
	return f
}

// schema is `codex app-server generate-json-schema --out DIR`: it writes the
// bundle named by CODEX_TEST_SCHEMA, or fails as a codex without the command
// would, or with "fifo" leaves a FIFO in the bundle's place. With
// CODEX_TEST_SCHEMA_DETACH set, it leaves behind a detached process holding
// its stdout.
func schema() {
	args := os.Args[1:]
	if len(args) != 4 || args[0] != "app-server" || args[1] != "generate-json-schema" || args[2] != "--out" {
		os.Stderr.WriteString("error: unexpected arguments\n")
		os.Exit(2)
	}
	src := os.Getenv("CODEX_TEST_SCHEMA")
	if src == "fail" {
		os.Stderr.WriteString("error: unrecognized subcommand 'generate-json-schema'\n")
		os.Exit(2)
	}
	if src == "fifo" {
		// A FIFO where the bundle should be, which no one will ever write.
		if err := syscall.Mkfifo(filepath.Join(args[3], SchemaFile), 0o600); err != nil {
			os.Exit(6)
		}
		return
	}
	if os.Getenv("CODEX_TEST_SCHEMA_DETACH") != "" {
		// A descendant that leaves the process group with setsid and keeps
		// stdout open, as a launcher's detached updater can.
		// It says when it has left, or the group kill at the fake's exit
		// would take it first and nothing would hold the pipe.
		left := filepath.Join(args[3], "left")
		cmd := exec.Command("perl", "-MPOSIX", "-e", `POSIX::setsid(); open(F, ">", $ARGV[0]); close F; sleep 10`, left)
		cmd.Stdout = os.Stdout
		if err := cmd.Start(); err != nil {
			os.Exit(5)
		}
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if _, err := os.Stat(left); err == nil {
				break
			}
		}
	}
	b, err := os.ReadFile(src)
	if err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(filepath.Join(args[3], SchemaFile), b, 0o644); err != nil {
		os.Exit(4)
	}
}

// SchemaFile is the bundle generate-json-schema writes.
const SchemaFile = "codex_app_server_protocol.schemas.json"

// Version is what the fake answers to --version: the version the fixtures
// were recorded from.
const Version = "codex-cli 0.157.1"

// Child reports whether this process was started as the fake: by a test that
// set CODEX_TEST_FIXTURE or CODEX_TEST_SCHEMA, under the name codex — a test
// binary that also plays another harness tells them apart by the name it was
// started as.
func Child() bool {
	return filepath.Base(os.Args[0]) == "codex" && (os.Getenv("CODEX_TEST_FIXTURE") != "" || os.Getenv("CODEX_TEST_SCHEMA") != "")
}

// Main is the fake codex: --version, app-server generate-json-schema, login
// and login status, and the app-server itself.
//
// The login keeps a stand-in credential in CODEX_HOME, as codex keeps
// auth.json there, and login status answers from it in codex's own words and
// exit status; CODEX_TEST_LOGIN=fails makes the login exit 1 having written
// nothing, which is the owner walking away from it.
func Main() {
	args := os.Args[1:]
	switch {
	case len(args) == 1 && args[0] == "--version":
		os.Stdout.WriteString(Version + "\n")
	case len(args) > 1 && args[0] == "app-server" && args[1] == "generate-json-schema":
		schema()
	case len(args) == 1 && args[0] == "login":
		if os.Getenv("CODEX_TEST_LOGIN") == "fails" {
			os.Stderr.WriteString("login cancelled\n")
			os.Exit(1)
		}
		if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json"), []byte(`{"stand_in":true}`), 0o600); err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
	case len(args) == 2 && args[0] == "login" && args[1] == "status":
		// With no CODEX_HOME this is the default home, logged in for every
		// test that is not about logins (decision 0053).
		if home := os.Getenv("CODEX_HOME"); home == "" {
			os.Stdout.WriteString("Logged in using ChatGPT\n")
			return
		}
		if _, err := os.Stat(filepath.Join(os.Getenv("CODEX_HOME"), "auth.json")); err != nil {
			os.Stdout.WriteString("Not logged in\n")
			os.Exit(1)
		}
		os.Stdout.WriteString("Logged in using ChatGPT\n")
	default:
		play()
	}
}
