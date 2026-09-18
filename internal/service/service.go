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
)

// stopTimeout is how long the service manager waits between SIGTERM and
// SIGKILL. A runner holds its runs in flight across a stop and settles them at
// the next start, so a stop needs only time to flush the event spool, not to
// finish a run; drain (DEV-13) is the command that waits for runs.
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
}

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
}

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
		if v := h.Getenv(k); v != "" {
			env[k] = v
		}
	}
	return Spec{
		Profile:    p.Profile,
		Executable: executable,
		Env:        env,
		WorkingDir: h.Home,
		LogFile:    filepath.Join(p.Data, "logs", "service.log"),
	}, nil
}

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
