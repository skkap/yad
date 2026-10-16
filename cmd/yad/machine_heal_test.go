package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A work machine whose host rebooted comes back on its own (YAD-202, decision
// 0090). The kit is run here as the host runs it, against a fixture LIMA_HOME
// and real processes — a pid that is gone, a pid the "reboot" gave to an
// unrelated process, and a Lima host agent that really is the instance's —
// with limactl, launchctl and systemctl as stubs that record what they were
// asked. ps is the host's own: what decides a pid file is the process table.
type kitHost struct {
	t                *testing.T
	kit, dir, home   string
	limaHome, states string
	limactl, launchd string
	systemd, keeper  string
	bash             string
	env              []string
}

func newKitHost(t *testing.T, hostOS string) *kitHost {
	t.Helper()
	bash := "/bin/bash" // macOS's 3.2, which the kit must run under, where there is one
	if _, err := os.Stat(bash); err != nil {
		if bash, err = exec.LookPath("bash"); err != nil {
			t.Skip("no bash on this machine")
		}
	}
	for _, tool := range []string{"ps", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(err)
		}
	}
	kit, err := filepath.Abs(filepath.Join("..", "..", "machines", "yad-machine"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	h := &kitHost{
		t: t, kit: kit, dir: dir, bash: bash,
		home:     filepath.Join(dir, "home"),
		limaHome: filepath.Join(dir, "lima"),
		states:   filepath.Join(dir, "states"),
		limactl:  filepath.Join(dir, "limactl.log"),
		launchd:  filepath.Join(dir, "launchctl.log"),
		systemd:  filepath.Join(dir, "systemctl.log"),
		keeper:   filepath.Join(dir, "home", ".local", "share", "yad-machine", "yad-machine"),
	}
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{bin, h.home, h.limaHome, h.states} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExec(t, filepath.Join(bin, "uname"), `#!/bin/sh
case "$1" in -m) echo arm64 ;; *) echo "$KIT_OS" ;; esac
`)
	// limactl list answers from one file per instance holding its status;
	// start makes it Running, unless the test says it fails.
	writeExec(t, filepath.Join(bin, "limactl"), `#!/bin/sh
echo "$*" >>"$KIT_LIMACTL"
for a; do last=$a; done
case "$1" in
list)
  for f in "$KIT_STATES"/*; do [ -f "$f" ] && echo "${f##*/} $(cat "$f")"; done
  exit 0 ;;
start)
  [ -e "$KIT_STATES/.fail" ] && { echo "limactl: it would not start" >&2; exit 1; }
  echo Running >"$KIT_STATES/$last"; exit 0 ;;
autostart)
  rm -f "$HOME/Library/LaunchAgents/io.lima-vm.autostart.$last.plist" "$XDG_CONFIG_HOME/systemd/user/lima-vm@$last.service"
  exit 0 ;;
delete) rm -f "$KIT_STATES/$last"; exit 0 ;;
esac
exit 0
`)
	writeExec(t, filepath.Join(bin, "launchctl"), `#!/bin/sh
echo "$*" >>"$KIT_LAUNCHCTL"
case "$1" in
print) [ -e "$KIT_STATES/.loaded" ] ;;
bootstrap) : >"$KIT_STATES/.loaded" ;;
bootout) rm -f "$KIT_STATES/.loaded" ;;
esac
`)
	writeExec(t, filepath.Join(bin, "systemctl"), `#!/bin/sh
echo "$*" >>"$KIT_SYSTEMCTL"
case "$2" in
is-enabled) [ -e "$KIT_STATES/.enabled" ] ;;
enable) : >"$KIT_STATES/.enabled" ;;
disable) rm -f "$KIT_STATES/.enabled" ;;
esac
`)
	h.env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"HOME="+h.home,
		"XDG_CONFIG_HOME="+filepath.Join(h.home, ".config"),
		"XDG_DATA_HOME="+filepath.Join(h.home, ".local", "share"),
		"XDG_STATE_HOME="+filepath.Join(h.home, ".local", "state"),
		"LIMA_HOME="+h.limaHome,
		"KIT_OS="+hostOS,
		"KIT_STATES="+h.states,
		"KIT_LIMACTL="+h.limactl,
		"KIT_LAUNCHCTL="+h.launchd,
		"KIT_SYSTEMCTL="+h.systemd,
	)
	for _, k := range []string{"YAD_MACHINE_HOST", "YAD_MACHINE_PRINT", "YAD_MACHINE_LOG"} {
		h.env = append(h.env, k+"=")
	}
	return h
}

// instance makes a Lima instance directory holding what a host agent leaves:
// its pid files, with the contents given, and its sockets.
func (h *kitHost) instance(name, status string, pids map[string]string) string {
	h.t.Helper()
	d := filepath.Join(h.limaHome, name)
	if err := os.MkdirAll(d, 0o700); err != nil {
		h.t.Fatal(err)
	}
	files := map[string]string{"lima.yaml": "vmType: vz\n", "disk": "", "ha.sock": "", "default_ep.sock": "", "default_fd.sock": "", "ssh.sock": ""}
	for f, pid := range pids {
		files[f] = pid + "\n"
	}
	for f, body := range files {
		if err := os.WriteFile(filepath.Join(d, f), []byte(body), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	if status != "" {
		if err := os.WriteFile(filepath.Join(h.states, name), []byte(status+"\n"), 0o644); err != nil {
			h.t.Fatal(err)
		}
	}
	return d
}

func (h *kitHost) run(stdin string, args ...string) (string, error) {
	h.t.Helper()
	return h.runScript(h.kit, stdin, args...)
}

func (h *kitHost) runScript(script, stdin string, args ...string) (string, error) {
	h.t.Helper()
	cmd := exec.Command(h.bash, append([]string{script}, args...)...)
	cmd.Env = h.env
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *kitHost) log(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

func (h *kitHost) status(name string) string {
	b, _ := os.ReadFile(filepath.Join(h.states, name))
	return strings.TrimSpace(string(b))
}

// liveProcess starts a process that stays up until the test ends, and returns its pid.
func liveProcess(t *testing.T, cmd *exec.Cmd) string {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	return strconv.Itoa(cmd.Process.Pid)
}

// exitedPid is a pid whose process has exited.
func exitedPid(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(cmd.Process.Pid)
}

// hostAgent starts a process that looks to ps like Lima's host agent for the
// instance in dir: the test binary, under the name limactl, with the
// arguments Lima 2.2 gives it.
func hostAgent(t *testing.T, dir string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "limactl")
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(link, "hostagent", "--pidfile", filepath.Join(dir, "ha.pid"), "--socket", filepath.Join(dir, "ha.sock"), filepath.Base(dir))
	cmd.Env = append(os.Environ(), beYad+"=lima")
	pid := liveProcess(t, cmd)
	// Until ps shows the new command line, the process may still be the
	// fork that has not exec'd yet.
	for range 200 {
		out, _ := exec.Command("ps", "-ww", "-o", "command=", "-p", pid).Output()
		if bytes.Contains(out, []byte("hostagent")) {
			return pid
		}
		exec.Command("sleep", "0.01").Run()
	}
	t.Fatalf("pid %s never showed as a host agent", pid)
	return ""
}

func onDisk(path string) bool { _, err := os.Lstat(path); return err == nil }

// Only a pid file that does not name the instance's own Lima process is
// removed — one whose process is gone, one the reboot gave to another
// process, one naming another instance's host agent, one holding no pid —
// and with them the host agent's sockets; then the machine is started. While
// any Lima process of the instance is alive nothing is removed and nothing is
// started. LIMA_HOME is where it looks, never ~/.lima.
func TestKitStartClearsOnlyAStalePidAndStartsTheMachine(t *testing.T) {
	h := newKitHost(t, "Darwin")
	unrelated := liveProcess(t, exec.Command("sleep", "600"))
	// The same names under ~/.lima, which LIMA_HOME says is not Lima's here:
	// nothing in it may be touched.
	decoy := filepath.Join(h.home, ".lima", "reused")
	if err := os.MkdirAll(decoy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoy, "ha.pid"), []byte(unrelated+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// tl-general's host agent is alive; "tl" is a prefix of its name, and
	// its ha.pid names tl-general's agent, which is not tl's.
	liveDir := h.instance("tl-general", "Broken", nil)
	agent := hostAgent(t, liveDir)
	h.instance("tl-general", "Broken", map[string]string{"ha.pid": agent, "vz.pid": agent})

	for _, c := range []struct {
		name    string
		pids    map[string]string
		removed bool
		says    []string
	}{
		{"dead", map[string]string{"ha.pid": exitedPid(t), "vz.pid": exitedPid(t)}, true, []string{"is not running"}},
		{"reused", map[string]string{"ha.pid": unrelated, "vz.pid": unrelated}, true, []string{"pid " + unrelated + " now belongs to another process: sleep"}},
		{"mixed", map[string]string{"ha.pid": exitedPid(t), "vz.pid": unrelated}, true, []string{"ha.pid: pid", "vz.pid: pid " + unrelated + " now belongs to another process: sleep"}},
		{"tl", map[string]string{"ha.pid": agent}, true, []string{"pid " + agent + " now belongs to another process: "}},
		{"garbage", map[string]string{"ha.pid": "not a pid"}, true, []string{"ha.pid: it holds no pid"}},
		{"tl-general", nil, false, []string{"pid " + agent + " is tl-general's own Lima process", "nothing was cleared"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := filepath.Join(h.limaHome, c.name)
			if c.pids != nil {
				d = h.instance(c.name, "Broken", c.pids)
			}
			before := h.log(h.limactl)
			out, err := h.run("", "start", c.name)
			started := strings.Contains(strings.TrimPrefix(h.log(h.limactl), before), "start --tty=false "+c.name)
			for _, s := range c.says {
				if !strings.Contains(out, s) {
					t.Errorf("start said nothing like %q:\n%s", s, out)
				}
			}
			// What it says about a process is its executable, never its
			// arguments, which are another program's.
			for _, arg := range []string{"sleep 600", "--pidfile"} {
				if strings.Contains(out, arg) {
					t.Errorf("start printed a process's arguments (%q):\n%s", arg, out)
				}
			}
			for _, f := range []string{"ha.pid", "vz.pid", "ha.sock", "default_ep.sock", "default_fd.sock"} {
				if onDisk(filepath.Join(d, f)) == c.removed {
					t.Errorf("%s exists = %v after start, want %v", f, !c.removed, !c.removed)
				}
			}
			for _, f := range []string{"ssh.sock", "disk", "lima.yaml"} {
				if !onDisk(filepath.Join(d, f)) {
					t.Errorf("start removed %s, which is not the host agent's to leave", f)
				}
			}
			if c.removed {
				if err != nil || !started || h.status(c.name) != "Running" {
					t.Errorf("start = %v, started %v, status %q; want it started and Running:\n%s", err, started, h.status(c.name), out)
				}
			} else if err == nil || started {
				t.Errorf("start = %v, started %v; want it refused, starting nothing:\n%s", err, started, out)
			}
		})
	}
	if !onDisk(filepath.Join(decoy, "ha.pid")) {
		t.Error("start cleared ~/.lima, which LIMA_HOME says is not where Lima keeps its machines")
	}
}

// status says a machine is Broken, which pid files are stale and why, and the
// command that heals it — and removes nothing itself. --json carries the same.
func TestKitStatusReportsABrokenMachineAndHowItIsHealed(t *testing.T) {
	h := newKitHost(t, "Darwin")
	unrelated := liveProcess(t, exec.Command("sleep", "600"))
	d := h.instance("tl-general", "Broken", map[string]string{"ha.pid": unrelated, "vz.pid": unrelated})
	out, err := h.run("", "status", "tl-general")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	for _, s := range []string{
		"tl-general  Broken",
		"its VM is Broken: Lima's files name processes that are not its own",
		"ha.pid: pid " + unrelated + " now belongs to another process: sleep",
		h.kit + " start tl-general` removes them and starts it",
		// Lima's own autostart is not registered here, and neither is the kit's.
		"nothing starts it at login — `LIMA_HOME=" + h.limaHome + " " + h.kit + " autostart tl-general` registers it",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("status said nothing like %q:\n%s", s, out)
		}
	}
	if !onDisk(filepath.Join(d, "ha.pid")) || !onDisk(filepath.Join(d, "ha.sock")) {
		t.Error("status removed something; it only looks")
	}
	out, err = h.run("", "status", "tl-general", "--json")
	if err != nil {
		t.Fatalf("status --json: %v\n%s", err, out)
	}
	for _, s := range []string{`"vm": "Broken"`, `"autostart": "none"`, `"ha.pid: pid ` + unrelated + ` now belongs to another process: sleep"`} {
		if !strings.Contains(out, s) {
			t.Errorf("status --json has nothing like %s:\n%s", s, out)
		}
	}
}

// The kit's own agent replaces Lima's autostart — a machine an older kit
// built changes over when the kit is run again — is loaded once, and is
// reloaded only when it changes; boot, what it runs, heals and starts, fails
// while the machine will not start so the service manager retries, and does
// nothing for a machine that is gone; destroy removes it.
func TestKitAgentReplacesLimasAutostartOnMacOS(t *testing.T) {
	h := newKitHost(t, "Darwin")
	limaPlist := filepath.Join(h.home, "Library", "LaunchAgents", "io.lima-vm.autostart.tl-general.plist")
	if err := os.MkdirAll(filepath.Dir(limaPlist), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(limaPlist, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.instance("tl-general", "Running", nil)
	out, _ := h.run("", "status", "tl-general")
	if !strings.Contains(out, "it starts at login through Lima's own autostart") {
		t.Errorf("status does not say the machine still starts through Lima's autostart:\n%s", out)
	}

	out, err := h.run("", "autostart", "tl-general")
	if err != nil {
		t.Fatalf("autostart: %v\n%s", err, out)
	}
	if !strings.Contains(h.log(h.limactl), "autostart disable tl-general") || onDisk(limaPlist) {
		t.Error("autostart left Lima's own registration in place, to race the kit's at login")
	}
	plist := filepath.Join(h.home, "Library", "LaunchAgents", "yad-machine.tl-general.plist")
	b, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"<string>yad-machine.tl-general</string>",
		"<string>/bin/bash</string>\n\t\t<string>" + h.keeper + "</string>\n\t\t<string>boot</string>\n\t\t<string>tl-general</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>ThrottleInterval</key>\n\t<integer>30</integer>",
		"<key>AbandonProcessGroup</key>\n\t<true/>",
		"<key>LIMA_HOME</key>\n\t\t<string>" + h.limaHome + "</string>",
		"<key>PATH</key>\n\t\t<string>" + filepath.Join(h.dir, "bin") + ":/usr/bin:/bin:/usr/sbin:/sbin</string>",
	} {
		if !bytes.Contains(b, []byte(s)) {
			t.Errorf("the agent's plist has nothing like %q:\n%s", s, b)
		}
	}
	if plutil, err := exec.LookPath("plutil"); err == nil {
		if out, err := exec.Command(plutil, "-lint", plist).CombinedOutput(); err != nil {
			t.Errorf("plutil -lint: %v\n%s", err, out)
		}
	}
	kit, _ := os.ReadFile(h.kit)
	if keeper, err := os.ReadFile(h.keeper); err != nil || !bytes.Equal(keeper, kit) {
		t.Errorf("the agent's copy of the kit at %s is missing or differs: %v", h.keeper, err)
	}
	uid := strconv.Itoa(os.Getuid())
	if want := "bootstrap gui/" + uid + " " + plist; !strings.Contains(h.log(h.launchd), want) {
		t.Errorf("launchctl was never asked to %q:\n%s", want, h.log(h.launchd))
	}

	// Again, unchanged: loaded already, so not reloaded — loading runs it.
	before := strings.Count(h.log(h.launchd), "bootstrap")
	if out, err := h.run("", "autostart", "tl-general"); err != nil {
		t.Fatalf("autostart again: %v\n%s", err, out)
	}
	if n := strings.Count(h.log(h.launchd), "bootstrap"); n != before {
		t.Errorf("an autostart that changed nothing loaded the agent again (%d bootstraps, was %d)", n, before)
	}
	if out, _ := h.run("", "status", "tl-general"); strings.Contains(out, "Lima's own autostart") || strings.Contains(out, "nothing starts it at login") {
		t.Errorf("status still has a note about starting at login:\n%s", out)
	}

	// What the agent runs, from its own copy, after a reboot that reused the pid.
	unrelated := liveProcess(t, exec.Command("sleep", "600"))
	h.instance("tl-general", "Broken", map[string]string{"ha.pid": unrelated, "vz.pid": unrelated})
	if err := os.WriteFile(filepath.Join(h.states, ".fail"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runScript(h.keeper, "", "boot", "tl-general"); err == nil {
		t.Errorf("boot succeeded while the machine would not start, so launchd would not retry it:\n%s", out)
	}
	os.Remove(filepath.Join(h.states, ".fail"))
	out, err = h.runScript(h.keeper, "", "boot", "tl-general")
	if err != nil || h.status("tl-general") != "Running" || !strings.Contains(out, "tl-general is running") {
		t.Errorf("boot = %v, status %q; want the machine Running:\n%s", err, h.status("tl-general"), out)
	}
	if out, err := h.runScript(h.keeper, "", "boot", "gone"); err != nil {
		t.Errorf("boot for a machine with no VM = %v, want success so it is not retried for ever:\n%s", err, out)
	}

	if out, err := h.run("tl-general\n", "destroy", "tl-general"); err != nil {
		t.Fatalf("destroy: %v\n%s", err, out)
	}
	if onDisk(plist) {
		t.Error("destroy left the kit's agent behind")
	}
	if want := "bootout gui/" + uid + "/yad-machine.tl-general"; !strings.HasSuffix(strings.TrimSpace(h.log(h.launchd)), want) {
		t.Errorf("destroy did not unload the agent; launchctl was last asked:\n%s", h.log(h.launchd))
	}
}

func TestKitAgentReplacesLimasAutostartOnLinux(t *testing.T) {
	h := newKitHost(t, "Linux")
	unitDir := filepath.Join(h.home, ".config", "systemd", "user")
	limaUnit := filepath.Join(unitDir, "lima-vm@tl-general.service")
	if err := os.MkdirAll(unitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(limaUnit, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.instance("tl-general", "Running", nil)
	if out, err := h.run("", "autostart", "tl-general"); err != nil {
		t.Fatalf("autostart: %v\n%s", err, out)
	}
	if onDisk(limaUnit) {
		t.Error("autostart left Lima's own unit in place, to race the kit's at login")
	}
	unit := filepath.Join(unitDir, "yad-machine-tl-general.service")
	b, err := os.ReadFile(unit)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		"StartLimitIntervalSec=0",
		"Type=oneshot\nRemainAfterExit=yes",
		fmt.Sprintf("ExecStart=/bin/bash %q boot tl-general", h.keeper),
		fmt.Sprintf("%q", "LIMA_HOME="+h.limaHome),
		"Restart=on-failure\nRestartSec=30\nKillMode=process",
		"StandardOutput=append:" + filepath.Join(h.home, ".local", "state", "yad-machine", "tl-general.log"),
		"WantedBy=default.target",
	} {
		if !bytes.Contains(b, []byte(s)) {
			t.Errorf("the agent's unit has nothing like %q:\n%s", s, b)
		}
	}
	if log := h.log(h.systemd); !strings.Contains(log, "--user daemon-reload") || !strings.Contains(log, "--user enable yad-machine-tl-general.service") {
		t.Errorf("systemctl was not asked to reload and enable the unit:\n%s", log)
	}
	// A unit file starts nothing unless it is enabled: status asks systemd.
	if out, _ := h.run("", "status", "tl-general", "--json"); !strings.Contains(out, `"autostart": "kit"`) {
		t.Errorf("status --json does not say the kit starts it:\n%s", out)
	}
	os.Remove(filepath.Join(h.states, ".enabled"))
	if out, _ := h.run("", "status", "tl-general"); !strings.Contains(out, "nothing starts it at login") {
		t.Errorf("status does not say a disabled unit starts nothing:\n%s", out)
	}
	if out, err := h.run("tl-general\n", "destroy", "tl-general"); err != nil {
		t.Fatalf("destroy: %v\n%s", err, out)
	}
	if onDisk(unit) || !strings.Contains(h.log(h.systemd), "--user disable yad-machine-tl-general.service") {
		t.Errorf("destroy left the kit's unit behind:\n%s", h.log(h.systemd))
	}
}

// Two starts of one machine never heal it at once — one would clear what the
// other's new host agent just wrote: a start holding the machine's lock
// refuses a second, and a lock whose holder is gone is taken over. autostart
// refuses a machine that does not exist rather than registering an agent
// for it. And every command printed runs as pasted, against the same
// LIMA_HOME: the advice status gives for a Broken machine, run through sh,
// heals it.
func TestKitStartIsLockedAndItsAdviceRunsAsPrinted(t *testing.T) {
	h := newKitHost(t, "Darwin")
	unrelated := liveProcess(t, exec.Command("sleep", "600"))
	d := h.instance("tl-general", "Broken", map[string]string{"ha.pid": unrelated})
	holder := liveProcess(t, exec.Command("sleep", "600"))
	lock := filepath.Join(d, "yad-machine.lock")
	started, err := exec.Command("sh", "-c", "LC_ALL=C TZ=UTC ps -o lstart= -p "+holder+" | tr -s ' '").Output()
	if err != nil {
		t.Fatal(err)
	}
	setLock := func(holder string) {
		t.Helper()
		os.Remove(lock)
		if err := os.Symlink(holder, lock); err != nil {
			t.Fatal(err)
		}
	}
	// Held: a live pid that started when the lock says — asked from a
	// terminal in another locale and zone than the login agent's, which
	// must still read the same time.
	setLock(holder + " " + strings.TrimRight(string(started), "\n"))
	h.env = append(h.env, "LC_ALL=ja_JP.UTF-8", "LANG=ja_JP.UTF-8", "TZ=Asia/Tokyo")
	out, err := h.run("", "start", "tl-general")
	if err == nil || !strings.Contains(out, "is being started already, by pid "+holder) || !onDisk(filepath.Join(d, "ha.pid")) || !onDisk(lock) {
		t.Errorf("start while another holds the lock = %v; want it refused, touching nothing:\n%s", err, out)
	}
	// Left by a power cut, its pid since given to another process — the
	// same live pid, started at another time — or to none: taken over.
	for _, stale := range []string{holder + " Thu Jan  1 00:00:00 1970", exitedPid(t) + " Thu Jan  1 00:00:00 1970"} {
		setLock(stale)
		h.instance("tl-general", "Broken", map[string]string{"ha.pid": unrelated})
		out, err := h.run("", "start", "tl-general")
		if err != nil || h.status("tl-general") != "Running" || onDisk(lock) {
			t.Errorf("start over a stale lock %q = %v, status %q, lock left %v; want it taken over and the machine started:\n%s", stale, err, h.status("tl-general"), onDisk(lock), out)
		}
	}
	setLock(exitedPid(t) + " Thu Jan  1 00:00:00 1970")
	h.instance("tl-general", "Broken", map[string]string{"ha.pid": unrelated})

	out, err = h.run("", "status", "tl-general")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	want := "LIMA_HOME=" + h.limaHome + " " + h.kit + " start tl-general"
	i := strings.Index(out, "`"+want+"`")
	if i < 0 {
		t.Fatalf("status gives no %q to paste:\n%s", want, out)
	}
	// Pasted into a shell that has no LIMA_HOME of its own.
	cmd := exec.Command("sh", "-c", want)
	for _, kv := range h.env {
		if !strings.HasPrefix(kv, "LIMA_HOME=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "LIMA_HOME=")
	if b, err := cmd.CombinedOutput(); err != nil || h.status("tl-general") != "Running" || onDisk(filepath.Join(d, "ha.pid")) || onDisk(lock) {
		t.Errorf("the printed command = %v, status %q; want the machine healed, Running and the lock gone:\n%s", err, h.status("tl-general"), b)
	}

	out, err = h.run("", "autostart", "tl-generl")
	if err == nil || !strings.Contains(out, "there is no machine called tl-generl, so nothing was registered") || onDisk(filepath.Join(h.home, "Library", "LaunchAgents", "yad-machine.tl-generl.plist")) {
		t.Errorf("autostart for a machine that does not exist = %v; want it refused, registering nothing:\n%s", err, out)
	}
}
