package v1

import "time"

// Capabilities is the capability document: what a runner advertises. Every
// field is derived from the machine or the owner's config, so two runners with
// the same document are interchangeable for routing.
type Capabilities struct {
	RunnerID         string          `json:"runner_id"`
	Name             string          `json:"name"`
	YadVersion       string          `json:"yad_version"`
	OS               string          `json:"os"`
	Arch             string          `json:"arch"`
	Labels           []string        `json:"labels,omitempty"`
	Harnesses        []HarnessReport `json:"harnesses"`
	HostTools        []HostTool      `json:"host_tools,omitempty" doc:"The non-harness tools a run may need, as they exist on this runner. A tool is usable when present is true and error is empty, and, for a tool that has a login, when logged_in is true as well. A tool that is missing or broken is reported rather than left out."`
	Capacity         Capacity        `json:"capacity"`
	ProtocolFeatures []string        `json:"protocol_features,omitempty"`
	ObservedAt       time.Time       `json:"observed_at"`
}

// HarnessReport is one harness as it exists on the runner. Accounts appear by
// label and state only; credentials never leave the machine.
type HarnessReport struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Kind    string `json:"kind" enum:"first-class,recognised"`
	Present bool   `json:"present"`
	Version string `json:"version,omitempty"`
	// Error is the runner's own words, never the harness's: a child's output
	// and the path it was started from stay on the machine (DEV-60).
	Error    string          `json:"error,omitempty" doc:"Why this harness cannot take runs, and the next action for whoever owns the machine. Written by the runner: it never quotes what the harness printed and never names a path on the machine."`
	Models   []string        `json:"models,omitempty"`
	Accounts []AccountReport `json:"accounts,omitempty"`
	// Warnings are what the runner found wrong with a harness it can still
	// drive — an installed Codex whose app-server protocol differs from the
	// one the adapter was built against. A hub may show them or prefer a
	// runner without; they never make a harness refuse runs, which Error does.
	// The same rule as Error binds them (DEV-67).
	Warnings []string `json:"warnings,omitempty" doc:"What is wrong with a harness the runner can still drive, each with the next action for whoever owns the machine. Never a reason to refuse runs. Written by the runner: it never quotes what the harness printed and never names a path on the machine."`
}

// AccountState is what a hub may know about an account, and the whole of it.
// The words are DOMAIN.md's: an account is free, limited until a reset, or
// needs login.
type AccountState string

const (
	// AccountFree takes runs.
	AccountFree AccountState = "free"
	// AccountLimited is at a usage limit until LimitedUntil.
	AccountLimited AccountState = "limited"
	// AccountNeedsLogin cannot run a turn until the owner logs it in again:
	// its harness home holds no working login, or the home is not there at
	// all. The owner runs `yad account add` at the machine. Skipped for
	// claiming exactly as a limited account is, and never an error that stops
	// a runner registering.
	AccountNeedsLogin AccountState = "needs_login"
)

// AccountStates lists the closed set.
func AccountStates() []AccountState {
	return []AccountState{AccountFree, AccountLimited, AccountNeedsLogin}
}

// AccountReport is an account's public face: its label, its state, and until
// when a limit lasts. A label is not a secret and a credential is — nothing
// else from an account's home is reportable, and none of it appears here.
type AccountReport struct {
	Label string `json:"label"`
	// omitempty keeps state out of the schema's required list. Every runner
	// that has this field always sets it — Report substitutes free for an
	// empty one — so the wire is unchanged; what it buys is that a runner
	// from before this field still validates against a hub generated after
	// it. Hubs update centrally and runners sit on other people's machines,
	// so that is the direction that matters, and §2's rule is that a field
	// added within v1 never breaks an older runner.
	State        AccountState `json:"state,omitempty" enum:"free,limited,needs_login"`
	LimitedUntil *time.Time   `json:"limited_until,omitempty"`
	// Windows is every usage window the harness last told the runner about,
	// whether or not the account is at a limit: a hub seeing one at 96% knows
	// why a runner will stop claiming soon, and one at 100% with a reset says
	// why it has stopped (decision 0039).
	//
	// Reported from every run, so a window is as fresh as the last turn that
	// ran on the account and no fresher. Absent means no run has yet heard a
	// window from this harness, never that the account has no limits.
	Windows []AccountWindow `json:"windows,omitempty"`
}

