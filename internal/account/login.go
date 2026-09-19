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
	// Appended last, and os/exec keeps the last of a repeated name: an owner
	// whose own shell exports CLAUDE_CONFIG_DIR still logs in to the account's
	// home and not to theirs.
	cmd.Env = append(os.Environ(), Env(harness, home)...)
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
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = append(os.Environ(), Env(harness, home)...)
	cmd.Dir = home
	stdout, err := cmd.Output()

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
		return false, fmt.Errorf("could not read codex's login state for the home %s — run `%s %v` there to see what it says", home, binary, args)
	default:
		if err != nil {
			return false, fmt.Errorf("`%s %v` failed in %s: %w", binary, args, home, err)
		}
		// Only this one field is decoded. A version that stops answering in
		// JSON is an error rather than a guess: reporting an account free
		// because a status line could not be read would send runs to a login
		// that is not there.
		var s struct {
			LoggedIn *bool `json:"loggedIn"`
		}
		if err := json.Unmarshal(stdout, &s); err != nil || s.LoggedIn == nil {
			return false, fmt.Errorf("could not read %s's login state for the home %s — run `%s %v` there to see what it says", harness, home, binary, args)
		}
		return *s.LoggedIn, nil
	}
}
