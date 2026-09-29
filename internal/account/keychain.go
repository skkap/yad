package account

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/shellword"
)

// On macOS, Claude does not keep the login of a home it is pointed at with
// CLAUDE_CONFIG_DIR inside that home. It keeps it in the user's login
// Keychain, as a generic password whose name is derived from the home's
// path (decision 0069, DEV-134). Deleting the home leaves the item behind,
// and a home made again at the same path — the same label added again —
// finds it: the new account runs on the old subscription, and `yad account
// list` calls it free before anyone has logged it in.
//
// So the Keychain item is part of the home, and goes with it: a removal
// deletes it, and making a home where none is deletes any item already there
// for that path, since a login with no home is by definition one left over.
// On Linux Claude writes the same login to .credentials.json inside the home,
// which deleting the home already deletes; nothing here runs there.
//
// Read from claude 2.1.284's own code, not documented anywhere: the naming
// function, the account attribute and CLAUDE_SECURESTORAGE_CONFIG_DIR's part
// in both. A claude upgrade can move any of it, and the way to re-check one is
// in the decision record.

// Claude keeps two items per home. The first is the OAuth login every
// subscription account has; the second is the API key a Console login
// stores, and the legacy key Claude still reads. Both name the home the same
// way, with the same suffix.
const (
	keychainLogin  = "Claude Code-credentials"
	keychainAPIKey = "Claude Code"
)

// KeychainServices are the service names of the Keychain items Claude keeps
// for a home it is given as CLAUDE_CONFIG_DIR: each base name, a hyphen and
// the first eight hex digits of the SHA-256 of the path, exactly as passed.
//
// Claude hashes the path NFC-normalised. It is hashed here as given, which
// is the same thing for every path yad makes on a Mac: macOS account names,
// and so the homes under them, are ASCII, and NFC changes nothing in ASCII
// or in text typed and stored composed. A data directory moved by the owner
// to a path spelled in decomposed Unicode is the one case this misses, and
// normalising would take golang.org/x/text as a dependency for it.
//
// The home is the value yad puts in CLAUDE_CONFIG_DIR, which is also the
// value it puts in CLAUDE_SECURESTORAGE_CONFIG_DIR (TurnEnv): Claude hashes
// the second when it is set, and the two are one path, so the name is the
// same whichever a claude reads.
func KeychainServices(home string) []string {
	sum := sha256.Sum256([]byte(home))
	suffix := "-" + hex.EncodeToString(sum[:4])
	return []string{keychainLogin + suffix, keychainAPIKey + suffix}
}

// keychainAccountName is the account Claude files its items under, as it
// accepts one.
var keychainAccountName = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// keychainAccount is the account attribute Claude gives its items: $USER, or
// the user's name when $USER is empty, and a fixed name when either is
// something Claude will not use. Read from this process's environment, which
// is the one every claude yad starts inherits (supervise.Scrub leaves USER
// alone), so the item named here is the one those claudes read and write.
func keychainAccount() string {
	name := os.Getenv("USER")
	if name == "" {
		u, err := user.Current()
		if err != nil {
			return "claude-code-user"
		}
		name = u.Username
	}
	if !keychainAccountName.MatchString(name) {
		return "claude-code-user"
	}
	return name
}

// security is the program that reaches the Keychain, by its absolute path:
// it ships with every Mac, and a `security` found on PATH could be anything
// the owner's shell put first. A variable only so that a test can put the
// fake in its place.
var security = "/usr/bin/security"

// keychainHere is whether this machine keeps Claude's logins in a Keychain.
// A variable so that the macOS path is tested on every platform, against
// the fake.
var keychainHere = runtime.GOOS == "darwin"

// KeychainLogin says whether this harness keeps its accounts' logins in the
// Keychain on this machine, rather than only inside their homes.
func KeychainLogin(harness string) bool { return harness == "claude" && keychainHere }

// securityNotFound is security's exit status for "The specified item could
// not be found in the keychain" — the answer a removal wants, and the one
// claude's own delete accepts as done.
const securityNotFound = 44

// securityTimeout bounds one call. A delete answers at once; the bound is for
// a locked Keychain, which can hold the call on a dialog nobody is at the
// screen to answer, and the removal a hub asked for or a run starting must
// not wait on it for ever. Two calls fit inside the daemon's own bound on an
// account change (control.AccountsDeadline).
const securityTimeout = 10 * time.Second

// forgetKeychain deletes the Keychain items Claude keeps for home, and says
// which it deleted. An item that is not there is no error. Nothing is done for
// a harness other than Claude or off macOS, and nothing in a test binary
// unless the test put the fake in place: a test must never reach the owner's
// real Keychain, whichever package it is in.
//
// leftover says why the caller is asking, for the error: a home about to be
// made, or one going.
func forgetKeychain(harness, home string, leftover bool) (forgot []string, err error) {
	if !KeychainLogin(harness) || home == "" {
		return nil, nil
	}
	if security == "/usr/bin/security" && testing.Testing() {
		return nil, nil
	}
	account := keychainAccount()
	var failed []string
	var causes []string
	for _, service := range KeychainServices(home) {
		gone, cause := deleteKeychainItem(service, account)
		switch {
		case cause != "":
			failed = append(failed, shellword.Command("security", "delete-generic-password", "-s", service, "-a", account))
			causes = append(causes, cause)
		case gone:
			forgot = append(forgot, service)
		}
	}
	if len(failed) > 0 {
		return forgot, &KeychainError{Home: home, Leftover: leftover, Commands: failed, Cause: strings.Join(causes, "; ")}
	}
	return forgot, nil
}

// deleteKeychainItem runs one delete. gone is whether an item was there and
// is not now; cause is why it could not be done, empty when it was.
func deleteKeychainItem(service, account string) (gone bool, cause string) {
	ctx, cancel := context.WithTimeout(context.Background(), securityTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, security, "delete-generic-password", "-s", service, "-a", account)
	// No stdin: security must not stop to ask at the terminal of whoever
	// ran yad. Nothing it prints for a delete is a secret — the password is
	// printed only when asked for with -g or -w, which this never passes.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, ""
	case errors.As(err, &exit) && exit.ExitCode() == securityNotFound:
		return false, ""
	case ctx.Err() != nil:
		return false, fmt.Sprintf("security did not answer within %s — the login Keychain may be locked", securityTimeout)
	}
	msg := strings.TrimSpace(stderr.String())
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	// Bounded: it is quoted in an error a person reads, and a line is all
	// security has to say.
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	if msg == "" {
		return false, err.Error()
	}
	return false, fmt.Sprintf("%v: %s", err, msg)
}

// KeychainError is a Keychain item Claude keeps for a home that could not be
// deleted. It carries the command that deletes each by hand, since the owner
// is the one who can unlock a Keychain.
type KeychainError struct {
	Home string
	// Leftover is an item found where a home was about to be made, rather
	// than one going with a home that is being removed.
	Leftover bool
	// Commands delete the items one by one, as the owner pastes them.
	Commands []string
	Cause    string
}

func (e *KeychainError) Error() string {
	cmds := "`" + strings.Join(e.Commands, "` and `") + "`"
	if e.Leftover {
		return fmt.Sprintf("claude has a login in the macOS Keychain for %s from an account removed before, and it could not be deleted (%s), so the home is not made — a new account there would run on that old login; %s deletes it by hand", e.Home, e.Cause, cmds)
	}
	return fmt.Sprintf("the login claude keeps for %s in the macOS Keychain could not be deleted (%s), and the home is kept until it is; %s deletes it by hand", e.Home, e.Cause, cmds)
}
