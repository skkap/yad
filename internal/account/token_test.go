package account

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A stored token reaches the account's runs as CLAUDE_CODE_OAUTH_TOKEN, after
// the home variable, and the file holding it is the owner's alone.
func TestATokenAccountHandsItsTokenToItsRuns(t *testing.T) {
	home, err := Ensure(t.TempDir(), "claude", "tl")
	if err != nil {
		t.Fatal(err)
	}
	if got := Env("claude", home); slices.ContainsFunc(got, isToken) {
		t.Fatalf("an account with no token carries %q", got)
	}
	if err := SetToken(home, "  sk-ant-oat01-not-a-real-token\n"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(home, tokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the token file is %v, want 0600", info.Mode().Perm())
	}
	want := []string{"CLAUDE_CONFIG_DIR=" + home, "CLAUDE_SECURESTORAGE_CONFIG_DIR=" + home, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-not-a-real-token"}
	if got := Env("claude", home); !slices.Equal(got, want) {
		t.Errorf("Env = %q, want %q", got, want)
	}
	// The command yad prints for this account never carries the token.
	if s := suggest("claude", "/bin/claude", home, []string{"auth", "status"}); strings.Contains(s, "not-a-real-token") {
		t.Errorf("the suggested command carries the token: %s", s)
	}
	// Codex takes no token: its home is all its runs are given.
	if got := Env("codex", home); len(got) != 1 {
		t.Errorf("codex was handed %q", got)
	}
}

// A token file others can read is not used: the secret must be assumed
// leaked, and runs fail visibly rather than on a credential that may be
// someone else's by now.
func TestATokenOthersCanReadIsNotUsed(t *testing.T) {
	home, err := Ensure(t.TempDir(), "claude", "tl")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetToken(home, "sk-ant-oat01-x"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(home, tokenFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := Env("claude", home); slices.ContainsFunc(got, isToken) {
		t.Errorf("a world-readable token was used: %q", got)
	}
}

func TestSetTokenRefusesWhatIsNotOneToken(t *testing.T) {
	home := t.TempDir()
	for _, bad := range []string{"", "   \n", "sk-ant one", "line\nline", "tab\there"} {
		if err := SetToken(home, bad); err == nil {
			t.Errorf("SetToken(%q) stored it", bad)
		}
	}
	if _, ok := TokenStored(home); ok {
		t.Error("a refused token left a file behind")
	}
}

// The login check sees the token too, so an account added with one is
// logged in by the harness's own answer (claude 2.1.281 answers loggedIn
// with authMethod oauth_token for any token at all).
func TestTheLoginCheckSeesTheToken(t *testing.T) {
	bin := self(t, "claude")
	t.Setenv("ACCOUNT_TEST_TOKEN_IS_LOGIN", "1")
	home, err := Ensure(t.TempDir(), "claude", "tl")
	if err != nil {
		t.Fatal(err)
	}
	if in, err := LoggedIn(context.Background(), "claude", bin, home); err != nil || in {
		t.Fatalf("before the token: in=%v err=%v", in, err)
	}
	if err := SetToken(home, "sk-ant-oat01-x"); err != nil {
		t.Fatal(err)
	}
	if in, err := LoggedIn(context.Background(), "claude", bin, home); err != nil || !in {
		t.Fatalf("with the token: in=%v err=%v", in, err)
	}
}

// The owner is told a month before a token's year runs out, by label and
// date, never by the home it lives in.
func TestAnAgeingTokenIsWarnedAbout(t *testing.T) {
	home, err := Ensure(t.TempDir(), "claude", "tl")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetToken(home, "sk-ant-oat01-x"); err != nil {
		t.Fatal(err)
	}
	stored, _ := TokenStored(home)
	for _, c := range []struct {
		age  time.Duration
		warn bool
	}{
		{0, false},
		{TokenWarnAt - time.Hour, false},
		{TokenWarnAt + time.Hour, true},
		{TokenLife + time.Hour, true},
	} {
		w := TokenWarning("tl", home, stored.Add(c.age))
		if (w != "") != c.warn {
			t.Errorf("at %s: warning %q, want one = %v", c.age, w, c.warn)
		}
		if strings.Contains(w, home) {
			t.Errorf("the warning names the home: %q", w)
		}
	}
	if w := TokenWarning("tl", t.TempDir(), time.Now().Add(2*TokenLife)); w != "" {
		t.Errorf("an account with no token was warned about one: %q", w)
	}
}

// Every reader agrees on whether the token file counts: runs, list's LOGIN
// column, the expiry warning and the next action all read it one way, so a
// file yad will not use is never shown as a working token (decision 0054).
func TestEveryReaderAgreesOnWhetherATokenCounts(t *testing.T) {
	for _, c := range []struct {
		name    string
		prepare func(t *testing.T, home string)
		counts  bool
		problem bool
	}{
		{"none", func(t *testing.T, home string) {}, false, false},
		{"a good token", func(t *testing.T, home string) { mustSetToken(t, home, "sk-ant-oat01-x") }, true, false},
		{"readable by others", func(t *testing.T, home string) {
			mustSetToken(t, home, "sk-ant-oat01-x")
			if err := os.Chmod(filepath.Join(home, tokenFile), 0o644); err != nil {
				t.Fatal(err)
			}
		}, false, true},
		{"empty", func(t *testing.T, home string) {
			if err := os.WriteFile(filepath.Join(home, tokenFile), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, false, true},
		{"a link", func(t *testing.T, home string) {
			target := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.WriteFile(target, []byte("sk-ant-oat01-x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(home, tokenFile)); err != nil {
				t.Fatal(err)
			}
		}, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			home, err := Ensure(t.TempDir(), "claude", "tl")
			if err != nil {
				t.Fatal(err)
			}
			c.prepare(t, home)
			_, stored := TokenStored(home)
			env, id := TurnEnv("claude", home)
			for what, got := range map[string]bool{
				"HasToken": HasToken(home), "TokenStored": stored, "in the run's env": slices.ContainsFunc(env, isToken),
				"TokenID": id != "", "the expiry warning": TokenWarning("tl", home, time.Now().Add(TokenLife)) != "",
				"--token - in the next action": slices.Contains(AddArgs("claude", "tl", home), "--token"),
			} {
				if got != c.counts {
					t.Errorf("%s = %v, want %v", what, got, c.counts)
				}
			}
			if got := TokenProblem(home) != ""; got != c.problem {
				t.Errorf("TokenProblem = %q, want one = %v", TokenProblem(home), c.problem)
			}
		})
	}
}

// ClearToken turns a token account back into a login account; TokenID moves
// with the token and never is it.
func TestClearTokenAndTokenID(t *testing.T) {
	home, err := Ensure(t.TempDir(), "claude", "tl")
	if err != nil {
		t.Fatal(err)
	}
	mustSetToken(t, home, "sk-ant-oat01-one")
	one := TokenID(home)
	mustSetToken(t, home, "sk-ant-oat01-two")
	two := TokenID(home)
	if one == "" || one == two || strings.Contains(one+two, "sk-ant") {
		t.Errorf("TokenID one=%q two=%q", one, two)
	}
	if err := ClearToken(home); err != nil {
		t.Fatal(err)
	}
	if TokenFileExists(home) || HasToken(home) || TokenID(home) != "" {
		t.Error("the token is still there after ClearToken")
	}
	if err := ClearToken(home); err != nil {
		t.Errorf("clearing an account with no token: %v", err)
	}
}

func mustSetToken(t *testing.T, home, tok string) {
	t.Helper()
	if err := SetToken(home, tok); err != nil {
		t.Fatal(err)
	}
}

// isToken is whether an environment entry hands a harness a stored token.
func isToken(kv string) bool { return strings.HasPrefix(kv, tokenVar+"=") }
