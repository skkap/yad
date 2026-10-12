package supervise

// The environment every child gets. Untagged, unlike the rest of the
// package, because internal/config refuses an [env] name by the same rule
// Scrub removes by, and one rule kept in two places drifts.

import (
	"os"
	"slices"
	"strings"
	"sync/atomic"

	v1 "github.com/skkap/yad/protocol/v1"
)

// Scrub returns env without the variables a child must not inherit:
//   - CLAUDECODE and CLAUDE_CODE_* make a nested claude believe it is running
//     inside another Claude Code session and change its behaviour;
//   - every variable that chooses whose credential a harness uses —
//     v1.AccountVariable, the list a grant is refused by — since one set on
//     the machine ranks above the account's login and moves the run off it
//     while every report still names the account (YAD-62, decision 0060);
//   - YAD_* is the runner's own configuration, which a harness has no business
//     reading.
//
// From every child, not only a harness: a setup hook or a git hook that
// starts a harness would hand the variable on to it. A project that needs
// such a key for its own use is sent it as a grant under another name, which
// is what the grant refusal tells a hub.
//
// Only env is scrubbed. What comes after it — the owner's [env] layer
// (Environ), then what a caller appends: a run's grants, an account's home
// and a token account's CLAUDE_CODE_OAUTH_TOKEN (account.TurnEnv, decision
// 0054) — reaches the child as given, and wins over anything left in env,
// os/exec keeping the last of a repeated name.
func Scrub(env []string, keep []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if contains(keep, name) || !scrubbed(name) {
			out = append(out, kv)
		}
	}
	return out
}

func scrubbed(name string) bool {
	return name == "CLAUDECODE" || strings.HasPrefix(name, "CLAUDE_CODE_") ||
		strings.HasPrefix(name, "YAD_") || accountVariable(name)
}

// harnessHomes are the account variables Scrub lets through. The owner's
// CLAUDE_CONFIG_DIR or CODEX_HOME is not a credential ranked above an
// account. With no accounts it is the harness's own login, which the default
// login check (decision 0053) and codex.DefaultHome read from this same
// environment; with an account, the account's home is appended after it and
// wins. CLAUDE_SECURESTORAGE_CONFIG_DIR is the same kind of variable — where
// Claude keeps that login, when it is not the home — and every account sets
// it to its own home the same way (decision 0070). internal/account's tests
// hold this set to the variables an account sets, so a harness that gains
// one cannot be missed here.
var harnessHomes = []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CODEX_HOME"}

func accountVariable(name string) bool {
	_, ok := v1.AccountVariable(name)
	return ok && !contains(harnessHomes, name)
}

