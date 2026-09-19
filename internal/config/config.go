package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Config is config.toml: everything the owner decides about this runner. The
// hub decides nothing here — permission mode, sandbox and caps are the owner's,
// and no protocol field can widen them (decision 0015).
type Config struct {
	Name        string                   `toml:"name,omitempty"`
	Labels      []string                 `toml:"labels,omitempty"`
	Capacity    int                      `toml:"capacity"`
	Harness     map[string]HarnessConfig `toml:"harness,omitempty"`
	Connections []Connection             `toml:"connection,omitempty"`
	Sessions    SessionsConfig           `toml:"sessions"`
	Supervise   SuperviseConfig          `toml:"supervise"`
	Drain       DrainConfig              `toml:"drain"`
	Workdirs    WorkdirsConfig           `toml:"workdirs"`
}

// HarnessConfig is the owner's settings for one harness.
type HarnessConfig struct {
	// PermissionMode is claude's --permission-mode. Unset means
	// bypassPermissions: runs are unattended and auto-approve (0015), and
	// Claude's own default would deny every tool that needs a prompt.
	PermissionMode string `toml:"permission_mode,omitempty"`
	// Sandbox and Approval are codex's sandbox mode and approval policy.
	// Unset means danger-full-access and never: runs are unattended, and the
	// owner's machine is the boundary (0015, 0036). A policy that asks is
	// answered no — nobody is there to approve.
	Sandbox  string   `toml:"sandbox,omitempty"`
	Approval string   `toml:"approval,omitempty"`
	Cap      int      `toml:"cap,omitempty"`      // 0 = only the runner's capacity limits it
	Accounts []string `toml:"accounts,omitempty"` // failover order
}

// Connection is one hub this runner is registered with. Its credential lives in
// a separate 0600 file, never here, so config.toml can be shown and shared.
type Connection struct {
	Name string `toml:"name"`
	URL  string `toml:"url"`
	Cap  int    `toml:"cap,omitempty"`
}

// SessionsConfig governs reclaiming workdirs (decision 0011).
type SessionsConfig struct {
	IdleTTL Duration `toml:"idle_ttl"`
}

// SuperviseConfig holds the owner's watchdog defaults; a run may lower them.
type SuperviseConfig struct {
	Inactivity Duration `toml:"inactivity"`
}

// DrainConfig governs the runner's way down (decision 0029).
type DrainConfig struct {
	// Wait is how long a drain lets the runs held finish on their own before
	// cancelling them down the cancel ladder. "0s" cancels them at once.
	Wait Duration `toml:"wait"`
}

// WorkdirsConfig governs how a run's sources become its workdir (decisions
// 0033 and 0034).
type WorkdirsConfig struct {
	// Roots are the directories a hub's run may reach on this machine: a path
	// source, or a git source whose URL is a local path or file://, is taken
	// only when it resolves inside one of them. None means none is taken.
	Roots []string `toml:"roots,omitempty"`
	// GitTimeout bounds each git command a workdir needs — a first clone of a
	// large repository is the longest.
	GitTimeout Duration `toml:"git_timeout"`
	// SetupTimeout bounds a repository's .worktree/setup.
	SetupTimeout Duration `toml:"setup_timeout"`
}

// Duration is a time.Duration written as "336h" in TOML.
type Duration struct{ time.Duration }

func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("%q is not a duration like \"336h\" or \"30m\"", b)
	}
	d.Duration = v
	return nil
}

// Defaults. The inactivity watchdog errs long: Multica's grew from 5 minutes to
// 2 hours because "force-stopping a healthy run throws away the work".
//
// The drain wait is long enough for a typical run to end on its own. A service
// manager's stop timeout must be longer than it plus the cancel ladder, or the
// manager kills the runner mid-drain and the runs held end lost.
const (
	DefaultCapacity   = 4
	DefaultIdleTTL    = 14 * 24 * time.Hour
	DefaultInactivity = 30 * time.Minute
	DefaultDrainWait  = 30 * time.Minute
	// A whole git command, start to finish, not a silence: a first clone of a
	// large repository over a slow link takes minutes, and one still going
	// after ten is more likely wedged than busy.
	DefaultGitTimeout = 10 * time.Minute
	// A setup hook installs dependencies and builds; gpiwt measured the
	// slowest repositories at a few minutes.
	DefaultSetupTimeout = 15 * time.Minute
)

