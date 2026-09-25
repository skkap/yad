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
	if got := Env("claude", home); len(got) != 1 {
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
	want := []string{"CLAUDE_CONFIG_DIR=" + home, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-not-a-real-token"}
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
	if got := Env("claude", home); len(got) != 1 {
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
