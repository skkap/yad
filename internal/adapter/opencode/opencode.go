// Package opencode drives OpenCode through `opencode acp`, the Agent Client
// Protocol core in internal/adapter/acp (decision 0073). What is OpenCode's
// own is here: how the run's context reaches its model, the password on the
// server it opens, what its failures mean, and the one release it is pinned
// to.
package opencode

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/acp"
	"github.com/skkap/yad/internal/adapter/jsonrpc"
)

// Adapter is the OpenCode adapter.
type Adapter struct {
	// Raw, when set, receives the whole conversation with `opencode acp`,
	// one JSON object per line, ours wrapped as {">": …}: the local
	// diagnostic copy that is never sent to a hub (decision 0012), and what
	// the fixture recorder keeps. Called once per Start.
	Raw func(spec adapter.Spec) io.Writer
}

func (Adapter) Harness() string { return "opencode" }

// AppliesEffort: a run's effort is the session's thought_level option, which
// OpenCode calls effort and offers for a model with variants.
func (Adapter) AppliesEffort() bool { return true }

// Forks: a fork is session/fork, which OpenCode answers with a new session
// holding a copy of the conversation (decision 0065). Unstable in ACP v1,
// and pinned with the release (PinnedVersion).
func (Adapter) Forks() bool { return true }

// Steers is false: ACP v1 has no steer (decision 0073).
func (Adapter) Steers() bool { return false }

func (a Adapter) Start(ctx context.Context, spec adapter.Spec) (adapter.Turn, error) {
	return acp.Start(ctx, a.agent(), spec)
}

func (a Adapter) agent() acp.Agent {
	return acp.Agent{
		ID: "opencode", Name: "OpenCode",
		Args:     []string{"acp"},
		Prepare:  prepare,
		Classify: classify,
		ExitCode: exitCode,
		// Its model list is its login check (account.statusArgs): on Zen's
		// free models it runs with no credential, so `auth list` saying none
		// would be no answer.
		LoginCheck: []string{"models"},
		Raw:        a.Raw,
	}
}

// The variables the adapter sets on every run.
const (
	// envPassword is the password OpenCode's own HTTP server takes. `opencode
	// acp` serves the agent from a server it opens on 127.0.0.1 and talks to
	// itself over it; without a password, anything on the machine that finds
	// the port drives the session with the run's credentials (decision 0073).
	envPassword = "OPENCODE_SERVER_PASSWORD"
	// envConfig is inline configuration OpenCode merges over every file it
	// reads, and the route the run's context takes (decision 0050).
	envConfig = "OPENCODE_CONFIG_CONTENT"
)

// passwordBytes is the password's entropy: 32 random bytes, hex encoded. It
// lives as long as the run's process and is never written anywhere but the
// child's environment.
const passwordBytes = 32

// contextFile is the file the run's context is written to, in a directory of
// the run's own. OpenCode reads an absolute instruction path as a glob of its
// base name, so the name is fixed and holds no glob character.
const contextFile = "context.md"

// prepare readies one run: the run's environment without OpenCode's own
// variables, a fresh password for OpenCode's server, and the run's context as
// an instruction file OpenCode puts in its system prompt.
//
// OPENCODE_* is OpenCode's configuration — its permissions
// (OPENCODE_PERMISSION), inline config that can name a provider and its key
// (OPENCODE_CONFIG_CONTENT), its credentials (OPENCODE_AUTH_CONTENT) — and a
// run's environment carries a hub's grants: a hub must never widen what a
// harness may do on the owner's machine (AGENTS.md), so none of them passes,
// as Claude's adapter drops IS_SANDBOX. The owner's own, set in the runner's
// environment, reach OpenCode as they would reach it at a terminal.
//
// The context is OpenCode's equivalent of Claude's appended system prompt:
// config.instructions are read into the system prompt of every request the
// model gets, so they are outside the conversation, survive a compaction and
// reach a resumed or forked session's next turn as they reach a new one's.
// Measured on 1.18.33: a new session told nothing asked for a codeword the
// instructions named, and its fork, answered it (decision 0073).
func prepare(spec adapter.Spec) ([]string, func(), error) {
	secret := make([]byte, passwordBytes)
	if _, err := rand.Read(secret); err != nil {
		return nil, nil, &adapter.LocalError{Msg: "no password could be made for OpenCode's server on this runner", Err: err}
	}
	env := make([]string, 0, len(spec.Env)+2)
	for _, kv := range spec.Env {
		if name, _, _ := strings.Cut(kv, "="); !strings.HasPrefix(name, "OPENCODE_") {
			env = append(env, kv)
		}
	}
	env = append(env, envPassword+"="+hex.EncodeToString(secret))
	if spec.Brief.Context == "" {
		return env, nil, nil
	}
	config, err := ownerConfig()
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp("", "yad-opencode-")
	if err != nil {
		return nil, nil, &adapter.LocalError{Msg: "the run's context could not be written for OpenCode on this runner", Err: err}
	}
	cleanup := func() { os.RemoveAll(dir) }
	path := filepath.Join(dir, contextFile)
	if err := os.WriteFile(path, []byte(spec.Brief.Context), 0o600); err != nil {
		cleanup()
		return nil, nil, &adapter.LocalError{Msg: "the run's context could not be written for OpenCode on this runner", Err: err}
	}
	var instructions []any
	if list, ok := config["instructions"].([]any); ok {
		instructions = list
	}
	config["instructions"] = append(instructions, path)
	b, err := json.Marshal(config)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return append(env, envConfig+"="+string(b)), cleanup, nil
}

