package v1

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Run is one turn to execute against one session. It names its session,
// harness and model explicitly; none is inferred from runner configuration.
//
// There is deliberately no permission, sandbox or tool-policy field: those are
// the runner owner's configuration, and a hub must not be able to widen them.
type Run struct {
	RunID   string     `json:"run_id" doc:"The run's id, chosen by the hub and unique within it. It is the {run} in the events and result paths, and a runner never runs the same id twice."`
	Session SessionRef `json:"session" doc:"The session the run belongs to."`
	Harness string     `json:"harness" doc:"The harness to run, by its id in the capability document: claude or codex today. Offer a run only to a runner whose document shows this harness first-class, present and without an error."`
	// Model is required: a run names its model, and a harness left to its own
	// default runs something different, and differently priced, from what the
	// hub asked for.
	Model string `json:"model" doc:"The model the harness runs, in the harness's own terms: an alias such as haiku or a full name such as claude-haiku-4-5 or gpt-5.1-codex. Required; the runner never falls back to a default."`
	// Effort is a string and not an enum, as Model is: the levels are each
	// harness's own, they differ by model, and they grow with harness
	// releases — an enum here would be a v1 enum that had to grow with them
	// (decision 0047), and a runner that checked names would refuse a level
	// its harness had just learned. The harness decides; the runner passes
	// the word through. A hub offers a run carrying one only to a runner
	// advertising the "effort" feature for the run's harness: any other would
	// run it at the harness's default, and nothing would say so (decisions
	// 0049, 0069).
	Effort  string   `json:"effort,omitempty" doc:"How hard the harness thinks, in the harness's own terms, as model is: low, medium, high, xhigh or max for Claude Code; for Codex, one of the reasoning levels its model lists, such as low, medium, high or xhigh. Not a closed set, and the runner checks no name: a level the harness does not take fails the run with the harness's own error. It must still look like a level — at most 64 bytes of letters, digits, - and _ — or the run is refused whole. Absent: the harness's default. Offer a run carrying one only to a runner advertising effort for its harness: in that harness's features when protocol_features lists harness_features, and in protocol_features otherwise."`
	Brief   Brief    `json:"brief" doc:"What the run is told."`
	Sources []Source `json:"sources,omitempty" doc:"What the session's workdir is built from, used by the run that opens the session; a run continuing it names the same sources or none. Absent: the workdir starts empty. Each source sets exactly one of git, a repository checked out as a worktree, or path, an absolute directory on the runner's machine worked in place, taken only inside the directories its owner allows (their home unless they listed others), never at a runner whose capability document says path_sources is false, and otherwise failed with class source_refused."`
	Grants  []Grant  `json:"grants,omitempty" doc:"Short-lived secrets for this run alone, delivered to the harness process and deleted when the run ends. Names follow rules the schema cannot state; a run breaking one is refused whole."`
	// StartAt is a one-shot moment the run must not start before, like an email
	// API's send_at. There is no recurrence anywhere in the protocol. A hub
	// offers a run carrying one only to a runner advertising the "start_at"
	// feature, until the moment has passed: any other runner would start it on
	// arrival.
	StartAt      *time.Time `json:"start_at,omitempty" doc:"A moment the run must not start before, once. While it is ahead, offer the run only to a runner advertising start_at, which claims it and holds it; once it has passed, any runner may take it."`
	MaxWaitMS    int64      `json:"max_wait_ms,omitempty" doc:"The most milliseconds the run may spend waiting for a free account, summed over every wait. Past it the run ends timed_out with error class max_wait_exceeded. Absent or 0: no cap."`
	WallClockMS  int64      `json:"wall_clock_ms,omitempty" doc:"The most milliseconds the harness may run, summed over the run's turns and not counting waits. Past it the run is stopped and ends timed_out with class wall_clock_timeout. Absent or 0: no cap."`
	InactivityMS int64      `json:"inactivity_ms,omitempty" doc:"Stop the run when the harness emits no event for this many milliseconds; it ends timed_out with class inactivity_timeout. It can only lower the owner's own timeout (30 minutes unless they changed it), never raise it. Absent or 0: the owner's."`
}

// SessionMode is how the runner holds the harness process for a session.
type SessionMode string

const (
	SessionPerRun SessionMode = "per_run"
	// SessionLive is reserved: refused until a runner advertises the
	// "live_sessions" feature.
	SessionLive SessionMode = "live"
)