// AccountVariables names the variables in env that Scrub removes because they
// choose a harness's credential, in env's order, for `yad doctor` and the
// daemon to warn about. Names only: the values are credentials. An empty one
// is left out — Scrub still removes it, but it chooses nothing, and a warning
// about it would teach the owner to skip the warnings.
func AccountVariables(env []string) []string {
	var out []string
	for _, kv := range env {
		if name, v, _ := strings.Cut(kv, "="); v != "" && accountVariable(name) && !contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// owner is the owner's [env] layer, as NAME=value in name order. One per
// process, set once its config.toml has loaded and before the first child
// starts, because every child — a harness turn, a setup hook, the docker
// probe, a login check — gets it, and a check that ran in another
// environment would answer about a different machine (decisions 0045, 0060).
// Atomic because children start from many goroutines; a test that sets it
// puts it back.
var owner atomic.Pointer[[]string]

// SetOwnerEnv makes env the layer every child Start runs gets after the
// scrub and before its spec's own Env. Nil or empty clears it. Callers have
// refused what OwnerRefusal refuses (config.Validate does), so nothing here
// collides with a variable the scrub removes or yad sets.
func SetOwnerEnv(env map[string]string) {
	layer := make([]string, 0, len(env))
	for name, v := range env {
		layer = append(layer, name+"="+v)
	}
	slices.Sort(layer)
	owner.Store(&layer)
}

// OwnerEnv is the owner's layer as NAME=value, in name order: a copy, so a
// caller may append to it. Its values are for a child's environment and for
// adapter.Mask, never for a message, a log line or a hub.
func OwnerEnv() []string {
	if p := owner.Load(); p != nil {
		return slices.Clone(*p)
	}
	return nil
}

// Declared reports whether the owner's layer sets name — which a grant may
// then not, since it would replace the owner's value for that run alone.
func Declared(name string) bool {
	if p := owner.Load(); p != nil {
		for _, kv := range *p {
			if n, _, _ := strings.Cut(kv, "="); n == name {
				return true
			}
		}
	}
	return false
}

// LookupEnv is name as a child Start runs inherits it, before its spec's own
// Env: the owner's [env] value when it declares name, else this process's.
// For code that reads a variable in order to build on what the child will
// get — OpenCode's inline configuration — since os.LookupEnv alone misses
// what [env] declares, and what the code builds would then replace the
// owner's value. It answers truly only in a process that has set the layer,
// as every one that starts a run has; a name read where none may have —
// USER, which `yad account remove` reads with no daemon running — is
// refused in [env] instead (OwnerRefusal). Not for a name Scrub removes,
// which no child inherits from this process.
func LookupEnv(name string) (string, bool) {
	if p := owner.Load(); p != nil {
		for _, kv := range *p {
			if n, v, _ := strings.Cut(kv, "="); n == name {
				return v, true
			}
		}
	}
	return os.LookupEnv(name)
}

// Environ is the environment a child starts from before its spec's own Env:
// this process's, scrubbed, then the owner's layer. Start uses it, and so
// does the one child that is not Start's — an interactive login, which needs
// the terminal.
func Environ(keep []string) []string {
	return append(Scrub(os.Environ(), keep), OwnerEnv()...)
}

// OwnerRefusal is why the owner's [env] may not set name, with the way round
// it; "" when it may. The rule is everything Scrub removes or chooses a
// harness's credential, and the variables yad itself owns for a child: a
// declared value of any of them would be removed, ignored, or quietly beaten
// by the value yad sets, and the owner would never learn which.
func OwnerRefusal(name string) string {
	if contains(harnessHomes, name) {
		return name + " is where the harness keeps its own login, which yad reads from the runner's environment when the harness has no accounts and replaces with the account's home when it has one (decision 0060) — " +
			"set it where the runner starts, or add an account"
	}
	if why, ok := v1.AccountVariable(name); ok {
		return name + " " + why + ", and yad keeps what chooses a harness's credential to the account it picked (decisions 0040 and 0060) — " +
			"to bill runs that way, add an account logged in with it; [env] is not for secrets"
	}
	switch {
	case name == "CLAUDECODE" || strings.HasPrefix(name, "CLAUDE_CODE_"):
		return "CLAUDECODE and CLAUDE_CODE_* are removed from every child yad starts, because they make a nested claude act as if it ran inside another Claude Code session (decision 0060) — " +
			"a Claude setting goes in the env block of Claude's own settings.json, which every account's home shares"
	case strings.HasPrefix(name, "YAD_"):
		return "YAD_* is yad's own configuration, read from the runner's environment and removed from every child — set it where the runner starts"
	case name == "IS_SANDBOX":
		return "IS_SANDBOX lets Claude bypass permissions as root, and yad's own root check and its doctor read it from the runner's environment, which [env] does not change — " +
			"set it where the runner starts, as a disposable container's entrypoint does"
	case name == "PATH":
		return "PATH is the runner's own — the one its service was installed with, which yad builds a run's from with its host-tool links in front (decision 0045) — " +
			"so a declared one would reach some children and lose to yad's in others; install the service again from a shell whose PATH you want"
	case name == "USER":
		// Read wherever yad names a claude's Keychain item — at an account's
		// removal too, from a CLI that may run with no daemon and no layer —
		// so a declared one would file a login under one name and delete it
		// under another.
		return "USER names the account Claude files its macOS Keychain login under, and yad reads it from the runner's environment to find and remove that login — set it where the runner starts, if anywhere"
	case name == "HOME":
		return "HOME moves every tool's configuration and credentials, the harness's own login among them, and yad resolves its own paths from it — set it where the runner starts, if anywhere"
	case strings.HasPrefix(name, "WT_"):
		return "WT_* is what yad gives a repository's setup hook — WT_SLOT, WT_BRANCH and the rest — so a declared one would lose to yad's in every hook and reach only the harness, where a tool reading it would take it for a hook's; choose another name"
	}
	return ""
}

func contains(xs []string, s string) bool {
	return slices.Contains(xs, s)
}