// Default is the config a new profile starts with.
func Default() Config {
	return Config{
		Capacity:  DefaultCapacity,
		Sessions:  SessionsConfig{IdleTTL: Duration{DefaultIdleTTL}},
		Supervise: SuperviseConfig{Inactivity: Duration{DefaultInactivity}},
		Drain:     DrainConfig{Wait: Duration{DefaultDrainWait}},
		Workdirs:  WorkdirsConfig{GitTimeout: Duration{DefaultGitTimeout}, SetupTimeout: Duration{DefaultSetupTimeout}},
	}
}

// Load reads config.toml. A missing file is the default config, not an error —
// a fresh machine must be able to run `yad doctor` before anything is set up.
// Unknown keys are refused: a typo in a permission setting must not be silently
// ignored.
func Load(p Paths) (Config, error) {
	c := Default()
	b, err := os.ReadFile(p.ConfigFile())
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	dec := toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return c, fmt.Errorf("%s: unknown setting:\n%s", p.ConfigFile(), strict.String())
		}
		return c, fmt.Errorf("%s: %w", p.ConfigFile(), err)
	}
	return c, c.Validate()
}

// Save writes config.toml at 0600 — it names hubs and accounts, which is enough
// to be worth keeping private.
func Save(p Paths, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := toml.Marshal(c)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.Config, 0o700); err != nil {
		return err
	}
	return writePrivate(p.ConfigFile(), b)
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func validName(s string) error {
	if !nameRE.MatchString(s) {
		return fmt.Errorf("name %q: use lowercase letters, digits, dash and underscore", s)
	}
	return nil
}

// Validate checks what the decoder cannot.
func (c Config) Validate() error {
	var errs []error
	if c.Capacity < 1 {
		errs = append(errs, fmt.Errorf("capacity must be at least 1, got %d", c.Capacity))
	}
	if c.Drain.Wait.Duration < 0 {
		errs = append(errs, fmt.Errorf("drain.wait must not be negative, got %s — it is how long a drain lets runs finish, like \"30m\"", c.Drain.Wait.Duration))
	}
	for _, r := range c.Workdirs.Roots {
		if !filepath.IsAbs(r) {
			errs = append(errs, fmt.Errorf("workdirs.roots: %q is not an absolute path — write it in full, like \"/home/me/src\"", r))
		} else if filepath.Clean(r) == "/" {
			errs = append(errs, errors.New("workdirs.roots: \"/\" would let a hub reach every directory on this machine — name the directories runs may use"))
		}
	}
	for _, d := range []struct {
		name string
		v    Duration
	}{{"workdirs.git_timeout", c.Workdirs.GitTimeout}, {"workdirs.setup_timeout", c.Workdirs.SetupTimeout}} {
		if d.v.Duration < 0 {
			errs = append(errs, fmt.Errorf("%s must not be negative, got %s", d.name, d.v.Duration))
		}
	}
	for id, h := range c.Harness {
		if h.Cap < 0 {
			errs = append(errs, fmt.Errorf("harness.%s.cap must not be negative", id))
		}
		for _, a := range h.Accounts {
			if err := validName(a); err != nil {
				errs = append(errs, fmt.Errorf("harness.%s.accounts: %w", id, err))
			}
		}
		if dup := firstDuplicate(h.Accounts); dup != "" {
			errs = append(errs, fmt.Errorf("harness.%s.accounts lists %q twice", id, dup))
		}
	}
	var names []string
	for _, conn := range c.Connections {
		if err := validName(conn.Name); err != nil {
			errs = append(errs, fmt.Errorf("connection: %w", err))
		}
		if conn.URL == "" {
			errs = append(errs, fmt.Errorf("connection %q has no url", conn.Name))
		} else if err := CheckHubURL(conn.URL); err != nil {
			errs = append(errs, fmt.Errorf("connection %q: %w", conn.Name, err))
		}
		if conn.Cap < 0 {
			errs = append(errs, fmt.Errorf("connection %q: cap must not be negative", conn.Name))
		}
		names = append(names, conn.Name)
	}
	if dup := firstDuplicate(names); dup != "" {
		errs = append(errs, fmt.Errorf("connection %q is listed twice", dup))
	}
	return errors.Join(errs...)
}

func firstDuplicate(xs []string) string {
	seen := make([]string, 0, len(xs))
	for _, x := range xs {
		if slices.Contains(seen, x) {
			return x
		}
		seen = append(seen, x)
	}
	return ""
}
