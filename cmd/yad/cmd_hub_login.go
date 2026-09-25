package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubapiclient"
)

const hubLoginUsage = "usage: yad hub login start <runner> <harness> [account] [--code -] | token <runner> <harness> <account> | status <runner> <login> | cancel <runner> <login>"

// loginPoll is how often a waiting `yad hub login` asks the hub how its login
// stands. The runner moves it only at its own syncs, seconds apart, so a
// second costs nothing a person would notice. A variable for the tests, whose
// hub and runner sync in milliseconds; nothing else assigns it.
var loginPoll = time.Second

// loginWait bounds each wait on the runner: for the link, and for the end. A
// runner syncing at all answers within a minute; the rest is its own
// deadlines, which end the login well inside this.
const loginWait = 5 * time.Minute

// cmdHubLogin is `yad hub login`: a hub login (decision 0055) through the
// service API, as a hub's UI would do it — a link to sign in at and the code
// pasted back, or a `claude setup-token` token.
func cmdHubLogin(ctx context.Context, g global, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(hubLoginUsage)
	}
	fs := flag.NewFlagSet("hub login "+args[0], flag.ContinueOnError)
	hf := addHubFlags(fs, g)
	code := fs.String("code", "", "with start: `-` reads the code from stdin without asking for it, for a script")
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return err
	}
	want := map[string][2]int{"start": {2, 3}, "token": {3, 3}, "status": {2, 2}, "cancel": {2, 2}}
	n, ok := want[args[0]]
	switch {
	case !ok:
		return fmt.Errorf("unknown hub login subcommand %q — %s", args[0], hubLoginUsage)
	case len(pos) < n[0] || len(pos) > n[1]:
		return errors.New(hubLoginUsage)
	case *code != "" && (*code != "-" || args[0] != "start"):
		// A code is short-lived, but the rule for secrets in argv is one
		// rule: every account on the machine can read them while it runs.
		return errors.New("--code takes only -, which reads the code from stdin; it belongs to start")
	}
	c, err := hf.client()
	if err != nil {
		return err
	}
	lc := loginCLI{c: c, hf: hf, stdout: stdout, stderr: stderr}
	switch args[0] {
	case "start":
		account := ""
		if len(pos) == 3 {
			account = pos[2]
		}
		return lc.start(ctx, pos[0], pos[1], account, *code == "-")
	case "token":
		return lc.token(ctx, pos[0], pos[1], pos[2])
	case "status":
		l, err := c.Login(ctx, pos[0], pos[1])
		if err != nil {
			return err
		}
		lc.describe(l)
		return nil
	default:
		l, err := c.CancelLogin(ctx, pos[0], pos[1])
		if err != nil {
			return err
		}
		if l.State.Terminal() {
			fmt.Fprintf(stdout, "login %s has ended %s\n", cleanLine(l.LoginID), l.State)
			return nil
		}
		fmt.Fprintf(stdout, "login %s is %s; runner %s ends it at its next sync — `%s` shows the end\n",
			cleanLine(l.LoginID), l.State, cleanLine(l.RunnerID), lc.statusCommand(l))
		return nil
	}
}

type loginCLI struct {
	c              *hubapiclient.Client
	hf             hubFlags
	stdout, stderr io.Writer
}

// start is the link way: the login started, its link printed once the runner
// has it, the code read and sent, and the end waited for.
func (lc loginCLI) start(ctx context.Context, runnerID, harness, label string, quiet bool) error {
	l, err := lc.c.StartLogin(ctx, runnerID, hubapi.LoginRequest{Harness: harness, Account: label})
	if err != nil {
		return err
	}
	fmt.Fprintf(lc.stderr, "login %s: %s on runner %s hears of it at its next sync, and reports the link to sign in at\n",
		cleanLine(l.LoginID), lc.what(l), cleanLine(l.RunnerID))
	l, err = lc.await(ctx, l, func(l hubapi.Login) bool { return l.State == hubapi.LoginState(v1.LoginWaiting) && l.URL != "" })
	if err != nil {
		return err
	}
	if l.State.Terminal() {
		return lc.ended(l)
	}
	// The link alone on stdout, so a script can hand it on; it is the
	// harness's output, and data.
	fmt.Fprintln(lc.stdout, cleanLine(l.URL))
	if !quiet {
		fmt.Fprint(lc.stderr, "open the link, sign in, and paste the code you are given here: ")
	}
	code, err := readLine(ctx, stdin)
	if err != nil {
		lc.giveUp(l)
		return err
	}
	if code == "" {
		lc.giveUp(l)
		return fmt.Errorf("no code was read, so the login is cancelled — run `%s` again and paste the code", lc.hf.command([]string{"hub", "login", "start"}, runnerID, harness, label))
	}
	if l, err = lc.c.SendLoginCode(ctx, l.RunnerID, l.LoginID, code); err != nil {
		return err
	}
	fmt.Fprintf(lc.stderr, "code sent; runner %s hands it to %s's login at its next sync and checks whether it took\n", cleanLine(l.RunnerID), cleanLine(l.Harness))
	return lc.finish(ctx, l)
}

