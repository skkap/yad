package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
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
	Accounts []string `toml:"accounts,omitempty"` // which accounts take part; the order only breaks ties (0039)
}

// Connection is one hub this runner is registered with. Its credential lives in
// a separate 0600 file, never here, and CheckHubURL refuses a URL carrying a
// user or password — at connect, at save and at load — so config.toml holds no
// secret and can be shown and shared.
type Connection struct {
	Name string `toml:"name"`
	URL  string `toml:"url"`
	Cap  int    `toml:"cap,omitempty"`
	// ManageAccounts false stops this hub adding and removing the runner's
	// accounts (decision 0057); absent lets it, as the owner trusts the hubs
	// it connects (0038). Read at start, like the rest of a connection.
	ManageAccounts *bool `toml:"manage_accounts,omitempty"`
}

// MayManageAccounts is whether this hub may add and remove accounts.
func (c Connection) MayManageAccounts() bool {
	return c.ManageAccounts == nil || *c.ManageAccounts
}

// SessionsConfig governs reclaiming workdirs (decisions 0011 and 0035).
type SessionsConfig struct {
	// IdleTTL closes a session nothing has run in for this long and reclaims
	// its workdir. "0s" keeps idle sessions until their hub or the owner
	// closes them.
	IdleTTL Duration `toml:"idle_ttl"`
	// DiskFloor is the free space the runner keeps on the disk under its
	// workdirs: below it, idle sessions close longest idle first until it is
	// met or none is left. A session with a run held is never among them.
	// "0" turns it off.
	DiskFloor ByteSize `toml:"disk_floor"`
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
	// only when it resolves inside one of them. Unset is the owner's home
	// directory — EffectiveRoots, not this field, is what a runner reads.
	Roots []string `toml:"roots,omitempty"`
	// PathSources false refuses every source that reaches this machine — a
	// path source, and a git source whose URL is a local path or file:// —
	// whatever Roots says (decision 0062). Unset or true takes them inside
	// the roots. A toggle rather than `roots = []`, which the omitempty above
	// cannot keep through a rewrite of config.toml: absent, the home default
	// stays unwritten, and false survives every writer as what it says.
	PathSources *bool `toml:"path_sources,omitempty"`
	// GitTimeout bounds each git command a workdir needs — a first clone of a
	// large repository is the longest.
	GitTimeout Duration `toml:"git_timeout"`
	// SetupTimeout bounds a repository's .worktree/setup.
	SetupTimeout Duration `toml:"setup_timeout"`
}

// AllowsPathSources is whether a run may name a source on this machine at
// all; EffectiveRoots then says where.
func (w WorkdirsConfig) AllowsPathSources() bool {
	return w.PathSources == nil || *w.PathSources
}

// EffectiveRoots is what a runner reads: the roots the owner listed, or their
// home directory when they listed none. The owner trusts the hubs it connects
// (decision 0038), and 0033's refuse-everything default cost them every path
// source before they had written any configuration at all.
//
// The default is resolved here and never written to config.toml: a home
// directory is the machine's, not the configuration's, and a config saved on
// one machine would carry the other's path.
//
// A machine with no usable home allows nothing, and the refusal names
// [workdirs] roots as before. The failure direction matters: a default that
// became "every directory" when a lookup failed is how this shape of bug is
// usually written. "/" is refused for the same reason Validate refuses it as a
// configured root — it is every directory on the machine.
//
// An empty list is the same as none at all: the field is omitempty, so a
// `roots = []` an owner wrote would not survive the next `yad connect`
// rewriting config.toml anyway. An owner who wants a run to reach less than
// their home names the directories it may use; one who wants it to reach
// nothing sets path_sources = false (AllowsPathSources).
func (w WorkdirsConfig) EffectiveRoots() []string {
	if len(w.Roots) > 0 {
		return w.Roots
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" || !filepath.IsAbs(home) || isFilesystemRoot(home) {
		return nil
	}
	return []string{home}
}

