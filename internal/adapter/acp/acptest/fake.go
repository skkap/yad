// Package acptest is a fake ACP agent for tests: the test binary, re-executed,
// plays a recorded conversation (testdata/<agent>-<version>/*.jsonl) as the
// agent, so every test drives the real adapter against a real process —
// pipes, process group, exit status and all — without a token or a network.
// Only tests import it, so it is never part of yad.
package acptest

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The fake's knobs, by environment variable.
const (
	// EnvFixture names the conversation to play. A fixture holds both
	// directions: what the agent wrote, and what YAD wrote wrapped as
	// {">": …}. The fake writes the agent's lines in order and, at each of
	// YAD's, waits for the adapter to send the same thing — a request or a
	// notification by its method, an answer to one of the agent's requests by
	// being an answer. Its own answers carry the ids the adapter actually
	// used.
	EnvFixture = "ACP_TEST_FIXTURE"
	// EnvLog is a file the fake appends what it saw to, one JSON object per
	// line: its argv, what it found in its environment, and every line the
	// adapter wrote.
	EnvLog = "ACP_TEST_LOG"
	// EnvMode picks another ending: "died" exits 1 at once with something on
	// stderr, before answering the prompt; "exit" exits 0 the same way;
	// "deaf" never takes a cancel at the gate; "silent" reads its input and
	// never writes a line.
	EnvMode = "ACP_TEST_MODE"
	// EnvGate names a file the fake waits for, up to a minute, before it
	// answers the prompt, with every update out; EnvAtGate is a file it
	// creates on reaching it. A session/cancel at the gate answers the
	// prompt cancelled, as an agent does, unless the mode is "deaf".
	EnvGate   = "ACP_TEST_GATE"
	EnvAtGate = "ACP_TEST_AT_GATE"
	// EnvWait is how long the fake waits for each message it expects (10s).
	EnvWait = "ACP_TEST_WAIT"
	// EnvModels is what `models` prints; absent, the fake prints Models.
	EnvModels = "ACP_TEST_MODELS"
	// EnvVersion is what --version prints; absent, Version.
	EnvVersion = "ACP_TEST_VERSION"
	// EnvWatch names variables whose values the fake logs, beside the
	// presence and length of every variable it is given a name for here.
	EnvWatch = "ACP_TEST_WATCH"
)

// Version is what the fake answers to --version: the OpenCode release the
// fixtures were recorded from.
const Version = "1.18.33"

// Models is what the fake's `models` prints by default: a short list in
// OpenCode's shape, provider/model.
const Models = "opencode/big-pickle\nopencode/nemotron-3.5-lightning-free\n"

// Child reports whether this process was started as the fake: by a test that
// set EnvFixture, under the name name — a test binary that also plays another
// harness tells them apart by the name it was started as.
func Child(name string) bool {
	return filepath.Base(os.Args[0]) == name && os.Getenv(EnvFixture) != ""
}

// Main is the fake agent: --version, models, auth list, and the ACP server
// itself.
func Main() {
	args := os.Args[1:]
	switch {
	case len(args) == 1 && args[0] == "--version":
		os.Stdout.WriteString(firstNonEmpty(os.Getenv(EnvVersion), Version) + "\n")
	case len(args) == 1 && args[0] == "models":
		models := Models
		if f := os.Getenv(EnvModels); f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				os.Exit(2)
			}
			models = string(b)
		}
		os.Stdout.WriteString(models)
	case len(args) == 2 && args[0] == "auth" && args[1] == "list":
		os.Stdout.WriteString("0 credentials\n")
	default:
		play()
	}
}

// clientMsg is one message from the adapter.
type clientMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func play() {
	logf := openLog()
	defer logf.Close()
	log := func(kind string, v any) {
		b, _ := json.Marshal(map[string]any{kind: v})
		logf.Write(append(b, '\n'))
	}
	log("argv", os.Args[1:])
	env := map[string]any{}
	for _, name := range strings.Split(os.Getenv(EnvWatch), ",") {
		if v, ok := os.LookupEnv(name); ok && name != "" {
			env[name] = v
		}
	}
	log("env", env)
	mode := os.Getenv(EnvMode)

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
	if mode == "silent" {
		<-eof
		time.Sleep(time.Hour)
	}
	raw, err := os.ReadFile(os.Getenv(EnvFixture))
	if err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(2)
	}
	wait := 10 * time.Second
	if d, err := time.ParseDuration(os.Getenv(EnvWait)); err == nil {
		wait = d
	}
	out := bufio.NewWriter(os.Stdout)
	ids := map[string]string{} // recorded request id → the adapter's
	promptID := ""             // the recorded id of the prompt
	var early []clientMsg      // what the adapter sent before the fixture expected it
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
play:
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var ours struct {
			Msg *clientMsg `json:">"`
		}
		if json.Unmarshal([]byte(line), &ours) == nil && ours.Msg != nil {
			out.Flush()
			got, ok := take(*ours.Msg)
			if !ok {
				os.Stderr.WriteString("the adapter never sent " + firstNonEmpty(ours.Msg.Method, "an answer") + "\n")
				os.Exit(1)
			}
			if len(ours.Msg.ID) > 0 && ours.Msg.Method != "" {
				ids[string(ours.Msg.ID)] = string(got.ID)
			}
			if ours.Msg.Method == "session/prompt" {
				promptID = string(ours.Msg.ID)
			}
			continue
		}
		var theirs struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.Unmarshal([]byte(line), &theirs)
		if theirs.Method == "" && len(theirs.ID) > 0 {
			recorded := string(theirs.ID)
			if id, ok := ids[recorded]; ok {
				for _, end := range []string{",", "}"} {
					if old := `"id":` + recorded + end; strings.Contains(line, old) {
						line = strings.Replace(line, old, `"id":`+id+end, 1)
						break
					}
				}
			}
			if recorded == promptID && promptID != "" {
				out.Flush()
				switch mode {
				case "died":
					os.Stderr.WriteString("Error: something went badly wrong\n")
					os.Exit(1)
				case "exit":
					os.Exit(0)
				}
				if awaitGate(in, &early, mode == "deaf") {
					out.WriteString(`{"jsonrpc":"2.0","id":` + ids[promptID] + `,"result":{"stopReason":"cancelled"}}` + "\n")
					break play
				}
			}
		}
		out.WriteString(line + "\n")
	}
	out.Flush()
	select {
	case <-eof:
	case <-time.After(30 * time.Second): // never outlive a broken test by much
	}
	code, _ := strconv.Atoi(os.Getenv("ACP_TEST_EXIT"))
	os.Exit(code)
}

// awaitGate holds the prompt's answer until the test opens the gate, or until
// the adapter cancels the turn, which it reports. Whatever else the adapter
// sends meanwhile is kept for later.
func awaitGate(in <-chan clientMsg, early *[]clientMsg, deaf bool) bool {
	gate := os.Getenv(EnvGate)
	if gate == "" {
		return false
	}
	if at := os.Getenv(EnvAtGate); at != "" {
		os.WriteFile(at, nil, 0o600)
	}
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); {
		select {
		case m := <-in:
			if m.Method == "session/cancel" {
				if !deaf {
					return true
				}
				continue
			}
			*early = append(*early, m)
		case <-time.After(10 * time.Millisecond):
		}
		if _, err := os.Stat(gate); err == nil {
			return false
		}
	}
	os.Exit(1)
	return false
}

func openLog() *os.File {
	path := os.Getenv(EnvLog)
	if path == "" {
		path = os.DevNull
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(3)
	}
	return f
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