// token is the token way: read from stdin, never from argv, sent once.
func (lc loginCLI) token(ctx context.Context, runnerID, harness, label string) error {
	b, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
	if err != nil {
		return fmt.Errorf("read the token from stdin: %w", err)
	}
	tok := strings.TrimSpace(string(b))
	if err := account.CheckToken(tok); err != nil {
		return fmt.Errorf("%s — pipe it in: `%s`", err, lc.hf.command([]string{"hub", "login", "token"}, runnerID, harness, label))
	}
	l, err := lc.c.StartLogin(ctx, runnerID, hubapi.LoginRequest{Harness: harness, Account: label, Token: tok})
	if err != nil {
		return err
	}
	fmt.Fprintf(lc.stderr, "login %s: runner %s stores the token for %s at its next sync; the hub keeps it until then, and no longer\n",
		cleanLine(l.LoginID), cleanLine(l.RunnerID), lc.what(l))
	return lc.finish(ctx, l)
}

// finish waits for the end and says what it was: exit 0 only for a login
// that took.
func (lc loginCLI) finish(ctx context.Context, l hubapi.Login) error {
	l, err := lc.await(ctx, l, func(hubapi.Login) bool { return false })
	if err != nil {
		return err
	}
	if l.State != hubapi.LoginState(v1.LoginSucceeded) {
		return lc.ended(l)
	}
	fmt.Fprintf(lc.stdout, "%s on runner %s is logged in, and takes runs\n", lc.what(l), cleanLine(l.RunnerID))
	return nil
}

// await polls until done says so or the login ends. Interrupted, it cancels
// the login on its way out: nobody is left to paste a code into it.
func (lc loginCLI) await(ctx context.Context, l hubapi.Login, done func(hubapi.Login) bool) (hubapi.Login, error) {
	deadline := time.Now().Add(loginWait)
	for !done(l) && !l.State.Terminal() {
		if time.Now().After(deadline) {
			return l, fmt.Errorf("runner %s has not moved login %s past %s in %s — is it syncing? `%s` shows where it stands",
				cleanLine(l.RunnerID), cleanLine(l.LoginID), l.State, loginWait, lc.statusCommand(l))
		}
		select {
		case <-ctx.Done():
			lc.giveUp(l)
			return l, ctx.Err()
		case <-time.After(loginPoll):
		}
		next, err := lc.c.Login(ctx, l.RunnerID, l.LoginID)
		if err != nil {
			if ctx.Err() != nil {
				lc.giveUp(l)
			}
			return l, err
		}
		l = next
	}
	return l, nil
}

// giveUp cancels a login this command will not finish, on a context of its
// own: the command's may be the one that just ended.
func (lc loginCLI) giveUp(l hubapi.Login) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := lc.c.CancelLogin(ctx, l.RunnerID, l.LoginID); err == nil {
		fmt.Fprintf(lc.stderr, "\nlogin %s is cancelled\n", cleanLine(l.LoginID))
	}
}

// ended is a login that did not take, as the error the command exits with.
func (lc loginCLI) ended(l hubapi.Login) error {
	msg := fmt.Sprintf("login %s for %s on runner %s ended %s", cleanLine(l.LoginID), lc.what(l), cleanLine(l.RunnerID), l.State)
	if l.Error != "" {
		// The runner's words, with their own next action.
		msg += ": " + cleanLine(l.Error)
	}
	return errors.New(msg)
}

func (lc loginCLI) describe(l hubapi.Login) {
	fmt.Fprintf(lc.stdout, "login %s — %s on runner %s, by %s: %s\n", cleanLine(l.LoginID), lc.what(l), cleanLine(l.RunnerID), l.Method, l.State)
	if l.URL != "" {
		fmt.Fprintf(lc.stdout, "sign in at %s\n", cleanLine(l.URL))
	}
	if l.UserCode != "" {
		fmt.Fprintf(lc.stdout, "and enter the code %s there\n", cleanLine(l.UserCode))
	}
	if l.CodeSent {
		fmt.Fprintln(lc.stdout, "a code is sent, and the runner takes it at its next sync")
	}
	if l.CancelRequestedAt != nil {
		fmt.Fprintln(lc.stdout, "a cancel is asked for; the runner ends it at its next sync")
	}
	if l.Error != "" {
		fmt.Fprintln(lc.stdout, cleanLine(l.Error))
	}
}

// what names the login's account for a person.
func (lc loginCLI) what(l hubapi.Login) string {
	if l.Account == "" {
		return cleanLine(l.Harness) + "'s own login"
	}
	return fmt.Sprintf("%s account %q", cleanLine(l.Harness), cleanLine(l.Account))
}

func (lc loginCLI) statusCommand(l hubapi.Login) string {
	return lc.hf.command([]string{"hub", "login", "status"}, l.RunnerID, l.LoginID)
}

// readLine is one line of r, trimmed, or ctx's end: a person may walk away
// from the prompt, and Ctrl-C must still end the command.
func readLine(ctx context.Context, r io.Reader) (string, error) {
	type line struct {
		s   string
		err error
	}
	got := make(chan line, 1)
	go func() {
		s, err := bufio.NewReader(io.LimitReader(r, 64<<10)).ReadString('\n')
		if errors.Is(err, io.EOF) {
			err = nil
		}
		got <- line{strings.TrimSpace(s), err}
	}()
	select {
	case l := <-got:
		return l.s, l.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// command is a `yad hub` command acting on the hub and admin token this one
// was given, positional arguments last: a bare one would ask the default hub
// about a login it does not have.
func (f hubFlags) command(verb []string, pos ...string) string {
	args := slices.Clone(verb)
	if u := *f.url; u != f.defURL {
		if config.RedactURL(u) != u {
			u = "<the same --hub URL>"
		}
		args = append(args, "--hub", u)
	}
	if *f.tokenFile != f.defTokenFile {
		args = append(args, "--token-file", *f.tokenFile)
	}
	for _, p := range pos {
		if p != "" {
			args = append(args, p)
		}
	}
	return f.paths.Command(args...)
}