// isFilesystemRoot resolves symlinks before deciding, because inRoots resolves
// its roots too: a home directory that is a link to "/" passes a check on the
// written path and then reaches every directory on the machine, which is the
// one thing this default must not do.
func isFilesystemRoot(p string) bool {
	if filepath.Clean(p) == "/" {
		return true
	}
	r, err := filepath.EvalSymlinks(p)
	return err == nil && filepath.Clean(r) == "/"
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

// ByteSize is a number of bytes written as "5GiB" or "500MB" in TOML.
type ByteSize int64

var byteUnits = []struct {
	suffix string
	n      int64
}{
	// Longest suffixes first, so "GiB" is not read as "B".
	{"TiB", 1 << 40}, {"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
	{"TB", 1e12}, {"GB", 1e9}, {"MB", 1e6}, {"KB", 1e3}, {"B", 1},
}

// MarshalText writes the size in the largest unit that holds it exactly.
func (b ByteSize) MarshalText() ([]byte, error) {
	if b == 0 {
		return []byte("0"), nil
	}
	best := byteUnits[len(byteUnits)-1]
	for _, u := range byteUnits {
		if int64(b)%u.n == 0 && u.n > best.n {
			best = u
		}
	}
	return []byte(strconv.FormatInt(int64(b)/best.n, 10) + best.suffix), nil
}

func (b *ByteSize) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	mult := int64(1)
	for _, u := range byteUnits {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.n
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/mult {
		return fmt.Errorf("%q is not a size like \"5GiB\" or \"500MB\"", text)
	}
	*b = ByteSize(n * mult)
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
	// Enough headroom for a build or a checkout to finish without the disk
	// filling under it, and small enough that a laptop with a modest disk is
	// not always under it: then every idle session would go at every sweep.
	DefaultDiskFloor = 5 << 30
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
		Sessions:  SessionsConfig{IdleTTL: Duration{DefaultIdleTTL}, DiskFloor: DefaultDiskFloor},
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
	c, err := ReadFile(p.ConfigFile())
	if errors.Is(err, fs.ErrNotExist) {
		return Default(), nil
	}
	return c, err
}

// ReadFile reads a config.toml at any path — a profile's, or a work machine
// spec's that `yad config apply` brings a profile in line with — and, unlike
// Load, a missing file is an error. What the file leaves out is the default.
func ReadFile(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	dec := toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return c, fmt.Errorf("%s: unknown setting:\n%s", path, strict.String())
		}
		return c, fmt.Errorf("%s: %w", path, err)
	}
	// Named, because a file that fails here was edited by hand and the owner
	// needs to know which one to open.
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
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

// ValidName is the rule for every name the owner chooses and YAD then puts in
// a path or on the wire — a connection, an account label. Exported because an
// account label becomes a directory under <data>/accounts/ and a field in
// health, so it is checked where it is typed and not only where it is loaded.
func ValidName(s string) error {
	if !nameRE.MatchString(s) {
		return fmt.Errorf("name %q: use lowercase letters, digits, dash and underscore", s)
	}
	return nil
}

// HarnessIDs is every harness the owner configured, in a stable order, so that
// what groups accounts by harness — `yad account list` and its --json — prints
// them the same way twice. The capability document does not depend on it: it
// iterates the detected harnesses and takes each one's accounts in the owner's
// own order.
func (c Config) HarnessIDs() []string {
	ids := make([]string, 0, len(c.Harness))
	for id := range c.Harness {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
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
	if c.Sessions.IdleTTL.Duration < 0 {
		errs = append(errs, fmt.Errorf("sessions.idle_ttl must not be negative, got %s — like \"336h\", or \"0s\" to keep idle sessions", c.Sessions.IdleTTL.Duration))
	}
	for _, r := range c.Workdirs.Roots {
		if !filepath.IsAbs(r) {
			errs = append(errs, fmt.Errorf("workdirs.roots: %q is not an absolute path — write it in full, like \"/home/me/src\"", r))
		} else if isFilesystemRoot(r) {
			// Resolved, not just cleaned: inRoots resolves every root before
			// it compares, so a root that is a link to "/" is "/" — the same
			// check the home default gets, for the same reason.
			errs = append(errs, fmt.Errorf("workdirs.roots: %q is the root of the filesystem, which would let a hub reach every directory on this machine — name the directories runs may use", r))
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
			if err := ValidName(a); err != nil {
				errs = append(errs, fmt.Errorf("harness.%s.accounts: %w", id, err))
			}
		}
		if dup := firstDuplicate(h.Accounts); dup != "" {
			errs = append(errs, fmt.Errorf("harness.%s.accounts lists %q twice", id, dup))
		}
	}
	var names []string
	for _, conn := range c.Connections {
		if err := ValidName(conn.Name); err != nil {
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
