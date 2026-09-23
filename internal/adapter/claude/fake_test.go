package claude

import (
	"bufio"
	"encoding/json"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The test binary doubles as claude. Re-executed with CLAUDE_TEST_FIXTURE set,
// it plays a recorded stream to stdout, reading stdin the way claude does, so
// every test drives the real adapter against a real process — pipes, process
// group, exit status and all — without a token.
//
// It plays the fixture line by line and blocks where claude would have been
// waiting on its input:
//   - a replayed user frame is played only once a user frame has arrived — the
//     first is the instruction, the rest are steers;
//   - a control_response is played only once a control_request has arrived,
//     and answers it by its request id;
//   - a result with more replayed user frames after it is played only once the
//     next user frame has arrived: in the recording, that steer was already
//     queued when the result came, which is why Claude answered it.
//
// The fixture's session id is rewritten to the one on the command line, as
// claude would have used it, unless CLAUDE_TEST_KEEP_SESSION is set.
//
// CLAUDE_TEST_MODE picks the ending: "" waits for stdin to close and exits
// with CLAUDE_TEST_EXIT; "died" exits 1 at once with something on stderr;
// "linger" ignores the closed stdin and SIGTERM, as a wedged claude would;
// "mute" closes stdout and then lingers the same way; "die-on-interrupt"
// keeps working until an interrupt arrives, then exits without answering it;
// "deaf" never takes the interrupt at all, and dies only by signal.
func fakeClaude() {
	logf := openLog()
	defer logf.Close()
	log := func(kind string, v any) {
		b, _ := json.Marshal(map[string]any{kind: v})
		logf.Write(append(b, '\n'))
	}
	args := os.Args[1:]
	log("argv", args)
	session := ""
	warned := false
	for i, a := range args {
		if (a == "--session-id" || a == "--resume") && i+1 < len(args) {
			session = args[i+1]
		}
		// Claude 2.1.280 on an effort it does not know: a warning while it
		// parses its arguments, then the turn at its default, exit 0.
		if a == "--effort" && i+1 < len(args) && !slices.Contains([]string{"low", "medium", "high", "xhigh", "max"}, args[i+1]) {
			os.Stderr.WriteString("Warning: Unknown --effort value '" + args[i+1] + "' — ignoring it and using the default effort. Valid values: low, medium, high, xhigh, max.\n")
			warned = true
		}
		if a == "--append-system-prompt-file" && i+1 < len(args) {
			b, err := os.ReadFile(args[i+1])
			if err != nil {
				log("context_error", err.Error())
			}
			log("context", string(b))
		}
	}

	users := make(chan struct{}, 64)
	controls := make(chan string, 64)
	eof := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(nil, 64<<20)
		for sc.Scan() {
			log("stdin", sc.Text())
			var f struct {
				Type      string `json:"type"`
				RequestID string `json:"request_id"`
			}
			json.Unmarshal(sc.Bytes(), &f)
			switch f.Type {
			case "user":
				users <- struct{}{}
			case "control_request":
				controls <- f.RequestID
			}
		}
		close(eof)
	}()

	raw, err := os.ReadFile(os.Getenv("CLAUDE_TEST_FIXTURE"))
	if err != nil {
		os.Stderr.WriteString(err.Error())
		os.Exit(2)
	}
	body := string(raw)
	if recorded := firstSession(body); recorded != "" && session != "" && os.Getenv("CLAUDE_TEST_KEEP_SESSION") == "" {
		body = strings.ReplaceAll(body, recorded, session)
	}
	out := bufio.NewWriter(os.Stdout)
	lines := strings.SplitAfter(body, "\n")
	endsInResult := false
	replaysAfter := make([]int, len(lines)+1)
	for i := len(lines) - 1; i >= 0; i-- {
		replaysAfter[i] = replaysAfter[i+1]
		if strings.Contains(lines[i], `"isReplay":true`) {
			replaysAfter[i]++
		}
	}
	credit := 0 // user frames taken early, for replays still to come
	for i, line := range lines {
		if line == "" {
			continue
		}
		var f struct {
			Type     string `json:"type"`
			IsReplay bool   `json:"isReplay"`
			Response *struct {
				RequestID string `json:"request_id"`
			} `json:"response"`
		}
		if json.Unmarshal([]byte(line), &f) == nil {
			switch {
			case f.Type == "user" && f.IsReplay:
				out.Flush()
				if credit > 0 {
					credit--
				} else if !await(users) {
					os.Exit(1)
				}
			case f.Type == "result" && replaysAfter[i] > credit:
				out.Flush()
				if !await(users) {
					os.Exit(1)
				}
				credit++
			case f.Type == "control_response" && f.Response != nil:
				out.Flush()
				if os.Getenv("CLAUDE_TEST_MODE") == "deaf" {
					// Never answers the interrupt; SIGTERM still ends it.
					time.Sleep(time.Hour)
				}
				id, ok := awaitString(controls)
				if !ok {
					os.Exit(1)
				}
				line = strings.Replace(line, strconv.Quote(f.Response.RequestID), strconv.Quote(id), 1)
			}
		}
		// Having warned, it plays its startup and then holds its first frame
		// of work for a while, as the real one spends a model round trip
		// before it: long enough that an adapter reading the warning has
		// stopped it by then, and one that did not is seen working.
		if warned && f.Type != "system" && !(f.Type == "user" && f.IsReplay) {
			out.Flush()
			time.Sleep(3 * time.Second)
			warned = false
		}
		endsInResult = isResult(line)
		out.WriteString(line)
		log("played", f.Type)
	}
	out.Flush()

	switch os.Getenv("CLAUDE_TEST_MODE") {
	case "died":
		os.Stderr.WriteString("Error: something went badly wrong\n")
		os.Exit(1)
	case "linger":
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
	case "die-on-interrupt":
		// Still working, until an interrupt arrives — then gone, unanswered.
		awaitString(controls)
		os.Exit(1)
	case "mute":
		// Output over, process not: stdout closed, stdin ignored.
		os.Stdout.Close()
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(time.Hour)
	}
	// Claude waits for its input to close only once it has answered; a stream
	// cut short is a claude that has already gone, and one that failed before
	// reading its instruction (a resume with no transcript) never waits at all.
	if endsInResult && replaysAfter[0] > 0 {
		select {
		case <-eof:
		case <-time.After(30 * time.Second): // never outlive a broken test by much
		}
	}
	code, _ := strconv.Atoi(os.Getenv("CLAUDE_TEST_EXIT"))
	os.Exit(code)
}

func isResult(line string) bool {
	var f struct {
		Type string `json:"type"`
	}
	return json.Unmarshal([]byte(line), &f) == nil && f.Type == "result"
}

func openLog() *os.File {
	path := os.Getenv("CLAUDE_TEST_LOG")
	if path == "" {
		path = os.DevNull
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(3)
	}
	return f
}

func await(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	case <-time.After(10 * time.Second):
		return false
	}
}

func awaitString(c <-chan string) (string, bool) {
	select {
	case s := <-c:
		return s, true
	case <-time.After(10 * time.Second):
		return "", false
	}
}

func firstSession(body string) string {
	for _, line := range strings.Split(body, "\n") {
		var f struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal([]byte(line), &f) == nil && f.SessionID != "" {
			return f.SessionID
		}
	}
	return ""
}
