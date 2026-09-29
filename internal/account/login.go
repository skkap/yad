package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/skkap/yad/internal/shellword"
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
//
// OpenCode has no login check of its own: `opencode auth list` names the
// providers it holds credentials for, and it runs on OpenCode Zen's free
// models with none. What decides whether it can take a run is whether it
// offers any model, so its check is the model list, which also spends no
// token (decision 0072).
var statusArgs = map[string][]string{
	"claude":   {"auth", "status"},
	"codex":    {"login", "status"},
	"opencode": {"models"},
}

// manualLogin is the login command of a harness whose login yad checks but
// cannot run itself — `yad account add` and a hub login need a home per
// account, which OpenCode does not have here (decision 0072) — for the
// owner to run at the machine.
var manualLogin = map[string][]string{
	"opencode": {"auth", "login"},
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
	// The binary is quoted for the same reason the home is: it is a path yad
	// resolved, not a word the reader typed, and a space or a $ in it makes
	// the line mean something else when pasted.
	cmd := shellword.Command(append([]string{binary}, args...)...)
	if home == "" {
		return cmd
	}
	// Every variable an account sets to its home, and never its token.
	var prefix string
	for _, v := range HomeVars(harness) {
		prefix += v + "=" + shellword.Quote(home) + " "
	}
	return prefix + cmd
}

// LoginArgs is the harness's own login command after its binary, or nil for a
// harness yad cannot log in.
func LoginArgs(harness string) []string { return slices.Clone(loginArgs[harness]) }

// StatusArgs is the harness's own login check after its binary, or nil.
func StatusArgs(harness string) []string { return slices.Clone(statusArgs[harness]) }

// LoginCommandArgs is the harness's own login command after its binary, for
// an owner to run: the one yad runs, or the one it cannot.
func LoginCommandArgs(harness string) []string {
	if args, ok := loginArgs[harness]; ok {
		return slices.Clone(args)
	}
	return slices.Clone(manualLogin[harness])
}

// ChecksLogin says whether yad can ask the harness if a home holds a login.
func ChecksLogin(harness string) bool {
	_, ok := statusArgs[harness]
	return ok
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
//
// extra goes after the harness's own login arguments: Codex's --device-auth,
// for a machine with no browser.
func Login(ctx context.Context, harness, binary, home string, in io.Reader, out, errw io.Writer, extra ...string) error {
	args, ok := loginArgs[harness]
	if !ok {
		return fmt.Errorf("yad does not know how to log %s in — log in with %s's own command inside %s, then run `yad account list` to see it", harness, harness, home)
	}
	cmd := exec.CommandContext(ctx, binary, append(slices.Clone(args), extra...)...)
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
	cmd.Env = append(supervise.Scrub(os.Environ(), nil), LoginEnv(harness, home)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, errw
	// Codex refuses to start outside a directory it trusts (DEV-24), and the
	// home is one it made itself, so the login runs there rather than in
	// whatever directory the owner happened to type the command in.
	cmd.Dir = home
	return cmd.Run()
}

// LoginEnv is what points a harness's own login at an account's home: the home
// variable, and never the account's stored token (decision 0054). A token in
// the environment would outrank the login being made and make the harness's
// check say yes to it, so a login beside a token is made, and checked, as if
// the token were not there — while the token stays in its file for the runs
// that are still using it, until the new login is confirmed.
func LoginEnv(harness, home string) []string {
	v, ok := homeVar[harness]
	if !ok || home == "" {
		return nil
	}
	env := []string{v + "=" + home}
	if sv, ok := storageVar[harness]; ok {
		env = append(env, sv+"="+home)
	}
	return env
}

// LoggedIn asks the harness whether this home holds a login: the one a run
// would use, which for a token account is its token.
func LoggedIn(ctx context.Context, harness, binary, home string) (bool, error) {
	return loggedIn(ctx, harness, binary, home, Env(harness, home))
}

// OwnLogin asks the harness whether this home holds a login of its own,
// whatever token is stored beside it: whether a login just made there took.
// Claude's check says yes to any token it is handed, so asking with the token
// in place would call every login beside one a success.
func OwnLogin(ctx context.Context, harness, binary, home string) (bool, error) {
	return loggedIn(ctx, harness, binary, home, LoginEnv(harness, home))
}

// loggedIn is the check itself, with the environment it runs in.
//
// Only the answer is kept. `claude auth status` also prints the account's
// e-mail address, its organisation and its plan; they are decoded into nothing
// and never logged, printed or reported — the label is the only thing about an
// account that leaves the machine.
func loggedIn(ctx context.Context, harness, binary, home string, env []string) (bool, error) {
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
	// An empty home is the harness's own default one, which a run with no
	// account uses: the check then runs where such a run's environment points
	// it, from the user's home directory.
	dir := home
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	proc, err := supervise.Start(ctx, supervise.Spec{Path: binary, Args: args, Dir: dir, Env: env})
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
	case "opencode":
		// Logged in is offering a model: a line shaped provider/model. A
		// failing list is a question that got no answer, never a "no".
		if err != nil {
			return false, fmt.Errorf("could not read %s's models for the home %s — run `%s` to see what it says", harness, home, suggest(harness, binary, home, args))
		}
		for _, line := range strings.Split(string(stdout), "\n") {
			if f := strings.TrimSpace(line); strings.Contains(f, "/") && !strings.ContainsAny(f, " \t") {
				return true, nil
			}
		}
		return false, nil
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
		// Only this one field is decoded, and it decides whatever the exit
		// status: measured on claude 2.1.281, a home with no login answers
		// {"loggedIn": false} and exits 1, so an exit status read first would
		// turn every "no" into "could not tell", and an account whose login
		// expired would never be parked. A version that stops answering in
		// JSON is an error rather than a guess: reporting an account free
		// because a status line could not be read would send runs to a login
		// that is not there.
		var s struct {
			LoggedIn *bool `json:"loggedIn"`
		}
		if jerr := json.Unmarshal(stdout, &s); jerr != nil || s.LoggedIn == nil {
			if err != nil {
				return false, fmt.Errorf("`%s` failed: %w", suggest(harness, binary, home, args), err)
			}
			return false, fmt.Errorf("could not read %s's login state for the home %s — run `%s` to see what it says", harness, home, suggest(harness, binary, home, args))
		}
		if *s.LoggedIn && err != nil {
			// "Yes" with a failure is not an answer to trust either way.
			return false, fmt.Errorf("`%s` failed: %w", suggest(harness, binary, home, args), err)
		}
		return *s.LoggedIn, nil
	}
}
