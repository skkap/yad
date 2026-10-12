// Package service installs a profile's runner as a per-user service — a
// launchd agent on macOS, a systemd --user unit on Linux — so an owner sets a
// runner up once and it survives logouts, crashes and reboots.
//
// Every launchctl and systemctl call goes through a Runner, so tests drive the
// whole install and uninstall sequence without touching the machine's service
// manager.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/upgrade"
)

// stopTimeout is the least the service manager waits between SIGTERM and
// SIGKILL. SIGTERM drains the runner (decision 0029), which can take the owner's
// drain wait and then the cancel ladder, so a unit's own timeout is the Spec's
// StopTimeout, derived from config.toml; this floor covers a runner with no
// runs, which needs only time to flush what it owes.
const stopTimeout = 30 * time.Second

// restartDelay is the pause before a crashed runner is started again. A runner
// that dies at once — a broken config.toml, a store it cannot open — would
// otherwise restart as fast as the service manager allows and fill its log.
const restartDelay = 10 * time.Second

// Runner runs one service-manager command and returns its standard output. An
// error carries the command's standard error, so it can be shown as is.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Spec is everything a unit says about one profile's runner.
type Spec struct {
	Profile    string
	Executable string            // absolute path to yad, as the owner installed it
	Env        map[string]string // PATH and the directory overrides, captured at install
	WorkingDir string            // the owner's home: a service's default is /, which no run wants
	LogFile    string            // standard output and error, until the daemon's own log takes over
	// StopTimeout is how long a stop may take before the service manager
	// kills the runner: its drain wait plus the cancel ladder and a last
	// flush (runner.StopBudget). A changed drain wait reaches the unit only
	// when `yad service install` runs again.
	StopTimeout time.Duration
}

// stopAfter is the unit's timeout between SIGTERM and SIGKILL, never below
// the floor.
func (s Spec) stopAfter() time.Duration { return max(s.StopTimeout, stopTimeout) }

// Args is the command line the service runs.
func (s Spec) Args() []string {
	return []string{s.Executable, "--profile", s.Profile, "daemon", "start", "--foreground"}
}

// Status is what the service manager knows about one profile's unit.
type Status struct {
	Name      string // launchd label or systemd unit name
	File      string // plist or unit file
	Installed bool   // the file exists
	Loaded    bool   // the service manager knows the job
	Running   bool
	PID       int
	Detail    string // the manager's own words for the state, for the owner
	// LogFile is the log the installed unit names, read back from its file.
	// It is the one thing in a unit that says which profile directories it
	// runs under: a label is only a profile's name, and a unit carrying
	// another YAD_DATA_DIR is a different runner with the same one. Empty
	// when there is no file to read.
	LogFile string
}

// Manager installs, removes and inspects one platform's units.
type Manager interface {
	// Name is the unit's name for a profile: a launchd label or a systemd unit.
	Name(profile string) string
	// File is where that unit's definition lives.
	File(profile string) string
	// Render is the unit's definition.
	Render(Spec) ([]byte, error)
	// Install writes the unit and (re)starts it; running it again replaces the
	// unit, which is how an owner picks up a moved binary or a new PATH.
	Install(ctx context.Context, s Spec) (notes []string, err error)
	// Uninstall stops the runner and removes the unit; nothing there is not an
	// error.
	Uninstall(ctx context.Context, profile string) error
	Status(ctx context.Context, profile string) (Status, error)
	// Start starts the profile's loaded unit now, and leaves one already
	// running as it is. A daemon the manager starts is one it watches and
	// restarts after a crash; one `yad daemon start` spawns is not (YAD-173).
	Start(ctx context.Context, profile string) error
	// StartCommand and RestartCommand are the manager's own commands for the
	// unit, as argv, for an error to offer when yad cannot run them itself.
	StartCommand(profile string) []string
	RestartCommand(profile string) []string
	// StatusCommand asks the manager about the unit in its own words, and
	// UnloadCommand stops the job and unloads it; both for an install or
	// uninstall that refused because the manager could not be asked.
	StatusCommand(profile string) []string
	UnloadCommand(profile string) []string
}

// UnansweredError is a service manager that could not say whether it holds
// a profile's job: no GUI domain over SSH, no user bus. It is never read as
// "not loaded" — an uninstall would remove the unit file from under a job
// still loaded, an install would load a second one beside it, and a start
// would put a second runner beside the first (decisions 0074, YAD-176).
type UnansweredError struct {
	Manager string // "launchd", "the systemd user manager"
	Name    string // the unit
	Err     error
}

