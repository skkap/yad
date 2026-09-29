package v1

import (
	"fmt"
	"regexp"
	"strings"
)

// A grant is a secret for a run to use: the runner puts it in the harness's
// environment as NAME=value, or writes it to a 0600 file in a directory of
// its own and puts the file's path in NAME. Either way the name becomes an
// environment variable, and for a file grant also the file's name — so a name
// that passes these rules is a plain file name too, never a path.
//
// The owner trusts the hubs it connects (decision 0038). A hub writes the
// brief, and a brief can tell the harness to read any file or send any secret
// anywhere, which the harness then auto-approves (0015) — so filtering
// variable names never protected the machine, and any valid environment
// variable name is accepted. Decision 0024's secret-shaped suffix rule and its
// namespace reservations are gone with that reasoning.
//
// Two lists remain, and neither is a security control. The four in
// deniedGrantNames and deniedGrantPrefixes would break the run rather than
// attack it, and they are refused so that a hub's mistake cannot unset PATH and
// make every run on the machine fail in a way nobody can trace back. The names
// in accountGrantNames would move the run off the account the runner chose for
// it, and they are refused so that the account layer keeps telling the truth
// (decision 0040). A grant is still delivered to the harness process alone,
// kept out of the prompt, the logs and the events, and deleted when the run
// ends — that protects the secret from being recorded, not the machine from
// the hub.
//
// Where a variable is the owner's to set rather than the hub's, the code that
// owns it keeps its own guard, which is not a naming rule: IS_SANDBOX is an
// acceptable grant name here and the Claude adapter still strips it from the
// run's environment, because permission mode and sandbox are runner
// configuration (0015, and 0038 keeps it).

// grantNamePattern is an environment variable name as a POSIX shell accepts
// one, and nothing else: no '/', '.', '=' or space, so a file grant's name is
// a plain file name and never a path. ASCII only, which is also what keeps a
// lookalike out — Cyrillic "РАТН" does not match this pattern at all.
var grantNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// deniedGrantNames and deniedGrantPrefixes are matched case-insensitively,
// which refuses more than it strictly has to: environment variables are
// case-sensitive on linux and darwin, so a grant named "path" arrives as
// "path" and leaves PATH alone. It is refused anyway because this list catches
// a hub's mistake, and the mistake it catches is a hub author's — one who
// expects Windows, where the environment folds case and "Path" is PATH. The
// over-refusal costs nothing real, since no legitimate grant is named "path"
// or "home", and it leaves no question of a name in the wrong case slipping
// past a list meant to be exhaustive.
//
// The filesystem's own case folding is a different problem, and it is handled
// where it actually bites: two file grants whose names differ only by case
// share one file (Run.Validate).
var deniedGrantNames = map[string]string{
	"PATH": "chooses which executable every command runs, so a run carrying it may not find its harness at all",
	"HOME": "moves every tool's configuration and credentials, the harness's own among them",
}

var deniedGrantPrefixes = []struct{ prefix, why string }{
	{"LD_", "the Linux dynamic loader reads it, and LD_PRELOAD loads code into every process the run starts"},
	{"DYLD_", "the macOS dynamic loader reads it the same way"},
}