// AccountWindow is one usage window of one account, as its harness names it:
// Claude's `five_hour` and `seven_day`, Codex's `primary` and `secondary`.
//
// The names are each harness's own and are deliberately not unified. DOMAIN.md
// defines a usage limit as "Claude's five-hour and weekly limits, Codex's
// primary and secondary windows" — the vocabulary is harness-specific in the
// domain model, and a name invented here would be a third vocabulary that
// matches neither harness's own reporting.
type AccountWindow struct {
	Name string `json:"name" doc:"The window as its harness names it: five_hour or seven_day for Claude, primary or secondary for Codex."`
	// UsedPercent is 0-100. The unit is in the name because the harnesses
	// disagree: Codex reports a percentage and Claude a 0-1 fraction, and a
	// factor of a hundred is invisible in a number alone.
	UsedPercent float64 `json:"used_percent" doc:"How much of the window is used, 0-100."`
	// ResetsAt is absent when the harness reported the window's use without
	// saying when it refills.
	ResetsAt *time.Time `json:"resets_at,omitempty" doc:"When the window refills, if the harness said."`
}

// HostTool is a non-harness executable a run may need — git, gh, docker.
//
// A tool that is present is not necessarily usable: a gh nobody has signed in
// cannot open a pull request, and a docker whose daemon is down cannot run a
// container. A hub routing on a tool wants `present`, no `error`, and
// `logged_in` where the tool has a login.
type HostTool struct {
	ID      string `json:"id" doc:"The tool: git, gh or docker."`
	Present bool   `json:"present" doc:"The binary was found on the runner. Present is not usable: see error and logged_in."`
	Version string `json:"version,omitempty" doc:"The first line the tool prints for its version."`
	// LoggedIn is nil when the tool has no notion of a login, and nil too when
	// it has one and the runner could not find out — Error says why.
	LoggedIn *bool `json:"logged_in,omitempty" doc:"Whether the tool is signed in. Absent for a tool with no login, and for one whose login state the runner could not find out, where error says why."`
	// LoginHosts are the hosts the tool is signed in to — github.com, a GitHub
	// Enterprise hostname, or both — so a hub can tell a runner that can reach
	// its repositories from one that cannot. Never who it is signed in as: an
	// account name is the machine owner's, not the hub's.
	LoginHosts []string `json:"login_hosts,omitempty" doc:"Every host the tool is signed in to, such as github.com or a GitHub Enterprise hostname; a gh signed in to two reports both. Never the account it is signed in as."`
	// Error is what is wrong with this tool on the runner: a path configured
	// for it that names nothing, a probe that timed out, a binary that would
	// not run, a Docker daemon that is not answering. It carries the next
	// action, and it never stops a runner registering — absence and breakage
	// are both facts a hub routes around. It can accompany present: false: a
	// tool the owner configured a path for, which is not there, is both absent
	// and worth explaining.
	Error string `json:"error,omitempty" doc:"What is wrong with this tool on the runner: a path configured for it that names nothing, a probe that timed out, a binary that would not run, a Docker daemon that is not answering. May accompany present: false, when a configured path names nothing. Carries the next action. A runner with one still registers."`
}

// Capacity is how many runs a runner executes at once: one pool, with the
// owner's optional per-harness caps. Per-connection caps are the runner's
// business and are not advertised.
type Capacity struct {
	Total     int            `json:"total"`
	ByHarness map[string]int `json:"by_harness,omitempty"`
}

// RegisterRequest is sent once, with the registration token as the bearer.
type RegisterRequest struct {
	Capabilities Capabilities `json:"capabilities"`
}

// RegisterResponse carries the runner credential — the only secret a runner
// keeps — and the hub's own features and timings.
type RegisterResponse struct {
	RunnerCredential string   `json:"runner_credential"`
	HubFeatures      []string `json:"hub_features,omitempty"`
	SyncIntervalMS   int      `json:"sync_interval_ms"`
	LeaseMS          int      `json:"lease_ms"`
	MinVersion       string   `json:"min_version,omitempty"`
}

// DeregisterRequest retires a runner's credential; runs it still holds become
// lost on the hub's side.
type DeregisterRequest struct {
	Reason string `json:"reason,omitempty"`
}

// Ack is the empty success body, so every response is a JSON object a hub can
// extend later without breaking decoders.
type Ack struct {
	OK bool `json:"ok"`
}
