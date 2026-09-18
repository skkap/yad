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
// A hub is untrusted input (decision 0015), and an environment variable is a
// lever: the loader, a runtime, a proxy, a CA bundle, a shell option or a
// harness's own configuration can all be moved by one. So the rules are, in
// order (decision 0024):
//
//  1. the name matches grantNamePattern;
//  2. it is not reserved — reservedGrantNames and reservedGrantPrefixes, each
//     entry with the reason it is refused;
//  3. it is named as the secret it is: it ends in one of secretSuffixes.
//
// The last rule is what makes the list above safe to be incomplete: the
// variables that steer a program — HTTPS_PROXY, NODE_EXTRA_CA_CERTS,
// SHELLOPTS, PS4, JAVA_TOOL_OPTIONS and the next one somebody invents — are
// not named like secrets. The reserved list catches the secret-shaped names
// that steer anyway (ANTHROPIC_API_KEY and the cloud providers' credentials
// move billing; GIT_* and NODE_* belong to tools a harness runs).

// grantNamePattern is an environment variable name as POSIX shells accept it,
// upper case only: no lower case, so no name differs from a reserved one by
// case alone on a case-insensitive filesystem.
var grantNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// secretSuffixes are the endings a grant's name may have.
var secretSuffixes = []string{"_TOKEN", "_KEY", "_SECRET", "_PASSWORD", "_CREDENTIAL", "_CREDENTIALS"}

// reservedGrantNames are refused outright, whatever their shape.
var reservedGrantNames = map[string]string{
	"PATH":         "chooses which executable every command runs",
	"HOME":         "moves every tool's configuration and credentials",
	"SHELL":        "chooses the shell the harness runs commands in",
	"TMPDIR":       "moves where tools write temporary files",
	"BASH_ENV":     "runs a file in every non-interactive bash",
	"ENV":          "runs a file in every sh",
	"NODE_OPTIONS": "loads code into every Node process, the Claude CLI among them",
	"IS_SANDBOX":   "switches off Claude's refusal to bypass permissions as root; only the owner declares it",
}

// reservedGrantPrefixes are namespaces a grant may not use.
var reservedGrantPrefixes = []struct{ prefix, why string }{
	{"LD_", "the Linux dynamic loader reads it: LD_PRELOAD loads code into every process"},
	{"DYLD_", "the macOS dynamic loader reads it"},
	{"YAD_", "the runner's own configuration"},
	{"CLAUDE", "Claude Code's configuration and credentials (CLAUDE_CONFIG_DIR, CLAUDE_CODE_*)"},
	{"ANTHROPIC_", "Claude's API settings: ANTHROPIC_BASE_URL redirects the owner's traffic, ANTHROPIC_API_KEY moves billing"},
	{"CODEX_", "Codex's configuration and credentials"},
	{"OPENAI_", "Codex's API settings: OPENAI_BASE_URL redirects the owner's traffic, OPENAI_API_KEY moves billing"},
	{"GIT_", "git's configuration: GIT_SSH_COMMAND and GIT_CONFIG_* run commands"},
	{"NODE_", "Node's runtime settings, which the Claude CLI reads"},
	{"NPM_CONFIG_", "npm's configuration, which a harness's tools read"},
	{"BUN_", "Bun's runtime settings"},
	// A harness pointed at a cloud provider reads that provider's own
	// credential chain: a grant there would sign the owner's model traffic as
	// the hub's account, which moves billing and puts prompts where the hub can
	// read them. A deploy credential for AWS, Google or Azure is exactly what
	// an owner's allowlist of grants (epic E7) is for; until then, none.
	{"AWS_", "Claude on Bedrock reads the AWS credential chain (AWS_BEARER_TOKEN_BEDROCK, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN)"},
	{"GOOGLE_", "Claude on Vertex reads Google's application credentials (GOOGLE_APPLICATION_CREDENTIALS)"},
	{"AZURE_", "Codex on Azure reads the Azure OpenAI credentials (AZURE_OPENAI_API_KEY)"},
}

// Validate checks a grant's name and delivery. A run carrying a grant that
// fails is refused whole — never run with the grant stripped, since a run
// without the secret it was given would fail in ways nobody can trace back.
func (g Grant) Validate() error {
	if g.As != GrantEnv && g.As != GrantFile {
		return fmt.Errorf("grant %q is delivered as %q, not env or file", g.Name, g.As)
	}
	if !grantNamePattern.MatchString(g.Name) {
		return fmt.Errorf("grant name %q is not an upper-case environment variable name ([A-Z_][A-Z0-9_]*)", g.Name)
	}
	if why, ok := reservedGrantNames[g.Name]; ok {
		return fmt.Errorf("grant name %s is reserved: it %s", g.Name, why)
	}
	for _, r := range reservedGrantPrefixes {
		if strings.HasPrefix(g.Name, r.prefix) {
			return fmt.Errorf("grant name %s is in the reserved namespace %s*: %s", g.Name, r.prefix, r.why)
		}
	}
	for _, s := range secretSuffixes {
		if strings.HasSuffix(g.Name, s) && len(g.Name) > len(s) {
			return nil
		}
	}
	return fmt.Errorf("grant name %s is not named as a secret: it must end in %s, such as ZUMINO_TOKEN",
		g.Name, strings.Join(secretSuffixes, ", "))
}