// accountGrantNames are the variables that choose whose credential a harness
// uses, or which home it reads its login from. Accounts are the owner's to
// configure (0038 keeps them), and the runner picks one for every turn. A grant
// under one of these names lands in the harness's environment after the
// owner's own copy was scrubbed (supervise.Scrub), and wins: the turn would
// spend a credential the hub chose while its events, health and
// `yad account list` name the account the runner picked. That account's limits
// would never fire, failover would have nothing to fail over, and health would
// report it free for ever (decision 0040).
//
// The owner's own copies of these names are removed from every child the
// runner starts, by supervise.Scrub reading this same list through
// AccountVariable, since one set on the machine ranks above the account just
// as a grant would. The harness home variables are the exception there: the
// owner's CLAUDE_CONFIG_DIR, CLAUDE_SECURESTORAGE_CONFIG_DIR or CODEX_HOME is
// the harness's own login when it has no accounts, and an account's home is
// appended after it and wins.
//
// One list for every harness, rather than each harness refusing its own. A hub
// checks it before queueing without knowing which adapter reads what, a run of
// one harness may start the other as a tool, and refusing ANTHROPIC_API_KEY on
// a Codex run costs only a rename, which the refusal names.
//
// Endpoint and header variables are here beside the credentials: the server
// that answers a turn decides whose account it runs on, and a turn answered
// through someone else's endpoint reports none of the account's windows —
// Claude on an API key emits no rate_limit_event at all
// (internal/adapter/claude/testdata/README.md).
//
// Left off is what takes effect only behind a switch refused here. Claude's
// cloud providers are each chosen by a CLAUDE_CODE_USE_<provider> switch —
// Bedrock, Vertex, Foundry, Claude Platform on AWS so far — and every one is
// refused by prefix, so that the next provider Claude ships is refused before
// anyone thinks to add it; Scrub removes every CLAUDE_CODE_* from the owner's
// environment too. So each provider's own keys, endpoints and workspace ids
// (ANTHROPIC_FOUNDRY_API_KEY, ANTHROPIC_AWS_API_KEY, ANTHROPIC_BEDROCK_BASE_URL,
// ANTHROPIC_AWS_WORKSPACE_ID, ANTHROPIC_VERTEX_BASE_URL and the like) move
// nothing on their own, and a project that deploys to a cloud keeps its
// credentials. The same holds for the Workload Identity Federation inputs
// (ANTHROPIC_IDENTITY_TOKEN, ANTHROPIC_IDENTITY_TOKEN_FILE,
// ANTHROPIC_SERVICE_ACCOUNT_ID, ANTHROPIC_WORKSPACE_ID): Claude federates only
// when ANTHROPIC_FEDERATION_RULE_ID and ANTHROPIC_ORGANIZATION_ID are both set,
// and both are refused. Each is refused, rather than the pair, because the
// owner's environment may already hold the other.
//
// Matched whole and case-insensitively, as the deny list is and for its
// reason. internal/account's tests hold every harness home variable to this
// list, so a harness that gains account homes cannot be missed here. The
// sources are Claude Code's authentication and environment-variable
// references and Codex's codex-rs/login; a name either harness adds for the
// same job is a line to add here.
var accountGrantNames = map[string]string{
	// Claude Code, in its own precedence order (code.claude.com/docs/en/authentication).
	"ANTHROPIC_AUTH_TOKEN":         "is a bearer token Claude sends in place of the account's login",
	"ANTHROPIC_API_KEY":            "is an Anthropic API key, which Claude can bill in place of the account's subscription",
	"CLAUDE_CODE_OAUTH_TOKEN":      "is a subscription's OAuth token, which Claude uses in place of the login in the account's home",
	"ANTHROPIC_PROFILE":            "names an Anthropic profile, which Claude ranks above the login in the account's home",
	"ANTHROPIC_FEDERATION_RULE_ID": "with ANTHROPIC_ORGANIZATION_ID puts Claude on a federated credential ranked above the account's login",
	"ANTHROPIC_ORGANIZATION_ID":    "with ANTHROPIC_FEDERATION_RULE_ID puts Claude on a federated credential ranked above the account's login",
	"ANTHROPIC_CONFIG_DIR":         "chooses the directory Claude reads Anthropic profiles from, and a federation profile there ranks above the account's login",
	"CLAUDE_CONFIG_DIR":            "is the home Claude reads its login from, which is the account itself",
	// Read from claude 2.1.284's code (decision 0069): set, it replaces the
	// home in the name of the macOS Keychain item Claude keeps the login in,
	// and in where it writes .credentials.json elsewhere — and set empty, it
	// means the owner's own default login.
	"CLAUDE_SECURESTORAGE_CONFIG_DIR": "chooses where Claude keeps its login — the macOS Keychain item or the .credentials.json file — in place of the account's home, so every run handed one path reads the one login there",
	"ANTHROPIC_BASE_URL":              "chooses the server that answers and bills Claude's turns",
	"ANTHROPIC_CUSTOM_HEADERS":        "adds headers to every request Claude makes, and an x-api-key or Authorization header there is a credential",
	// Codex (codex-rs/login/src/auth/manager.rs).
	"CODEX_HOME":                       "is the home Codex reads its login from, which is the account itself",
	"OPENAI_API_KEY":                   "is an OpenAI API key, which Codex can bill in place of the account's ChatGPT login",
	"CODEX_API_KEY":                    "is an OpenAI API key Codex uses in place of the login in the account's home",
	"CODEX_ACCESS_TOKEN":               "is an access token Codex uses in place of the login in the account's home",
	"OPENAI_BASE_URL":                  "chooses the server that answers and bills Codex's turns",
	"CODEX_REFRESH_TOKEN_URL_OVERRIDE": "sends the account's refresh token to another server, whose answer Codex then saves as the account's login",
	// Codex has no switch for its Bedrock provider: an account home whose
	// config.toml chooses it reads this key ahead of the AWS credential chain,
	// so for Codex it is not inert the way Claude's provider keys are. The AWS
	// chain itself stays grantable for a project's deploys.
	"AWS_BEARER_TOKEN_BEDROCK": "is a Bedrock API key, which Codex on its Bedrock provider uses ahead of the account's own AWS credentials",
}

