package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/skkap/yad/internal/supervise"
)

// loginArgs runs the harness's own login. YAD passes the home and nothing
// else: it never sees the token, never reads the file the login writes, and
// has no credential store of its own for accounts (decision 0039).
var loginArgs = map[string][]string{
	"claude": {"auth", "login"},
	"codex":  {"login"},
}

// statusArgs asks the harness whether a home has a login, and is the only way
// YAD ever learns that. It spends no token and reaches no model: each reads
// the home it is pointed at and answers in about the time it takes to start.
var statusArgs = map[string][]string{
	"claude": {"auth", "status"},
	"codex":  {"login", "status"},
}

// codexLoggedOut is what `codex login status` prints when the home has no
// login. Matching prose is unwelcome, and the temptation to simplify this back
// to reading the exit status is the reason for the paragraph below.
//
// The exit status cannot carry the distinction alone: measured against
// codex-cli 0.147.0, codex exits 1 both for a home with no login and for a
// home whose config.toml it could not parse. Reading the second as the first
// would park a working account in needs-login over a typo.
//
// The direction of failure is the point. Requiring this phrase means a codex
// that rewords its answer fails towards "unknown" — the caller gets an error
// and leaves the account's state exactly where it was — and never towards
// "logged out". An ambiguous signal must not be what parks an account.
const codexLoggedOut = "Not logged in"

// statusTimeout caps one login check. It reads a file in the home and prints a
// line, so a second is already generous; the bound is here because this runs
// on a run's completion path and a harness that hangs must not hold the run's
// result behind it.
const statusTimeout = 10 * time.Second

// statusDrain is how long the reader gets once the leader has exited: what it
// printed is already in the pipe.
const statusDrain = 250 * time.Millisecond

// statusOutputCap bounds what a login check may print. The answer is one line
// or a small JSON object; a harness that decides to print its whole log must
// not be read into memory unbounded.
const statusOutputCap = 64 << 10

// suggest is the command an error tells the owner to run, as a shell would
// take it.
//
// The home variable is part of it. Neither harness derives its home from the
// working directory, so `codex login status` pasted into a terminal reads the
// owner's own default home and answers about a different account entirely —
// which is worse than no suggestion, because it looks like it worked. The
// path is quoted so a home with a space in it survives the paste.
func suggest(harness, binary, home string, args []string) string {
	cmd := binary + " " + strings.Join(args, " ")
	env := Env(harness, home)
	if len(env) == 0 {
		return cmd
	}
	name, value, _ := strings.Cut(env[0], "=")
	return fmt.Sprintf("%s=%q %s", name, value, cmd)
}

// CanLogIn says whether `yad account add` knows how to log this harness in.
func CanLogIn(harness string) bool {
	_, ok := loginArgs[harness]
	return ok
}