func (e *UnansweredError) Error() string {
	return fmt.Sprintf("%s could not say whether it holds %s: %v", e.Manager, e.Name, e.Err)
}

func (e *UnansweredError) Unwrap() error { return e.Err }

// Host is the machine a Manager acts on — the seams tests replace.
type Host struct {
	Home   string
	UID    int
	EUID   int
	User   string
	Getenv func(string) string
	Run    Runner
}

// LocalHost is this machine, as the invoking user.
func LocalHost() (Host, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Host{}, fmt.Errorf("no home directory to install a per-user service into: %w", err)
	}
	return Host{
		Home: home, UID: os.Getuid(), EUID: os.Geteuid(), User: os.Getenv("USER"),
		Getenv: os.Getenv, Run: ExecRunner{},
	}, nil
}

// ForOS is the Manager for a GOOS — runtime.GOOS, on this machine.
func ForOS(goos string, h Host) (Manager, error) {
	switch goos {
	case "darwin":
		return &Launchd{Host: h}, nil
	case "linux":
		return &Systemd{Host: h}, nil
	default:
		return nil, fmt.Errorf("yad service supports macOS (launchd) and Linux (systemd), not %s — run `yad daemon start --foreground` under this system's own supervisor", goos)
	}
}

// RefuseRoot stops a service being installed for root. A root runner would run
// every harness as root with permissions bypassed (ARCHITECTURE.md §8), and
// Claude refuses that mode as root anyway; under sudo the unit would also land
// in root's home, not the owner's.
func RefuseRoot(h Host) error {
	if h.EUID == 0 {
		return errors.New("yad service must not run as root: a runner runs its harnesses as the user that owns it — run it again as that user, without sudo")
	}
	return nil
}

// envCarried are the variables, beyond PATH, that decide which directories a
// profile resolves to (config.Resolve). A unit starts with none of the
// installing shell's environment, so a profile kept on another volume would
// otherwise start a second, empty runner.
var envCarried = []string{"YAD_CONFIG_DIR", "YAD_DATA_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME"}

// NewSpec builds the Spec for a profile: the executable, the captured PATH and
// the directory overrides.
func NewSpec(p config.Paths, executable, path string, h Host) (Spec, error) {
	if !filepath.IsAbs(executable) {
		return Spec{}, fmt.Errorf("yad's own path %q is not absolute — run yad by its full path", executable)
	}
	// `go run` builds into a temporary directory that is deleted on exit, so a
	// unit pointing there starts a binary that no longer exists.
	if strings.HasPrefix(executable, filepath.Clean(os.TempDir())+string(filepath.Separator)) || strings.Contains(executable, "/go-build") {
		return Spec{}, fmt.Errorf("yad is running from a temporary build (%s) — install it first (`make install`) and run that binary", executable)
	}
	env := map[string]string{"PATH": path}
	for _, k := range envCarried {
		v := h.Getenv(k)
		if v == "" {
			continue
		}
		// The service starts in the owner's home, not where install ran, so a
		// relative override would name another, empty profile there.
		if !filepath.IsAbs(v) {
			return Spec{}, fmt.Errorf("%s=%s is relative — set it to an absolute path and run this again", k, v)
		}
		env[k] = v
	}
	// The fork an install came from. A runner with self-update on fetches
	// its releases from it (decision 0071); a unit without it would put
	// upstream's binary over a fork's at the next check.
	if repo := h.Getenv(upgrade.RepoEnv); repo != "" {
		env[upgrade.RepoEnv] = repo
	}
	return Spec{
		Profile:    p.Profile,
		Executable: executable,
		Env:        env,
		WorkingDir: h.Home,
		LogFile:    LogFile(p),
	}, nil
}

// LogFile is where a profile's unit sends the runner's standard output and
// error. It is under the profile's data directory, which is what tells the
// profile's unit apart from one of the same name for other directories.
func LogFile(p config.Paths) string { return filepath.Join(p.Data, "logs", "service.log") }

// writeFile replaces a unit file atomically: a service manager that reads a
// half-written plist or unit loads nothing, or worse, loads the fragment.
func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// The unit holds no secret — PATH and directories — and both launchd and
	// systemd expect a unit readable like any other config file.
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}
