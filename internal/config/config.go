package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
}

// HarnessConfig is the owner's settings for one harness.
type HarnessConfig struct {
	PermissionMode string   `toml:"permission_mode,omitempty"` // claude: --permission-mode
	Sandbox        string   `toml:"sandbox,omitempty"`         // codex: sandbox policy
	Approval       string   `toml:"approval,omitempty"`        // codex: approval policy
	Cap            int      `toml:"cap,omitempty"`             // 0 = only the runner's capacity limits it
	Accounts       []string `toml:"accounts,omitempty"`        // failover order
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
const (
	DefaultCapacity   = 4
	DefaultIdleTTL    = 14 * 24 * time.Hour
	DefaultInactivity = 30 * time.Minute
)

// Default is the config a new profile starts with.
func Default() Config {
	return Config{
		Capacity:  DefaultCapacity,
		Sessions:  SessionsConfig{IdleTTL: Duration{DefaultIdleTTL}},
		Supervise: SuperviseConfig{Inactivity: Duration{DefaultInactivity}},
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
