package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/config"
)

// machines/guest/runner.sh is the step of `yad-machine up` that brings a built
// machine's config.toml onto its spec's and restarts the runner only when it
// must (DEV-136). It runs here as it does in the VM, against this test binary
// as yad, with systemctl a stub and `yad service install` recorded rather
// than done: what it proves is the script's decisions, not systemd's.
type kitMachine struct {
	t        *testing.T
	stage    string
	env      []string
	installs string // one line per `yad service install`
	down     string // exists while the stub systemctl says the unit is down
	failing  string // exists while `yad service install` fails
	old      string // exists while yad is one from before `yad config apply`
	state    string // where root.sh owes a restart: under the home, as root.sh finds it
	p        config.Paths
}

func newKitMachine(t *testing.T) *kitMachine {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash on this machine")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m := &kitMachine{
		t:        t,
		stage:    filepath.Join(dir, "stage"),
		installs: filepath.Join(dir, "installs"),
		down:     filepath.Join(dir, "down"),
		failing:  filepath.Join(dir, "failing"),
		old:      filepath.Join(dir, "old"),
		state:    filepath.Join(dir, ".local", "state", "yad-machine"),
		p:        config.Paths{Profile: config.DefaultProfile, Config: filepath.Join(dir, "config"), Data: filepath.Join(dir, "data")},
	}
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{bin, filepath.Join(m.stage, "guest"), filepath.Join(m.stage, "spec")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	script, err := os.ReadFile(filepath.Join("..", "..", "machines", "guest", "runner.sh"))
	if err != nil {
		t.Fatal(err)
	}
	writeExec(t, filepath.Join(m.stage, "guest", "runner.sh"), string(script))
	writeExec(t, filepath.Join(bin, "yad"), `#!/bin/sh
if [ "$1" = help ] && [ -e "$KIT_OLD" ]; then echo "usage: yad connect | service"; exit 0; fi
if [ "$1" = service ]; then
  [ -e "$KIT_FAILING" ] && { echo "yad: systemctl said no" >&2; exit 1; }
  echo "$*" >>"$KIT_INSTALLS"
  rm -f "$KIT_DOWN"
  exit 0
fi
`+beYad+`=yad exec "$KIT_SELF" "$@"
`)
	writeExec(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
case "$*" in
  *is-enabled*|*is-active*) [ -e "$KIT_DOWN" ] && exit 3; exit 0 ;;
esac
echo "stub systemctl: unexpected $*" >&2; exit 1
`)
	m.env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+dir,
		// Set, to prove runner.sh ignores it: root.sh writes the restart
		// it owes under the home, and cannot see this.
		"XDG_STATE_HOME="+filepath.Join(dir, "elsewhere"),
		"YAD_CONFIG_DIR="+m.p.Config,
		"YAD_DATA_DIR="+m.p.Data,
		"KIT_SELF="+self,
		"KIT_INSTALLS="+m.installs,
		"KIT_DOWN="+m.down,
		"KIT_FAILING="+m.failing,
		"KIT_OLD="+m.old,
	)
	return m
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (m *kitMachine) spec(src string) {
	m.t.Helper()
	if err := os.WriteFile(filepath.Join(m.stage, "spec", "config.toml"), []byte(src), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

// owe writes what root.sh writes before it replaces yad.
func (m *kitMachine) owe(lines string) {
	m.t.Helper()
	if err := os.MkdirAll(m.state, 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.state, "restart-owed"), []byte(lines), 0o644); err != nil {
		m.t.Fatal(err)
	}
}

func (m *kitMachine) set(flag string, on bool) {
	m.t.Helper()
	if !on {
		os.Remove(flag)
		return
	}
	if err := os.WriteFile(flag, nil, 0o644); err != nil {
		m.t.Fatal(err)
	}
}

// up runs the script and returns its output and how many restarts it made.
func (m *kitMachine) up() (string, int, error) {
	m.t.Helper()
	before := m.restarts()
	cmd := exec.Command("bash", filepath.Join(m.stage, "guest", "runner.sh"), m.stage)
	cmd.Env = m.env
	out, err := cmd.CombinedOutput()
	return string(out), m.restarts() - before, err
}

func (m *kitMachine) mustUp() (string, int) {
	m.t.Helper()
	out, n, err := m.up()
	if err != nil {
		m.t.Fatalf("runner.sh: %v\n%s", err, out)
	}
	return out, n
}

func (m *kitMachine) restarts() int {
	b, err := os.ReadFile(m.installs)
	if err != nil {
		return 0
	}
	return strings.Count(string(b), "\n")
}

func (m *kitMachine) config() config.Config {
	m.t.Helper()
	c, err := config.Load(m.p)
	if err != nil {
		m.t.Fatal(err)
	}
	return c
}

func TestKitUpBringsConfigOntoTheSpecAndRestartsOnlyForAChange(t *testing.T) {
	m := newKitMachine(t)
	m.spec("name = \"tl-general\"\nlabels = [\"linux\"]\ncapacity = 2\n[harness.claude]\naccounts = [\"main\"]\n")

	// The first up: no config.toml, no runner.
	m.set(m.down, true)
	out, n := m.mustUp()
	if n != 1 {
		t.Fatalf("first up restarted the runner %d times, want 1:\n%s", n, out)
	}
	if c := m.config(); c.Name != "tl-general" {
		t.Fatalf("first up left config.toml %+v", c)
	}

	// Between ups, yad writes the machine's copy: a hub connected, an
	// account logged in at the machine.
	if _, err := config.Update(t.Context(), m.p, func(c *config.Config) (bool, error) {
		c.Connections = append(c.Connections, config.Connection{Name: "zumino", URL: "https://zumino.cc/api/yad/v1"})
		h := c.Harness["claude"]
		h.Accounts = append(h.Accounts, "second")
		c.Harness["claude"] = h
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(m.p.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}

	// No spec change: nothing written, nothing restarted.
	out, n = m.mustUp()
	if n != 0 {
		t.Errorf("an up with no spec change restarted the runner:\n%s", out)
	}
	if now, _ := os.ReadFile(m.p.ConfigFile()); string(now) != string(written) {
		t.Errorf("an up with no spec change rewrote config.toml:\n%s", now)
	}
	if !strings.Contains(out, "already matches") {
		t.Errorf("an up with no spec change said:\n%s", out)
	}

	// The spec changes its labels and adds an account.
	m.spec("name = \"tl-general\"\nlabels = [\"linux\", \"tl\"]\ncapacity = 3\n[harness.claude]\naccounts = [\"main\", \"tl\"]\n")
	out, n = m.mustUp()
	if n != 1 {
		t.Errorf("an up with a spec change restarted the runner %d times, want 1:\n%s", n, out)
	}
	for _, want := range []string{`labels = ["linux", "tl"] (was ["linux"])`, "capacity = 3 (was 2)", "config.toml changed"} {
		if !strings.Contains(out, want) {
			t.Errorf("up said\n%s\nwant %q in it", out, want)
		}
	}
	c := m.config()
	if !slices.Equal(c.Harness["claude"].Accounts, []string{"main", "tl", "second"}) || len(c.Connections) != 1 || c.Capacity != 3 {
		t.Errorf("config.toml after the spec change: %+v", c)
	}

	// And the up after that is quiet again.
	if out, n = m.mustUp(); n != 0 {
		t.Errorf("a second up of the same spec restarted the runner:\n%s", out)
	}
}

// A restart the machine is owed survives an up that stopped short of it, and
// a new yad or a stopped runner is owed one with no config change at all.
func TestKitUpRestartsForANewYadAStoppedRunnerAndARestartItOwes(t *testing.T) {
	m := newKitMachine(t)
	m.spec("capacity = 2\n")
	m.mustUp()

	// What root.sh owes before it replaces yad, twice over: an up that
	// failed after the replacement, and the next one's.
	m.owe("yad was replaced\nyad was replaced\n")
	if out, n := m.mustUp(); n != 1 || !strings.Contains(out, "runner service — yad was replaced\n") {
		t.Errorf("an up that replaced yad restarted %d times:\n%s", n, out)
	}

	m.set(m.down, true)
	if out, n := m.mustUp(); n != 1 || !strings.Contains(out, "not running") {
		t.Errorf("an up with the runner stopped restarted %d times:\n%s", n, out)
	}

	m.spec("capacity = 3\n")
	m.set(m.failing, true)
	if out, _, err := m.up(); err == nil {
		t.Fatalf("an up whose service install failed succeeded:\n%s", out)
	}
	m.set(m.failing, false)
	out, n := m.mustUp()
	if n != 1 || !strings.Contains(out, "config.toml changed") {
		t.Errorf("the up after a failed restart restarted %d times:\n%s", n, out)
	}
	if out, n := m.mustUp(); n != 0 {
		t.Errorf("the owed restart was made twice:\n%s", out)
	}

	// An up that died after apply wrote config.toml and before it owed the
	// restart: the checksum it left says the file has moved since.
	m.spec("capacity = 4\n")
	if err := os.WriteFile(filepath.Join(m.state, "applying"), []byte("0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, n := m.mustUp(); n != 1 || !strings.Contains(out, "config.toml changed") {
		t.Errorf("the up after an interrupted apply restarted %d times:\n%s", n, out)
	}
	if _, err := os.Stat(filepath.Join(m.state, "applying")); err == nil {
		t.Error("an apply seen through left its checksum behind")
	}
}

// A yad from before `config apply` — an old YAD_VERSION — gets the kit's old
// behaviour: the spec seeded once, then left alone with a note, and no
// restart for a spec it did not apply.
func TestKitUpWithAYadThatCannotApplySeedsOnce(t *testing.T) {
	m := newKitMachine(t)
	m.set(m.old, true)
	m.spec("# the spec's own words\ncapacity = 2\n")
	m.set(m.down, true)
	if out, n := m.mustUp(); n != 1 {
		t.Fatalf("first up restarted %d times:\n%s", n, out)
	}
	if b, _ := os.ReadFile(m.p.ConfigFile()); !strings.HasPrefix(string(b), "# the spec's own words") {
		t.Errorf("first up did not copy the spec in:\n%s", b)
	}
	m.spec("capacity = 3\n")
	out, n := m.mustUp()
	if n != 0 || !strings.Contains(out, "has no `yad config apply`") {
		t.Errorf("an up with an old yad restarted %d times and said:\n%s", n, out)
	}
	if c := m.config(); c.Capacity != 2 {
		t.Errorf("an old yad's up changed capacity to %d", c.Capacity)
	}
}

// A spec that turns self-update on while YAD_VERSION pins a release is refused
// before anything is built (decision 0071): the runner would replace the pin
// within hours. Every way config.toml can write the setting is read; a spec
// that leaves it off, or a YAD_VERSION of latest, goes on to look for Lima —
// which is absent here, and is the next thing up needs.
func TestKitUpRefusesSelfUpdateWithAPinnedVersion(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash on this machine")
	}
	kit, err := filepath.Abs(filepath.Join("..", "..", "machines", "yad-machine"))
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	for _, tool := range []string{"awk", "bash", "dirname", "readlink"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skip(err)
		}
		if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	const refused = "turns self-update on ([update] auto = true), and YAD_VERSION pins yad at v0.3.0"
	const noLima = "limactl is not on PATH"
	for _, c := range []struct {
		name, config, version string
		on                    bool
		want                  string
	}{
		{"a section", "capacity = 2\n\n[update]\nauto = true   # follow releases\n", "v0.3.0", true, refused},
		{"a dotted key", "update.auto = true\n", "v0.3.0", true, refused},
		{"an inline table", "update = { auto = true }\n", "v0.3.0", true, refused},
		{"off", "[update]\nauto = false\n", "v0.3.0", false, noLima},
		{"commented out", "[update]\n# auto = true\n", "v0.3.0", false, noLima},
		{"not pinned", "[update]\nauto = true\n", "latest", true, noLima},
		{"no version at all", "[update]\nauto = true\n", "", true, noLima},
	} {
		t.Run(c.name, func(t *testing.T) {
			spec := t.TempDir()
			for name, body := range map[string]string{"machine.env": "MACHINE_NAME=test\n", "config.toml": c.config} {
				if err := os.WriteFile(filepath.Join(spec, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// The kit reads it with awk, yad with its TOML parser: the two
			// must agree on every spelling here.
			cfg, err := config.ReadFile(filepath.Join(spec, "config.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Update.Auto != c.on {
				t.Fatalf("yad reads update.auto as %v, want %v", cfg.Update.Auto, c.on)
			}
			cmd := exec.Command(filepath.Join(bin, "bash"), kit, "up", spec)
			cmd.Env = []string{"PATH=" + bin, "HOME=" + t.TempDir(), "YAD_VERSION=" + c.version}
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), c.want) {
				t.Errorf("up = %v, want it to stop saying %q:\n%s", err, c.want, out)
			}
		})
	}
}
