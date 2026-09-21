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
// One list for every harness, rather than each harness refusing its own. A hub
// checks it before queueing without knowing which adapter reads what, a run of
// one harness may start the other as a tool, and refusing ANTHROPIC_API_KEY on
// a Codex run costs only a rename, which the refusal names.
//
// Endpoint and header variables are here beside the credentials: the server
// that answers a turn decides whose account it runs on, and a turn answered
// through someone else's endpoint reports none of the account's windows —
// Claude on an API key emits no rate_limit_event at all
// (internal/adapter/claude/testdata/README.md). Left off is what takes effect
// only behind a switch already here: the Bedrock, Vertex and Foundry keys and
// endpoints (AWS_BEARER_TOKEN_BEDROCK, ANTHROPIC_FOUNDRY_API_KEY,
// ANTHROPIC_VERTEX_BASE_URL and the like) are read only once a
// CLAUDE_CODE_USE_* switch is on, and those switches are refused below and
// scrubbed from the owner's environment.
//
// Matched whole and case-insensitively, as the deny list is and for its
// reason. internal/account's tests hold every harness home variable to this
// list, so a harness that gains account homes cannot be missed here.
var accountGrantNames = map[string]string{
	// Claude Code.
	"ANTHROPIC_API_KEY":        "is an Anthropic API key, which Claude can bill in place of the account's subscription",
	"ANTHROPIC_AUTH_TOKEN":     "is a bearer token Claude sends in place of the account's login",
	"CLAUDE_CODE_OAUTH_TOKEN":  "is a subscription's OAuth token, which Claude uses in place of the login in the account's home",
	"CLAUDE_CONFIG_DIR":        "is the home Claude reads its login from, which is the account itself",
	"ANTHROPIC_BASE_URL":       "chooses the server that answers and bills Claude's turns",
	"ANTHROPIC_CUSTOM_HEADERS": "adds headers to every request Claude makes, and an x-api-key or Authorization header there is a credential",
	"CLAUDE_CODE_USE_BEDROCK":  "moves Claude onto an AWS account's credentials",
	"CLAUDE_CODE_USE_VERTEX":   "moves Claude onto a Google Cloud account's credentials",
	"CLAUDE_CODE_USE_FOUNDRY":  "moves Claude onto an Azure account's credentials",
	// Codex.
	"CODEX_HOME":      "is the home Codex reads its login from, which is the account itself",
	"OPENAI_API_KEY":  "is an OpenAI API key, which Codex can bill in place of the account's ChatGPT login",
	"CODEX_API_KEY":   "is an OpenAI API key codex exec uses in place of the login in the account's home",
	"OPENAI_BASE_URL": "chooses the server that answers and bills Codex's turns",
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
	for name, why := range accountGrantNames {
		if strings.EqualFold(g.Name, name) {
			return fmt.Errorf("grant name %s is refused: %s %s, and a grant may not move a run off the account the runner's owner chose for it (decision 0040) — "+
				"for the project's own use, send the value under another name and have the brief say which; "+
				"to run on another login, the runner's owner adds one as an account with `yad account add`", g.Name, name, why)
		}
	}
	return nil
}