// accountGrantPrefixes are families of account-choosing variables refused
// whole, for the reason given with each.
var accountGrantPrefixes = []struct{ prefix, why string }{
	{"CLAUDE_CODE_USE_", "is how Claude is switched onto a cloud provider's credentials (Bedrock, Vertex, Foundry, Claude Platform on AWS), and the next provider will be named the same way"},
}

// AccountVariable says whether name chooses whose credential a harness uses or
// which home it reads its login from — whether it is on the list above — and
// if so why, in words that follow the name: "ANTHROPIC_PROFILE names an
// Anthropic profile, …". Matched as Grant.Validate matches it, whole or by
// prefix and in any case.
//
// Exported because the list has two readers. Grant.Validate refuses these
// names from a hub; the runner's supervise.Scrub removes them from the
// owner's own environment, which ranks above the account the same way, and
// `yad doctor` names any it finds there (DEV-62). protocol/v1 imports nothing
// of ours (ARCHITECTURE.md §1), so the list cannot move below it, and a copy
// in the runner is the drift this function exists to prevent. For a Go hub it
// answers what Validate already enforces. The answer may change as names are
// added; the signature is the promise.
func AccountVariable(name string) (why string, ok bool) {
	_, why, ok = accountVariable(name)
	return why, ok
}

// accountVariable is AccountVariable with the list entry that matched — the
// name itself, or a prefix followed by '*' — for the refusal to quote.
func accountVariable(name string) (entry, why string, ok bool) {
	for n, why := range accountGrantNames {
		if strings.EqualFold(name, n) {
			return n, why, true
		}
	}
	for _, d := range accountGrantPrefixes {
		if len(name) >= len(d.prefix) && strings.EqualFold(name[:len(d.prefix)], d.prefix) {
			return d.prefix + "*", d.why, true
		}
	}
	return "", "", false
}

// accountRefusal is the one message for a name either account list refuses:
// what the name does, the decision, and the two ways a hub gets what it
// wanted without it.
func accountRefusal(name, what, why string) error {
	return fmt.Errorf("grant name %s is refused: %s %s, and a grant may not move a run off the account the runner's owner chose for it (decision 0040) — "+
		"for the project's own use, send the value under another name and have the brief say which; "+
		"to run on another login, the runner's owner adds one as an account with `yad account add`", name, what, why)
}

// Validate checks a grant's name and delivery. A run carrying a grant that
// fails is refused whole — never run with the grant stripped, since a run
// without the secret it was given would fail in ways nobody can trace back.
func (g Grant) Validate() error {
	if g.As != GrantEnv && g.As != GrantFile {
		return fmt.Errorf("grant %q is delivered as %q, not env or file", g.Name, g.As)
	}
	if !grantNamePattern.MatchString(g.Name) {
		return fmt.Errorf("grant name %q is not an environment variable name ([A-Za-z_][A-Za-z0-9_]*), which is also the file name a file grant is written to", g.Name)
	}
	for name, why := range deniedGrantNames {
		if strings.EqualFold(g.Name, name) {
			return fmt.Errorf("grant name %s is refused: %s %s — send the value under another name", g.Name, name, why)
		}
	}
	for _, d := range deniedGrantPrefixes {
		if len(g.Name) >= len(d.prefix) && strings.EqualFold(g.Name[:len(d.prefix)], d.prefix) {
			return fmt.Errorf("grant name %s is refused: %s* is denied because %s — send the value under another name", g.Name, d.prefix, d.why)
		}
	}
	if entry, why, ok := accountVariable(g.Name); ok {
		return accountRefusal(g.Name, entry, why)
	}
	return nil
}
