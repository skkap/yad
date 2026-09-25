package account

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/shellword"
)

// The test binary doubles as the harness. Re-executed with
// ACCOUNT_TEST_HARNESS set — deliberately not YAD_-prefixed, because the
// login check scrubs the environment exactly as a run's child does and
// supervise.Scrub removes YAD_*, which would leave the re-executed binary
// running the test suite again instead of playing the harness — it answers `auth login`/`auth status` as claude
// does and `login`/`login status` as codex does, keeping its "credential" in
// the home it was pointed at — which is the whole of what these tests are
// about: that the home YAD passes is the home the harness reads and writes.
//
// ACCOUNT_TEST_LOGIN_FAILS makes the login exit non-zero having written
// nothing, which is the owner walking away from it.
func TestMain(m *testing.M) {
	if h := os.Getenv("ACCOUNT_TEST_HARNESS"); h != "" {
		os.Exit(fakeHarness(h, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// credentialFile is where the fake harness keeps its pretend credential. Its
// contents never matter: nothing in YAD ever reads them.
const credentialFile = ".credentials.json"

func fakeHarness(id string, args []string) int {
	home := os.Getenv("CLAUDE_CONFIG_DIR")
	if id == "codex" {
		home = os.Getenv("CODEX_HOME")
	}
	if home == "" {
		fmt.Fprintln(os.Stderr, "no home in the environment")
		return 3
	}
	cmd := strings.Join(args, " ")
	loggedIn := func() bool {
		_, err := os.Stat(filepath.Join(home, credentialFile))
		return err == nil
	}
	switch cmd {
	case "auth login", "login":
		if os.Getenv("ACCOUNT_TEST_LOGIN_FAILS") != "" {
			fmt.Fprintln(os.Stderr, "login cancelled")
			return 1
		}
		// A real login prints a URL and a code, and this is the closest the
		// fake gets: something on stdout that must never reach a YAD log.
		fmt.Println("paste this code: TOP-SECRET-LOGIN-CODE")
		os.WriteFile(filepath.Join(home, credentialFile), []byte(`{"token":"sk-not-a-real-token"}`), 0o600)
		return 0
	case "auth status":
		if os.Getenv("ACCOUNT_TEST_BAD_STATUS") != "" {
			// A claude whose status stopped being JSON.
			fmt.Println("Logged in as owner@example.com")
			return 0
		}
		// Claude answers in JSON, with the account's e-mail and plan beside
		// the one field YAD reads.
		in := loggedIn()
		// As claude does: any CLAUDE_CODE_OAUTH_TOKEN is a login to its
		// check, valid or not.
		if os.Getenv("ACCOUNT_TEST_TOKEN_IS_LOGIN") != "" && os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
			in = true
		}
		b, _ := json.Marshal(map[string]any{
			"loggedIn": in, "email": "owner@example.com", "subscriptionType": "max",
			"configDirectory": home,
		})
		fmt.Println(string(b))
		// As claude 2.1.281 does: the answer is in the JSON, and a "no"
		// exits 1 as well.
		if !in {
			return 1
		}
		return 0
	case "login status":
		// Prints the logged-out phrase and then hangs, so the deadline kills
		// it: the exit the supervisor produces is an *exec.ExitError and the
		// output carries the prefix, which together used to be read as a
		// definitive answer.
		if os.Getenv("ACCOUNT_TEST_HANG_AFTER_PHRASE") != "" {
			fmt.Println(codexLoggedOut + " — and now this hangs")
			// Not select{}: with no other goroutine Go turns that into an
			// immediate deadlock panic, and the child would exit at once
			// rather than hang.
			time.Sleep(5 * time.Minute)
			return 1
		}
		// codex exits 1 both for a missing login and for a home it could not
		// read; only what it printed tells the two apart.
		if os.Getenv("ACCOUNT_TEST_BROKEN_HOME") != "" {
			fmt.Println("Error loading configuration: " + home + "/config.toml:1:5: key with no value")
			return 1
		}
		if loggedIn() {
			fmt.Println("Logged in using ChatGPT")
			return 0
		}
		fmt.Println("Not logged in")
		return 1
	}
	fmt.Fprintf(os.Stderr, "unexpected argv %q\n", cmd)
	return 2
}

// self is this test binary, standing in for the harness.
func self(t *testing.T, id string) string {
	t.Helper()
	t.Setenv("ACCOUNT_TEST_HARNESS", id)
	// Without this each re-executed child sleeps a second at exit under -race.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestLoginAndItsCheck(t *testing.T) {
	for _, id := range []string{"claude", "codex"} {
		t.Run(id, func(t *testing.T) {
			bin := self(t, id)
			data := t.TempDir()
			home, err := Ensure(data, id, "work")
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()

			in, err := LoggedIn(ctx, id, bin, home)
			if err != nil {
				t.Fatal(err)
			}
			if in {
				t.Fatal("a home nobody has logged into reports a login")
			}
			var out strings.Builder
			if err := Login(ctx, id, bin, home, strings.NewReader(""), &out, &out); err != nil {
				t.Fatal(err)
			}
			if in, err = LoggedIn(ctx, id, bin, home); err != nil || !in {
				t.Fatalf("after the login: in=%v err=%v", in, err)
			}
			// The login wrote into the home it was given, and nowhere else.
			if _, err := os.Stat(filepath.Join(home, credentialFile)); err != nil {
				t.Errorf("the login did not write into %s: %v", home, err)
			}
		})
	}
}

// A login the owner abandons leaves a home with no login in it. That is a
// state to report, not a success.
func TestAbandonedLoginLeavesTheHomeWithoutOne(t *testing.T) {
	bin := self(t, "claude")
	t.Setenv("ACCOUNT_TEST_LOGIN_FAILS", "1")
	data := t.TempDir()
	home, err := Ensure(data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var out strings.Builder
	if err := Login(ctx, "claude", bin, home, strings.NewReader(""), &out, &out); err == nil {
		t.Error("an abandoned login reported success")
	}
	in, err := LoggedIn(ctx, "claude", bin, home)
	if err != nil {
		t.Fatal(err)
	}
	if in {
		t.Error("a home with no credential reports a login")
	}
}

// Two homes, one login: the harness reads the home it is given, which is what
// per-account isolation rests on. Verified against the real claude on macOS
// (DEV-24) and on Linux (DEV-26); this holds the wiring that depends on it.
func TestOneHomesLoginDoesNotReachAnother(t *testing.T) {
	bin := self(t, "claude")
	data := t.TempDir()
	ctx := context.Background()
	first, err := Ensure(data, "claude", "personal")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Ensure(data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := Login(ctx, "claude", bin, first, strings.NewReader(""), &out, &out); err != nil {
		t.Fatal(err)
	}
	if in, err := LoggedIn(ctx, "claude", bin, first); err != nil || !in {
		t.Fatalf("the home that was logged in reports in=%v err=%v", in, err)
	}
	if in, err := LoggedIn(ctx, "claude", bin, second); err != nil || in {
		t.Fatalf("the other home saw the first home's login: in=%v err=%v", in, err)
	}
}

// What a login prints on the way to a login — the code a person pastes into a
// browser — reaches the caller's own writer and nothing else. YAD keeps none of
// it: not in a return value, and not in the error when the login fails.
func TestLoginKeepsNothingItPrinted(t *testing.T) {
	t.Run("a login that took", func(t *testing.T) {
		bin := self(t, "claude")
		home, err := Ensure(t.TempDir(), "claude", "work")
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		if err := Login(context.Background(), "claude", bin, home, strings.NewReader(""), &out, &out); err != nil {
			t.Fatal(err)
		}
		// The fake prints a code, as a real login does. If it stops, the
		// assertion below proves nothing, so this is checked first.
		if !strings.Contains(out.String(), "TOP-SECRET-LOGIN-CODE") {
			t.Fatal("the fake login printed no code; this test would pass vacuously")
		}
		// Login returns only whether the command succeeded. There is no other
		// value for what it printed to travel in.
		if _, err := LoggedIn(context.Background(), "claude", bin, home); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a login that failed", func(t *testing.T) {
		bin := self(t, "claude")
		t.Setenv("ACCOUNT_TEST_LOGIN_FAILS", "1")
		home, err := Ensure(t.TempDir(), "claude", "work")
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		err = Login(context.Background(), "claude", bin, home, strings.NewReader(""), &out, &out)
		if err == nil {
			t.Fatal("an abandoned login reported success")
		}
		// An exec error is an exit status, never the child's output.
		for _, leak := range []string{"TOP-SECRET-LOGIN-CODE", "login cancelled"} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("the error carries what the login printed (%q): %v", leak, err)
			}
		}
	})

}

func TestUnknownHarnessSaysWhatToDoInstead(t *testing.T) {
	ctx := context.Background()
	if _, err := LoggedIn(ctx, "gemini", "/nonexistent", t.TempDir()); err == nil {
		t.Error("a harness with no login command reported a state")
	}
	err := Login(ctx, "gemini", "/nonexistent", t.TempDir(), strings.NewReader(""), os.Stdout, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "gemini") {
		t.Errorf("the refusal does not name the harness: %v", err)
	}
}

// An unanswered question is not a "no". A login check that fails for its own
// reasons must report an error, so the caller leaves the account's state where
// it is rather than parking a working account.
func TestALoginCheckThatCannotAnswerIsAnError(t *testing.T) {
	t.Run("codex cannot read the home", func(t *testing.T) {
		bin := self(t, "codex")
		t.Setenv("ACCOUNT_TEST_BROKEN_HOME", "1")
		data := t.TempDir()
		home, err := Ensure(data, "codex", "work")
		if err != nil {
			t.Fatal(err)
		}
		// The login is there; only the check is broken.
		if err := os.WriteFile(filepath.Join(home, credentialFile), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		in, err := LoggedIn(context.Background(), "codex", bin, home)
		if err == nil {
			t.Fatalf("a check that could not answer reported in=%v and no error", in)
		}
		if in {
			t.Error("an error answer also said the home was logged in")
		}
		// Whatever it printed stays where it was printed.
		if strings.Contains(err.Error(), "key with no value") {
			t.Errorf("the error quotes the command's output: %v", err)
		}
	})

	t.Run("the binary is not there", func(t *testing.T) {
		for _, id := range []string{"claude", "codex"} {
			in, err := LoggedIn(context.Background(), id, filepath.Join(t.TempDir(), "gone"), t.TempDir())
			if err == nil || in {
				t.Errorf("%s: in=%v err=%v, want an error", id, in, err)
			}
		}
	})

	t.Run("claude stops answering in json", func(t *testing.T) {
		bin := self(t, "claude")
		t.Setenv("ACCOUNT_TEST_BAD_STATUS", "1")
		data := t.TempDir()
		home, err := Ensure(data, "claude", "work")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, credentialFile), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		in, err := LoggedIn(context.Background(), "claude", bin, home)
		if err == nil {
			t.Fatalf("a status that is not JSON was read as an answer: in=%v", in)
		}
		if in {
			t.Error("an error answer also said the home was logged in")
		}
		if strings.Contains(err.Error(), "owner@example.com") {
			t.Errorf("the error quotes what the command printed: %v", err)
		}
	})
}

// A check its own deadline killed answered nothing, whatever it printed first.
//
// This is the narrow case the phrase match opened: the supervisor kills the
// child, Wait returns an *exec.ExitError, and the output already begins with
// the logged-out phrase — so without the deadline check the account would be
// parked on the strength of a timeout, which is what the rest of this file
// exists to prevent.
func TestATimedOutCheckIsNotAnAnswer(t *testing.T) {
	bin := self(t, "codex")
	t.Setenv("ACCOUNT_TEST_HANG_AFTER_PHRASE", "1")
	data := t.TempDir()
	home, err := Ensure(data, "codex", "work")
	if err != nil {
		t.Fatal(err)
	}
	// The login is there; only the check cannot finish.
	if err := os.WriteFile(filepath.Join(home, credentialFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A deadline of the test's own, so this does not wait out statusTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	in, err := LoggedIn(ctx, "codex", bin, home)
	if err == nil {
		t.Fatalf("a check killed by its deadline was read as an answer: in=%v", in)
	}
	if in {
		t.Error("an error answer also said the home was logged in")
	}
	// The message names a command the owner can actually run — including the
	// home variable, without which the paste reads the owner's own default
	// home and answers about a different account.
	// Single-quoted, not Go-quoted: a shell expands $ inside double quotes.
	want := "CODEX_HOME=" + shellword.Quote(home) + " " + shellword.Quote(bin) + " login status"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("the next action is not the command yad ran:\n  want to contain: %s\n  got: %v", want, err)
	}
}

// A home a shell would rewrite still comes back as the path yad used. The
// next-action text is meant to be pasted, and inside double quotes a shell
// expands $, so Go's %q was not enough.
func TestTheSuggestedCommandSurvivesAShell(t *testing.T) {
	for _, home := range []string{
		"/tmp/yad/$USER/home",
		"/tmp/yad/with space/home",
		"/tmp/yad/back`tick/home",
		"/tmp/yad/it's/home",
	} {
		got := suggest("codex", "/bin/codex", home, []string{"login", "status"})
		// What a POSIX shell would produce for the value, reconstructed the
		// way sh unquotes it: everything between single quotes is literal,
		// and '\'' is the one escape.
		want := "CODEX_HOME='" + strings.ReplaceAll(home, "'", `'\''`) + "' /bin/codex login status"
		if got != want {
			t.Errorf("suggest(%q) = %s, want %s", home, got, want)
		}
		if strings.Contains(got, `="`) {
			t.Errorf("suggest(%q) double-quoted the home, so a shell would expand it: %s", home, got)
		}
	}
}

// The next-action command is read back by a real shell, not by this package's
// idea of what a shell does.
//
// A Go test asserting Go's notion of quoting agrees with itself, which is how
// the earlier version of this passed while emitting ”' where POSIX wants
// '\”. Here sh parses the line and prints what it resolved, so the assertion
// is about the thing an operator will actually paste. No token, no network.
func TestTheSuggestedCommandIsReadBackByARealShell(t *testing.T) {
	const binary = "/opt/co dex/bin/codex"
	for _, home := range []string{
		"/tmp/yad/$USER/home",
		"/tmp/yad/with space/home",
		"/tmp/yad/it's/home",
		"/tmp/yad/back`tick/home",
		`/tmp/yad/back\slash/home`,
	} {
		line := suggest("codex", binary, home, []string{"login", "status"})
		// Swap the harness for a probe that prints what sh resolved, leaving
		// the quoting of the environment assignment exactly as suggest wrote it.
		probe := strings.Replace(line,
			shellword.Quote(binary)+" login status",
			`sh -c 'printf "%s|%s|%s" "$CODEX_HOME" "$1" "$2"' _ login status`, 1)
		if probe == line {
			t.Fatalf("the probe did not replace the command, so this proves nothing: %s", line)
		}
		out, err := exec.Command("sh", "-c", probe).Output()
		if err != nil {
			t.Fatalf("sh could not run %s: %v", probe, err)
		}
		if want := home + "|login|status"; string(out) != want {
			t.Errorf("sh read\n  %s\nas %q, want %q", line, out, want)
		}
	}
}

// A token account asked two ways: as its runs run, where claude's check says
// yes to the token whatever else the home holds; and as its own login, which
// a login made beside the token is judged by (decision 0054). The login
// itself is made without the token in its environment.
func TestALoginBesideATokenIsJudgedWithoutIt(t *testing.T) {
	bin := self(t, "claude")
	t.Setenv("ACCOUNT_TEST_TOKEN_IS_LOGIN", "1")
	ctx := context.Background()
	home, err := Ensure(t.TempDir(), "claude", "tl")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetToken(home, "sk-ant-oat01-stored"); err != nil {
		t.Fatal(err)
	}
	if env := LoginEnv("claude", home); len(env) != 1 || strings.HasPrefix(env[0], tokenVar+"=") {
		t.Errorf("the login's environment is %v, want the home and no token", env)
	}
	for _, tc := range []struct {
		name       string
		login      bool
		asRun, own bool
	}{
		{"before any login", false, true, false},
		{"after a login", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.login {
				var out strings.Builder
				if err := Login(ctx, "claude", bin, home, strings.NewReader(""), &out, &out); err != nil {
					t.Fatal(err)
				}
			}
			if in, err := LoggedIn(ctx, "claude", bin, home); err != nil || in != tc.asRun {
				t.Errorf("as a run: in=%v err=%v, want %v", in, err, tc.asRun)
			}
			if in, err := OwnLogin(ctx, "claude", bin, home); err != nil || in != tc.own {
				t.Errorf("its own login: in=%v err=%v, want %v", in, err, tc.own)
			}
		})
	}
}