// SessionRef names the session a run belongs to.
type SessionRef struct {
	ID   string      `json:"id" doc:"The session's id, chosen by the hub. A session lives on the runner that claimed its first run and is continued only there."`
	New  bool        `json:"new" doc:"true for the run that opens the session, false for every later run. A runner refuses new true for a session id it already has, and new false for one it does not hold."`
	Mode SessionMode `json:"mode,omitempty" enum:"per_run,live" doc:"per_run, the default: a fresh harness process for each run, resuming the session's conversation. live is reserved: offer it only to a runner advertising live_sessions, which none does yet."`
	// ForkFrom opens a session whose conversation starts as a copy of
	// another's, which goes on untouched (decision 0065). It rides on the
	// run that opens the session and on no later one: from then on the fork
	// is a session like any other, resuming its own conversation. A hub
	// offers it only to the runner holding the session it names, and only
	// while that runner advertises fork for the run's harness (decision 0069).
	ForkFrom string `json:"fork_from,omitempty" doc:"Only with new true: open this session as a fork of the session named here — a new conversation that starts from a copy of that session's, as far as the harness has written it, and diverges from there, while the session forked goes on unchanged. Offer the run only to the runner that holds that session, and only while it advertises fork for the run's harness — in that harness's features when protocol_features lists harness_features; the runner refuses it (class refused) for a session it does not hold for this hub, of another harness, or closed or closing. The fork gets its own workdir from its own sources, as any new session does; nothing of the forked session's workdir comes with it. Absent: the session starts with an empty conversation."`
}

// Brief is what the run is told: context goes into the harness's system prompt
// so it survives compaction; the instruction is the one user turn.
type Brief struct {
	Context     string `json:"context,omitempty" doc:"Standing background for this run, continuing runs included: appended to Claude's system prompt, and Codex's developer instructions. It outlasts a compaction, and it is this run's, not the session's — send it whole on every run of the session."`
	Instruction string `json:"instruction" doc:"The run's one user message: what to do. Required."`
}

// Source is one input the workdir is built from: a git repository or an
// existing local path. Exactly one field is set.
type Source struct {
	// Neither field has a doc tag, and Run.Sources describes both instead:
	// the document's oneOf branches copy these two property schemas, and
	// oasdiff reads a description added inside a branch as the branch
	// removed and another added — a breaking change for a sentence.
	Git  *GitSource `json:"git,omitempty"`
	Path string     `json:"path,omitempty"`
}

// GitSource is a repository to check out as a worktree on Branch, cut from Base.
type GitSource struct {
	URL    string `json:"url" doc:"The repository: an https or ssh URL, fetched with the machine's own credentials and never with a password in the URL, or an absolute path or file:// URL of a repository on the runner's machine, inside the directories its owner allows and never at a runner whose capability document says path_sources is false. Required."`
	Base   string `json:"base,omitempty" doc:"The ref a new branch is cut from. Absent: the repository's default branch."`
	Branch string `json:"branch,omitempty" doc:"The branch the run works on, checked out as a worktree of the repository. Absent: yad/<connection>/<session>, named after the session."`
}

// GrantDelivery is how a grant reaches the harness. Never argv.
type GrantDelivery string

const (
	GrantEnv  GrantDelivery = "env"
	GrantFile GrantDelivery = "file"
)

// Grant is a short-lived secret scoped to one run.
type Grant struct {
	Name  string        `json:"name" doc:"An environment variable name, [A-Za-z_][A-Za-z0-9_]*, which is also the file name for a file grant. Not PATH or HOME, nor a name beginning LD_ or DYLD_, nor one that picks a harness's credential or home — the whole list: ANTHROPIC_AUTH_TOKEN, ANTHROPIC_API_KEY, CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_PROFILE, ANTHROPIC_FEDERATION_RULE_ID, ANTHROPIC_ORGANIZATION_ID, ANTHROPIC_CONFIG_DIR, CLAUDE_CONFIG_DIR, CLAUDE_SECURESTORAGE_CONFIG_DIR, ANTHROPIC_BASE_URL, ANTHROPIC_CUSTOM_HEADERS, any name beginning CLAUDE_CODE_USE_, CODEX_HOME, OPENAI_API_KEY, CODEX_API_KEY, CODEX_ACCESS_TOKEN, OPENAI_BASE_URL, CODEX_REFRESH_TOKEN_URL_OVERRIDE and AWS_BEARER_TOKEN_BEDROCK, whatever the run's harness. Every name is matched in any case. No two grants may share a name, and no two file grants may differ only by case."`
	Value string        `json:"value" doc:"The secret. Never logged or shown; a hub should keep it only until the run is terminal."`
	As    GrantDelivery `json:"as" enum:"env,file" doc:"env: set as the variable name=value in the harness's environment. file: written to a 0600 file named name outside the workdir, whose path the harness reads from the variable name."`
}

// RunState is where a run is. The set is closed and mirrored by DOMAIN.md's
// **Run state** entry; a parity test keeps them equal.
type RunState string

const (
	RunClaimed   RunState = "claimed"
	RunPreparing RunState = "preparing"
	RunRunning   RunState = "running"
	// RunWaiting holds no process: the run is parked on a usage limit with a
	// resume time, and survives a runner restart.
	RunWaiting   RunState = "waiting"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	RunCancelled RunState = "cancelled"
	RunTimedOut  RunState = "timed_out"
	RunLost      RunState = "lost"
)