// ownerConfig is the inline configuration the owner gives OpenCode in the
// runner's own environment. Its keys are kept and the context is added to
// its instructions, since the variable holds one value and the last one set
// wins.
func ownerConfig() (map[string]any, error) {
	value, set := os.LookupEnv(envConfig)
	config := map[string]any{}
	if !set || strings.TrimSpace(value) == "" {
		return config, nil
	}
	// A JSON null decodes into a nil map, which would be written to below;
	// only an object is configuration.
	if err := json.Unmarshal([]byte(value), &config); err != nil || config == nil {
		return nil, fmt.Errorf("%s in the runner's environment is not a JSON object yad can add the run's context to — make it plain JSON (no comments), or unset it", envConfig)
	}
	return config, nil
}

// exitCode is a bash call's exit status, which OpenCode reports in its
// rawOutput's metadata.
func exitCode(raw json.RawMessage) *int {
	var r struct {
		Metadata struct {
			Exit *int `json:"exit"`
		} `json:"metadata"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	return r.Metadata.Exit
}

// errorData is the structure OpenCode 1.18.33 puts beside a failed prompt:
// errorName is the name of its own error, which says what failed where the
// message is a provider's words.
type errorData struct {
	ErrorName  string `json:"errorName"`
	ProviderID string `json:"providerId"`
}

// codeAuthRequired is ACP's error for an agent that needs authentication,
// which OpenCode answers a provider's refusal of its credential with
// (ProviderAuthError).
const codeAuthRequired = -32000

// usageLimit is the wording OpenCode Zen's subscription limits carry, as
// OpenCode passes them through as a failed prompt's message: the only place
// the window and its reset are said (packages/console, zen.api.error.go*
// and subscriptionQuotaExceeded, at OpenCode 1.18.33). Matched narrowly —
// only a message that is one of these is a usage limit — because a message
// is a provider's words and anything looser would park runs on a sentence
// that merely mentions a limit.
var usageLimit = regexp.MustCompile(`^(5-hour|Weekly|Monthly) usage limit reached\. Resets in ([0-9a-z ]+)\.|^Subscription quota exceeded\. Retry in ([0-9a-z ]+)\.`)

// classify reads a failed prompt from OpenCode's structure first — the ACP
// code and errorName — and from its words only for the usage limits Zen
// names nowhere else.
func classify(e *jsonrpc.RPCError, spec adapter.Spec) acp.Failure {
	var d errorData
	json.Unmarshal(e.Data, &d)
	msg := strings.TrimPrefix(e.Message, "Internal error: ")
	switch {
	case e.Code == codeAuthRequired:
		provider := firstNonEmpty(d.ProviderID, "its provider")
		return acp.Failure{
			Class:        adapter.ClassHarness,
			Message:      fmt.Sprintf("OpenCode's credential for %s was refused — as the runner's user, log it in again with `opencode auth login` (%s shows what it has)", provider, spec.HarnessCheck("opencode", "auth", "list")),
			AuthRejected: true,
		}
	case d.ErrorName == "ContextOverflowError":
		return acp.Failure{Class: adapter.ClassPromptTooLong, Message: "OpenCode: " + msg}
	}
	if m := usageLimit.FindStringSubmatch(msg); m != nil {
		window, in := strings.ToLower(m[1]), m[2]
		if window == "" {
			window, in = "subscription", m[3]
		}
		l := &adapter.Limit{Window: window}
		if d, ok := resetIn(in); ok {
			l.ResetAt = time.Now().Add(d).UTC().Truncate(time.Minute)
		}
		return acp.Failure{Class: adapter.ClassUsageLimit, Message: "OpenCode: " + msg, Limit: l}
	}
	return acp.Failure{Class: adapter.ClassHarness, Message: "OpenCode failed the turn: " + msg}
}

// resetIn reads Zen's retry time: "2 days", "3hr 20min", "45min".
func resetIn(s string) (time.Duration, bool) {
	var total time.Duration
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, false
	}
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		unit := strings.TrimLeft(f, "0123456789")
		n, err := strconv.Atoi(strings.TrimSuffix(f, unit))
		if err != nil {
			return 0, false
		}
		if unit == "" && i+1 < len(fields) {
			i++
			unit = fields[i]
		}
		switch unit {
		case "day", "days":
			total += time.Duration(n) * 24 * time.Hour
		case "hr":
			total += time.Duration(n) * time.Hour
		case "min":
			total += time.Duration(n) * time.Minute
		default:
			return 0, false
		}
	}
	return total, true
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
