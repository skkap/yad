// Package codextest is a fake codex for tests: the test binary, re-executed
// under the name codex, plays the app-server from a recorded conversation.
// Only tests import it, so it is never part of yad.
package codextest

import (
	"bufio"
	"encoding/json"
	"os"
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
// unless CODEX_TEST_KEEP_THREAD is set.
//
// CODEX_TEST_HOLD names a file the fake waits for before it writes anything;
// CODEX_TEST_WAIT is how long it waits for each message it expects (10s).
//
// Once the conversation is played it exits when its input closes, as the
// app-server does. CODEX_TEST_MODE picks another ending: "died" exits 1 at
// once with something on stderr; "exit" exits 0 at once; "linger" ignores
// the closed input and SIGTERM; "mute" closes its output and then lingers the
// same way; "silent" reads its input and never writes a line; "deaf" never
// takes an interrupt, and dies only by signal.
func play() {
	logf := openLog()
	defer logf.Close()
	log := func(kind string, v any) {
		b, _ := json.Marshal(map[string]any{kind: v})
		logf.Write(append(b, '\n'))
	}
	log("argv", os.Args[1:])
	mode := os.Getenv("CODEX_TEST_MODE")

	type clientMsg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
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
	var early []clientMsg // what the adapter sent before the fixture expected it
	take := func(want clientMsg) (clientMsg, bool) {
		match := func(m clientMsg) bool {
			if want.Method == "" {
				return m.Method == "" && len(m.ID) > 0
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
				early = append(early, m)
			case <-timeout:
				return clientMsg{}, false
			}
		}
	}
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
// would.
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
const Version = "codex-cli 0.147.0"

// Child reports whether this process was started as the fake: by a test that
// set CODEX_TEST_FIXTURE or CODEX_TEST_SCHEMA, under the name codex — a test
// binary that also plays another harness tells them apart by the name it was
// started as.
func Child() bool {
	return filepath.Base(os.Args[0]) == "codex" && (os.Getenv("CODEX_TEST_FIXTURE") != "" || os.Getenv("CODEX_TEST_SCHEMA") != "")
}

// Main is the fake codex: --version, app-server generate-json-schema, and the
// app-server itself.
func Main() {
	args := os.Args[1:]
	switch {
	case len(args) == 1 && args[0] == "--version":
		os.Stdout.WriteString(Version + "\n")
	case len(args) > 1 && args[0] == "app-server" && args[1] == "generate-json-schema":
		schema()
	default:
		play()
	}
}