// RunStates lists the closed set in lifecycle order.
func RunStates() []RunState {
	return []RunState{RunClaimed, RunPreparing, RunRunning, RunWaiting, RunSucceeded, RunFailed, RunCancelled, RunTimedOut, RunLost}
}

// IsTerminal reports whether a run in this state is finished for good. A run
// reaches exactly one terminal state and never leaves it.
func (s RunState) IsTerminal() bool {
	switch s {
	case RunSucceeded, RunFailed, RunCancelled, RunTimedOut, RunLost:
		return true
	}
	return false
}

// Valid reports whether s is in the closed set.
func (s RunState) Valid() bool {
	for _, v := range RunStates() {
		if s == v {
			return true
		}
	}
	return false
}

// Validate checks what the schema cannot express, and what both sides must
// check anyway: a hub before offering a run, a runner before claiming one.
func (r Run) Validate() error {
	var errs []error
	for _, f := range []struct{ name, v string }{
		{"run_id", r.RunID}, {"session.id", r.Session.ID}, {"harness", r.Harness},
		{"model", r.Model}, {"brief.instruction", r.Brief.Instruction},
	} {
		if f.v == "" {
			errs = append(errs, fmt.Errorf("%s is required", f.name))
		}
	}
	if r.Effort != "" && !effortPattern.MatchString(r.Effort) {
		errs = append(errs, fmt.Errorf("effort %q is not an effort level: send at most %d letters, digits, - or _, such as high, or no effort for the harness's default", clip(r.Effort, maxEffortLen), maxEffortLen))
	}
	switch r.Session.Mode {
	case "", SessionPerRun, SessionLive:
	default:
		errs = append(errs, fmt.Errorf("session.mode %q is not per_run or live", r.Session.Mode))
	}
	switch {
	case r.Session.ForkFrom == "":
	case !r.Session.New:
		// A continuing run resumes its session's own conversation; one naming
		// a session to fork would be asking for two conversations at once.
		errs = append(errs, fmt.Errorf("session.fork_from is only for the run that opens a session: send it with session.new true, or leave it out to continue session %s", r.Session.ID))
	case r.Session.ForkFrom == r.Session.ID:
		errs = append(errs, fmt.Errorf("session.fork_from names the session the run opens, %s: name the session to fork", r.Session.ID))
	}
	for i, src := range r.Sources {
		if err := src.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("sources[%d]: %w", i, err))
		}
	}
	seen := map[string]bool{}
	// A file grant's name is also its file's name, and macOS folds case: two
	// that differ only by case are written to one file, in order, so the
	// second truncates the first and both names end up pointing at the second
	// grant's value. Env grants are deliberately not held to this — FOO
	// and foo are two variables on linux and darwin alike, and both arrive
	// intact. The filesystem and the environment have different rules, so the
	// asymmetry is the point and not something to tidy into agreement.
	folded := map[string]string{}
	for i, g := range r.Grants {
		if err := g.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("grants[%d]: %w", i, err))
		}
		// Two grants by one name: one would silently replace the other.
		if seen[g.Name] {
			errs = append(errs, fmt.Errorf("grants[%d]: grant name %s is given twice", i, g.Name))
		}
		seen[g.Name] = true
		if g.As == GrantFile {
			key := strings.ToLower(g.Name)
			if first, ok := folded[key]; ok && first != g.Name {
				errs = append(errs, fmt.Errorf("grants[%d]: file grants %s and %s differ only by case and would share one file — rename one", i, first, g.Name))
			}
			folded[key] = g.Name
		}
	}
	return errors.Join(errs...)
}

// maxEffortLen bounds an effort. The longest level either harness has is
// six bytes; the bound leaves room for levels not yet invented and none for
// text. It is checked here, before any adapter sees the word, because an
// adapter learns of a level its harness refused only from what the harness
// says about it — Claude's refusal is a warning on stderr quoting the word,
// and a word longer than the runner keeps of stderr pushes the warning out of
// sight, so the run would go ahead at the harness's default (DEV-124).
const maxEffortLen = 64

// effortPattern is what a level looks like in every harness so far: low,
// xhigh, max, ultra.
var effortPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// clip shortens what an error quotes back, so a refusal of a long word is
// not itself long.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Validate enforces exactly one of git or path. The generated schema says it
// too, as a oneOf, but this is where the rule is enforced: huma validates
// against its own registry rather than that document, and a TypeScript union
// generated from a oneOf is not exclusive. The schema describes; this refuses.
func (s Source) Validate() error {
	switch {
	case s.Git != nil && s.Path != "":
		return errors.New("set git or path, not both")
	case s.Git != nil && s.Git.URL == "":
		return errors.New("git.url is required")
	case s.Git == nil && s.Path == "":
		return errors.New("set git or path")
	}
	return nil
}