// Login runs the harness's own login inside the account's home with the owner
// at the terminal, and reports only whether the command succeeded.
//
// stdin, stdout and stderr are the owner's: the login draws its own prompts and
// opens its own browser, and YAD captures none of it, because what it prints on
// the way to a login is the one thing that must not end up in a YAD log.
func Login(ctx context.Context, harness, binary, home string, in io.Reader, out, errw io.Writer) error {
	args, ok := loginArgs[harness]
	if !ok {
		return fmt.Errorf("yad does not know how to log %s in — log in with %s's own command inside %s, then run `yad account list` to see it", harness, harness, home)
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	// Scrubbed as a run's child is (supervise.Scrub), then the home appended
	// last — os/exec keeps the last of a repeated name, so an owner whose own
	// shell exports CLAUDE_CONFIG_DIR still logs in to the account's home and
	// not to theirs.
	//
	// The scrub matters beyond tidiness: a login that inherited CLAUDECODE
	// from the session the owner typed the command in believes it is nested
	// inside another Claude Code, and one that inherited ANTHROPIC_API_KEY
	// would not be logging the subscription in at all. The child a run gets
	// sees neither, so neither does this.
	cmd.Env = append(supervise.Scrub(os.Environ(), nil), Env(harness, home)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errw
	// Codex refuses to start outside a directory it trusts (DEV-24), and the
	// home is one it made itself, so the login runs there rather than in
	// whatever directory the owner happened to type the command in.
	cmd.Dir = home
	return cmd.Run()
}

// LoggedIn asks the harness whether this home holds a login.
//
// Only the answer is kept. `claude auth status` also prints the account's
// e-mail address, its organisation and its plan; they are decoded into nothing
// and never logged, printed or reported — the label is the only thing about an
// account that leaves the machine.
func LoggedIn(ctx context.Context, harness, binary, home string) (bool, error) {
	args, ok := statusArgs[harness]
	if !ok {
		return false, fmt.Errorf("yad cannot check %s's login state — `yad account list` shows the home, and %s's own command says whether it is logged in", harness, harness)
	}
	// Through supervise, as every other harness child is, for two reasons
	// beyond consistency. It scrubs the environment the same way a run's
	// child is scrubbed — otherwise this check could answer "logged in" on
	// the strength of an ANTHROPIC_API_KEY in the daemon's environment that
	// the run itself is denied, leaving a useless account marked free while
	// every run on it failed. And it puts the child in its own process group
	// with a bounded read: this runs on the run-completion path under a
	// context nothing cancels, so a harness that hangs holding its stdout
	// would hold the run's result behind it for ever.
	ctx, cancel := context.WithTimeout(ctx, statusTimeout)
	defer cancel()
	proc, err := supervise.Start(ctx, supervise.Spec{Path: binary, Args: args, Dir: home, Env: Env(harness, home)})
	if err != nil {
		return false, fmt.Errorf("could not ask %s whether %s holds a login: %w", harness, home, err)
	}
	// The leader's own fate decides rather than EOF: a descendant that left
	// the group holding the pipe would never let the read finish
	// (internal/harness/detect.go makes the same argument for --version).
	read := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(proc.Stdout(), statusOutputCap))
		read <- b
	}()
	var stdout []byte
	select {
	case stdout = <-read:
	case <-proc.Done():
	case <-ctx.Done():
	}
	if stdout == nil {
		drain := time.NewTimer(statusDrain)
		select {
		case stdout = <-read:
		case <-drain.C:
		}
		drain.Stop()
	}
	proc.Stdout().Close()
	if stdout == nil {
		stdout = <-read // ReadAll returns what it read before the close
	}
	err = proc.Wait()

	// A check the deadline killed answered nothing, whatever it had printed
	// before it died. Without this a codex that writes "Not logged in" as the
	// first line of a longer answer and then hangs is read as a definitive
	// logged-out — the supervisor kills it, Wait returns an ExitError, and the
	// prefix below matches — and the account is parked on the strength of a
	// timeout. harness.detectOne tracks the same distinction for --version.
	if ctx.Err() != nil {
		return false, fmt.Errorf("%s did not answer whether %s holds a login within %s — run `%s` to see what it does", harness, home, statusTimeout, suggest(harness, binary, home, args))
	}

	switch harness {
	case "codex":
		if err == nil {
			return true, nil
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && strings.HasPrefix(strings.TrimSpace(string(stdout)), codexLoggedOut) {
			return false, nil
		}
		// Anything else is a question that did not get an answer, and an
		// unanswered question is not a "no". The message deliberately carries
		// none of what the command printed.
		return false, fmt.Errorf("could not read codex's login state for the home %s — run `%s` to see what it says", home, suggest(harness, binary, home, args))
	default:
		if err != nil {
			return false, fmt.Errorf("`%s` failed: %w", suggest(harness, binary, home, args), err)
		}
		// Only this one field is decoded. A version that stops answering in
		// JSON is an error rather than a guess: reporting an account free
		// because a status line could not be read would send runs to a login
		// that is not there.
		var s struct {
			LoggedIn *bool `json:"loggedIn"`
		}
		if err := json.Unmarshal(stdout, &s); err != nil || s.LoggedIn == nil {
			return false, fmt.Errorf("could not read %s's login state for the home %s — run `%s` to see what it says", harness, home, suggest(harness, binary, home, args))
		}
		return *s.LoggedIn, nil
	}
}
