package account

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/shellword"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// fakeSecurity is the test binary playing /usr/bin/security, so no test ever
// reaches the owner's real Keychain. It appends each argv it is run with to
// ACCOUNT_TEST_SECURITY_LOG, one line per call, and answers as security does:
//
//   - found: the item was there and is deleted (exit 0);
//   - missing: "could not be found in the keychain" (exit 44);
//   - login: only the OAuth login's item is there;
//   - locked: security cannot reach the Keychain (exit 36 is what a locked one
//     answers with no one at the screen).
func fakeSecurity(mode string, args []string) int {
	f, err := os.OpenFile(os.Getenv("ACCOUNT_TEST_SECURITY_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	fmt.Fprintln(f, strings.Join(args, "\x1f"))
	f.Close()
	notFound := func() int {
		fmt.Fprintln(os.Stderr, "security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.")
		return 44
	}
	switch mode {
	case "found":
		return 0
	case "missing":
		return notFound()
	case "login":
		if len(args) > 2 && strings.HasPrefix(args[2], keychainLogin+"-") {
			return 0
		}
		return notFound()
	case "locked":
		fmt.Fprintln(os.Stderr, "security: SecKeychainItemDelete: User interaction is not allowed.")
		return 36
	}
	return 2
}

// useFakeSecurity puts the fake in security's place, as a Mac, answering as
// mode says, and returns a reader of the calls it has had since the last read.
func useFakeSecurity(t *testing.T, mode string) (calls func() [][]string) {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	was, wasHere := security, keychainHere
	security, keychainHere = bin, true
	t.Cleanup(func() { security, keychainHere = was, wasHere })
	log := filepath.Join(t.TempDir(), "security.log")
	t.Setenv("ACCOUNT_TEST_SECURITY", mode)
	t.Setenv("ACCOUNT_TEST_SECURITY_LOG", log)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	return func() [][]string {
		t.Helper()
		b, err := os.ReadFile(log)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(log); err != nil {
			t.Fatal(err)
		}
		var out [][]string
		for line := range strings.Lines(string(b)) {
			out = append(out, strings.Split(strings.TrimSuffix(line, "\n"), "\x1f"))
		}
		return out
	}
}

// deletes is the argv of one delete of each named item, in order.
func deletes(account string, services ...string) [][]string {
	var out [][]string
	for _, s := range services {
		out = append(out, []string{"delete-generic-password", "-s", s, "-a", account})
	}
	return out
}

func sameCalls(a, b [][]string) bool {
	return slices.EqualFunc(a, b, func(x, y []string) bool { return slices.Equal(x, y) })
}

// The names are pinned to values worked out outside Go — `printf '%s' <path> |
// shasum -a 256`, the first eight hex digits — so that a change here that
// still agrees with itself cannot pass. The naming itself is claude
// 2.1.284's (decision 0070 says where to read it again).
func TestKeychainServicesArePinned(t *testing.T) {
	for _, c := range []struct {
		home string
		want []string
	}{
		{"/Users/owner/.local/share/yad/accounts/claude/work",
			[]string{"Claude Code-credentials-2130e35f", "Claude Code-2130e35f"}},
		// Hashed exactly as given: a space, an apostrophe and a $ are bytes
		// like any other, and nothing is expanded or cleaned.
		{"/home/o w/it's $HOME/claude/x",
			[]string{"Claude Code-credentials-d583f391", "Claude Code-d583f391"}},
	} {
		if got := KeychainServices(c.home); !slices.Equal(got, c.want) {
			t.Errorf("KeychainServices(%q) = %q, want %q", c.home, got, c.want)
		}
	}
}

func TestKeychainAccountIsClaudes(t *testing.T) {
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	fromUser := me.Username
	if !keychainAccountName.MatchString(fromUser) {
		fromUser = "claude-code-user"
	}
	for _, c := range []struct{ user, want string }{
		{"owner", "owner"},
		{"o.w-n_er", "o.w-n_er"},
		// Claude files under a fixed name what it will not use as one.
		{"o wner", "claude-code-user"},
		{"オーナー", "claude-code-user"},
		// An empty $USER is the user's own name, as claude reads it.
		{"", fromUser},
	} {
		t.Setenv("USER", c.user)
		if got := keychainAccount(); got != c.want {
			t.Errorf("USER=%q: keychainAccount() = %q, want %q", c.user, got, c.want)
		}
	}
}

// Removing a Claude account deletes both items Claude keeps for its home,
// and a removal that finds neither is done.
func TestRemovingAClaudeAccountDeletesItsKeychainLogin(t *testing.T) {
	t.Setenv("USER", "owner")
	for _, mode := range []string{"found", "missing", "login"} {
		t.Run(mode, func(t *testing.T) {
			calls := useFakeSecurity(t, mode)
			data := t.TempDir()
			home, err := Ensure(data, "claude", "work")
			if err != nil {
				t.Fatal(err)
			}
			calls()
			if err := Remove(data, "claude", "work"); err != nil {
				t.Fatalf("Remove: %v", err)
			}
			if want := deletes("owner", KeychainServices(home)...); !sameCalls(calls(), want) {
				t.Errorf("security was not run as %q", want)
			}
			if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the home is still there: %v", err)
			}
			assertNoSetAside(t, data, "claude", "work")
		})
	}
}

// A Keychain that will not let go keeps the home too, set aside, so a home
// and its login go together; the error names the command that deletes the
// login by hand, and the next removal finishes both.
func TestAKeychainThatRefusesKeepsTheHome(t *testing.T) {
	t.Setenv("USER", "owner")
	calls := useFakeSecurity(t, "missing")
	data := t.TempDir()
	home, err := Ensure(data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	calls()
	t.Setenv("ACCOUNT_TEST_SECURITY", "locked")
	err = Remove(data, "claude", "work")
	var kerr *KeychainError
	if !errors.As(err, &kerr) || kerr.Leftover {
		t.Fatalf("Remove = %v, want a KeychainError for a home going", err)
	}
	if len(calls()) != 2 {
		t.Error("security was not asked about both items")
	}
	if !strings.Contains(err.Error(), "home is kept") || !strings.Contains(err.Error(), "User interaction is not allowed") {
		t.Errorf("the error does not say what is kept and why: %v", err)
	}
	cmds := printedDeletes(t, err)
	if len(cmds) != 2 {
		t.Fatalf("the error names %d commands, want one per item: %v", len(cmds), err)
	}
	// Pasted into a shell, each deletes its item: the fake is what the
	// printed path names here, as /usr/bin/security is on a Mac.
	t.Setenv("ACCOUNT_TEST_SECURITY", "found")
	for _, cmd := range cmds {
		runInSh(t, cmd)
	}
	if want := deletes("owner", KeychainServices(home)...); !sameCalls(calls(), want) {
		t.Errorf("the printed commands did not run as %q", want)
	}
	t.Setenv("ACCOUNT_TEST_SECURITY", "locked")
	if n := setAside(t, data, "claude", "work"); n != 1 {
		t.Errorf("%d copies of the home are set aside, want the one kept", n)
	}

	t.Setenv("ACCOUNT_TEST_SECURITY", "found")
	if err := Remove(data, "claude", "work"); err != nil {
		t.Fatalf("Remove again: %v", err)
	}
	if want := deletes("owner", KeychainServices(home)...); !sameCalls(calls(), want) {
		t.Errorf("the second removal did not delete the login")
	}
	assertNoSetAside(t, data, "claude", "work")
}

// A home made where there was none is cleared of any login Claude left for
// its path — this is what makes a leftover from before the fix, or from a
// removal that died, harmless — and says what it cleared, for `yad account
// add` to tell the owner. A home already there is its own login's, and the
// Keychain is not asked.
func TestANewHomeStartsWithNoKeychainLogin(t *testing.T) {
	t.Setenv("USER", "owner")
	calls := useFakeSecurity(t, "login")
	data := t.TempDir()
	home, cleared, err := Prepare(data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	services := KeychainServices(home)
	if !sameCalls(calls(), deletes("owner", services...)) {
		t.Error("security was not asked to delete both items before the home was made")
	}
	if !slices.Equal(cleared, services[:1]) {
		t.Errorf("Prepare cleared %q, want the login it found", cleared)
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatal(err)
	}

	if _, cleared, err := Prepare(data, "claude", "work"); err != nil || len(cleared) > 0 {
		t.Fatalf("Prepare on the home again: %q, %v", cleared, err)
	}
	if got := calls(); len(got) > 0 {
		t.Errorf("a home already there had the Keychain asked about it: %q", got)
	}
}

// A leftover login that cannot be deleted stops the home being made: a home
// there would run on it.
func TestALeftoverThatCannotGoStopsTheHome(t *testing.T) {
	t.Setenv("USER", "owner")
	calls := useFakeSecurity(t, "locked")
	data := t.TempDir()
	_, _, err := Prepare(data, "claude", "work")
	var kerr *KeychainError
	if !errors.As(err, &kerr) || !kerr.Leftover {
		t.Fatalf("Prepare = %v, want a KeychainError for a leftover", err)
	}
	if !strings.Contains(err.Error(), "not made") {
		t.Errorf("the error does not say the home was not made: %v", err)
	}
	cmds := printedDeletes(t, err)
	if len(cmds) != 2 {
		t.Fatalf("the error names %d commands, want one per item: %v", len(cmds), err)
	}
	calls()
	t.Setenv("ACCOUNT_TEST_SECURITY", "found")
	for _, cmd := range cmds {
		runInSh(t, cmd)
	}
	home := HomeDir(data, "claude", "work")
	if want := deletes("owner", KeychainServices(home)...); !sameCalls(calls(), want) {
		t.Errorf("the printed commands did not run as %q", want)
	}
	if _, err := os.Lstat(HomeDir(data, "claude", "work")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the home was made anyway: %v", err)
	}
}

// A removal that finishes after the label has been added again leaves the
// new home's login alone: the Keychain item for the path is the new home's
// now.
func TestFinishingARemovalLeavesANewHomesLogin(t *testing.T) {
	t.Setenv("USER", "owner")
	calls := useFakeSecurity(t, "missing")
	data := t.TempDir()
	if _, err := Ensure(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if err := SetAside(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := Ensure(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	calls()
	if err := RemoveSetAside(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if got := calls(); len(got) > 0 {
		t.Errorf("finishing the old removal deleted the new home's login: %q", got)
	}
	assertNoSetAside(t, data, "claude", "work")
}

// Only Claude keeps a login outside its home, and only on a Mac: Codex's is
// auth.json in the home, and Claude's on Linux is .credentials.json in it,
// which deleting the home deletes.
func TestOnlyClaudeOnAMacHasAKeychainLogin(t *testing.T) {
	calls := useFakeSecurity(t, "found")
	data := t.TempDir()
	if _, err := Ensure(data, "codex", "work"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(data, "codex", "work"); err != nil {
		t.Fatal(err)
	}
	if got := calls(); len(got) > 0 {
		t.Errorf("a codex account had the Keychain asked about it: %q", got)
	}
	keychainHere = false
	if _, err := Ensure(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if err := Remove(data, "claude", "work"); err != nil {
		t.Fatal(err)
	}
	if got := calls(); len(got) > 0 {
		t.Errorf("off macOS the Keychain was asked: %q", got)
	}
}

// printedDeletes are the commands an error prints for deleting a Keychain
// item by hand: each set apart in backticks and starting with the program
// the runner itself runs.
func printedDeletes(t *testing.T, err error) []string {
	t.Helper()
	return shellwordtest.Commands(err.Error(), shellword.Quote(security)+" ")
}

// runInSh runs a printed command the way an owner pasting it would, through
// a real sh, in this test's environment — where the program it names is the
// fake, which records the argv it was given.
func runInSh(t *testing.T, line string) {
	t.Helper()
	if out, err := exec.Command("/bin/sh", "-c", line).CombinedOutput(); err != nil {
		t.Errorf("sh -c %s: %v\n%s", line, err, out)
	}
}

// What is printed names the program by the path the runner runs, not by a
// name the owner's PATH would resolve: on a Mac, /usr/bin/security.
func TestThePrintedDeleteNamesTheProgramByItsPath(t *testing.T) {
	argv := deleteArgv("Claude Code-credentials-2130e35f", "owner")
	if argv[0] != "/usr/bin/security" {
		t.Errorf("the delete runs and prints %q, want /usr/bin/security", argv[0])
	}
	line := shellword.Command(argv...)
	if !strings.HasPrefix(line, "/usr/bin/security ") {
		t.Fatalf("printed as %s", line)
	}
	// The stubs sh runs are functions, which cannot carry a path in their
	// name; the rest of the line is what is checked.
	shellwordtest.Check(t, "security"+strings.TrimPrefix(line, "/usr/bin/security"),
		"security", "delete-generic-password", "-s", "Claude Code-credentials-2130e35f", "-a", "owner")
}

func setAside(t *testing.T, data, harness, label string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(HomeDir(data, harness, label)))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), asidePrefix(label)) {
			n++
		}
	}
	return n
}

func assertNoSetAside(t *testing.T, data, harness, label string) {
	t.Helper()
	if n := setAside(t, data, harness, label); n != 0 {
		t.Errorf("%d copies of the home are left set aside", n)
	}
}
