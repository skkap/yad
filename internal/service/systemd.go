package service

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/skkap/yad/internal/shellword"
)

// Systemd installs a runner as a systemd --user unit: it runs in the owner's
// user manager, as the owner, and is restarted when it crashes.
type Systemd struct {
	Host Host
	// unitDir is the user manager's own directory for the owner's units, once
	// asked.
	unitDir string
}

// Name is the unit's name; one unit per profile, so profiles coexist.
func (s *Systemd) Name(profile string) string { return "yad-runner-" + profile + ".service" }

// File is in the owner's systemd/user directory, so installing needs no
// privilege. Install, Uninstall and Status ask the manager where that is
// first; before they have, it is the default under the owner's home.
func (s *Systemd) File(profile string) string {
	dir := s.unitDir
	if dir == "" {
		dir = filepath.Join(s.Host.Home, ".config", "systemd", "user")
	}
	return filepath.Join(dir, s.Name(profile))
}

// resolve asks the running user manager for its unit search path and takes
// the first systemd/user directory in the owner's home — the config directory,
// which the manager searches before the data directory. The path is fixed from
// the manager's own start-up environment; the installing shell's
// XDG_CONFIG_HOME, and even what the manager hands its services
// (environment.d, set-environment), can name a directory it never searches.
//
// A manager that cannot be asked — no user bus — is an UnansweredError for
// the profile's unit: install and uninstall stop here, before the unit is
// touched, and say so as start does.
func (s *Systemd) resolve(ctx context.Context, profile string) error {
	out, err := s.systemctl(ctx, "show", "--property=UnitPath", "--value")
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &UnansweredError{Manager: "the systemd user manager", Name: s.Name(profile), Err: err}
	}
	home := filepath.Clean(s.Host.Home) + string(filepath.Separator)
	for _, dir := range strings.Fields(string(out)) {
		if filepath.IsAbs(dir) && strings.HasPrefix(dir, home) && filepath.Base(dir) == "user" && filepath.Base(filepath.Dir(dir)) == "systemd" {
			s.unitDir = dir
			return nil
		}
	}
	return fmt.Errorf("the systemd user manager searches no directory in %s for units (UnitPath: %s) — if SYSTEMD_UNIT_PATH is set for it, add ~/.config/systemd/user to it and run this again", s.Host.Home, strings.TrimSpace(string(out)))
}

var unitTmpl = template.Must(template.New("unit").Funcs(template.FuncMap{"exec": quoteExec, "env": quoteEnv, "path": escapeSpecifiers}).Parse(`# Written by yad service install. Run it again to rewrite this file; yad service uninstall removes it.
[Unit]
Description=yad runner (profile {{.Profile}})
Documentation=https://github.com/skkap/yad
# Never give up restarting: RestartSec already spaces the attempts out.
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStart={{range $i, $a := .Args}}{{if $i}} {{end}}{{exec $a}}{{end}}
WorkingDirectory={{path .WorkingDir}}
{{- range .Env}}
Environment={{env (printf "%s=%s" .K .V)}}
{{- end}}
Restart=on-failure
RestartSec={{.RestartSec}}
RestartSteps=5
RestartMaxDelaySec={{.RestartMax}}
KillMode=mixed
TimeoutStopSec={{.StopSec}}
StandardOutput=append:{{path .LogFile}}
StandardError=append:{{path .LogFile}}

[Install]
WantedBy=default.target
`))

// quoteExec quotes one ExecStart word: double quotes with C-style escapes,
// and specifiers (%) and variable references ($) doubled, so a path with a
// space, a percent sign or a dollar reaches the runner exactly as written.
func quoteExec(s string) string { return quote(s, true) }

// quoteEnv quotes one Environment= assignment. systemd expands specifiers
// there but not variables, so a $ stays as it is.
func quoteEnv(s string) string { return quote(s, false) }

func quote(s string, dollars bool) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '%':
			b.WriteString("%%")
		case r == '$' && dollars:
			b.WriteString("$$")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// escapeSpecifiers is a path for a directive that takes the rest of its line
// verbatim — WorkingDirectory=, append: — where only specifiers are expanded.
func escapeSpecifiers(s string) string { return strings.ReplaceAll(s, "%", "%%") }

// Render is the unit. Restart=on-failure leaves a clean exit — an owner's
// stop — stopped. The restart delay grows from RestartSec to
// RestartMaxDelaySec over RestartSteps (systemd 254 and later; older versions
// warn and keep the fixed delay). KillMode=mixed sends SIGTERM to the runner
// alone, so the runner stops its harnesses down its own cancel ladder rather
// than systemd signalling every child at once; SIGKILL still reaches all of
// them at the timeout.
func (s *Systemd) Render(sp Spec) ([]byte, error) {
	// A line break ends a directive, and a path that is the rest of a line
	// loses trailing spaces; neither can be quoted in WorkingDirectory= or append:.
	for _, p := range []string{sp.LogFile, sp.WorkingDir} {
		if strings.ContainsAny(p, "\n\r") || strings.TrimSpace(p) != p {
			return nil, fmt.Errorf("%q cannot be named in a systemd unit — move it (YAD_DATA_DIR, or HOME) to a path without line breaks or edge spaces", p)
		}
	}
	for k, v := range sp.Env {
		if strings.ContainsAny(k+v, "\n\r") {
			return nil, fmt.Errorf("%s holds a line break, which a systemd unit cannot carry — fix it in your shell profile and run this again", k)
		}
	}
	var b bytes.Buffer
	err := unitTmpl.Execute(&b, map[string]any{
		"Profile": sp.Profile, "Args": sp.Args(), "Env": sortedEnv(sp.Env),
		"WorkingDir": sp.WorkingDir, "LogFile": sp.LogFile,
		"RestartSec": strconv.Itoa(int(restartDelay.Seconds())) + "s",
		"RestartMax": "5min",
		"StopSec":    strconv.Itoa(int(sp.stopAfter().Seconds())) + "s",
	})
	return b.Bytes(), err
}

func (s *Systemd) systemctl(ctx context.Context, args ...string) ([]byte, error) {
	out, err := s.Host.Run.Run(ctx, "systemctl", append([]string{"--user"}, args...)...)
	if err != nil && strings.Contains(err.Error(), "Failed to connect to") && strings.Contains(err.Error(), "bus") {
		return out, fmt.Errorf("%w — systemctl --user needs the owner's user manager: run this in a login session of that user (not su or sudo), or export XDG_RUNTIME_DIR=/run/user/%d", err, s.Host.UID)
	}
	return out, err
}

// Install stops a runner already installed, writes the unit, reloads the user
// manager and restarts the unit, so running install again is how a moved
// binary, a new PATH or a changed drain wait takes effect.
func (s *Systemd) Install(ctx context.Context, sp Spec) ([]string, error) {
	if err := RefuseRoot(s.Host); err != nil {
		return nil, err
	}
	data, err := s.Render(sp)
	if err != nil {
		return nil, err
	}
	if err := s.resolve(ctx, sp.Profile); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(sp.LogFile), 0o700); err != nil {
		return nil, err
	}
	file := s.File(sp.Profile)
	name := s.Name(sp.Profile)
	// A running runner is stopped under the unit it was started with: the
	// new unit's stop timeout may be shorter than the drain wait that runner
	// loaded, and restarting after daemon-reload would kill it mid-drain.
	// Asked of systemd, not of the file: a unit stays loaded, and running,
	// after its file is removed. A unit systemd never loaded is not an error.
	if _, err := s.systemctl(ctx, "stop", name); err != nil && !strings.Contains(err.Error(), "not loaded") {
		return nil, err
	}
	if err := writeFile(file, data); err != nil {
		return nil, fmt.Errorf("write %s: %w", file, err)
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", name}, {"restart", name}} {
		if _, err := s.systemctl(ctx, args...); err != nil {
			return nil, err
		}
	}
	return s.lingerNotes(ctx), nil
}

// lingerNotes says when the runner will stop at logout. Enabling lingering is
// the owner's decision — it keeps their user manager, and every other user
// unit, running with nobody logged in — so install says so and never does it.
func (s *Systemd) lingerNotes(ctx context.Context) []string {
	user := s.Host.User
	if user == "" {
		user = strconv.Itoa(s.Host.UID)
	}
	out, err := s.Host.Run.Run(ctx, "loginctl", "show-user", user, "--property=Linger", "--value")
	if err == nil && strings.TrimSpace(string(out)) == "yes" {
		return nil
	}
	return []string{
		fmt.Sprintf("lingering is off for %s, so this runner stops when you log out and starts only at your next login", user),
		fmt.Sprintf("to keep it running with nobody logged in — and after a reboot — run: `%s`", shellword.Command("loginctl", "enable-linger", user)),
	}
}

func (s *Systemd) show(ctx context.Context, name string) (map[string]string, error) {
	out, err := s.systemctl(ctx, "show", name, "--property=LoadState,ActiveState,SubState,MainPID,Result")
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &UnansweredError{Manager: "the systemd user manager", Name: name, Err: err}
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	return props, nil
}

// Uninstall stops and disables the unit if the user manager knows it, then
// removes the file and reloads, so nothing of the unit is left behind.
func (s *Systemd) Uninstall(ctx context.Context, profile string) error {
	if err := RefuseRoot(s.Host); err != nil {
		return err
	}
	if err := s.resolve(ctx, profile); err != nil {
		return err
	}
	name, file := s.Name(profile), s.File(profile)
	props, err := s.show(ctx, name)
	if err != nil {
		return err
	}
	if props["LoadState"] != "not-found" {
		if _, err := s.systemctl(ctx, "disable", "--now", name); err != nil {
			return err
		}
	}
	had, err := exists(file)
	if err != nil {
		return err
	}
	if had {
		if err := os.Remove(file); err != nil {
			return err
		}
	}
	if had || props["LoadState"] != "not-found" {
		if _, err := s.systemctl(ctx, "daemon-reload"); err != nil {
			return err
		}
		// A unit that failed is remembered until reset, even after its file
		// is gone; best effort, since one that never failed has nothing to reset.
		_, _ = s.systemctl(ctx, "reset-failed", name)
	}
	return nil
}

// Start starts the unit; systemctl leaves one already active as it is.
func (s *Systemd) Start(ctx context.Context, profile string) error {
	if err := RefuseRoot(s.Host); err != nil {
		return err
	}
	if _, err := s.systemctl(ctx, "start", s.Name(profile)); err != nil {
		return fmt.Errorf("`%s`: %w", shellword.Command(s.StartCommand(profile)...), err)
	}
	return nil
}

func (s *Systemd) StartCommand(profile string) []string {
	return []string{"systemctl", "--user", "start", s.Name(profile)}
}

// RestartCommand stops the unit — SIGTERM, a drain to the runner (decision
// 0029), for up to TimeoutStopSec — and starts it again.
func (s *Systemd) RestartCommand(profile string) []string {
	return []string{"systemctl", "--user", "restart", s.Name(profile)}
}

func (s *Systemd) StatusCommand(profile string) []string {
	return []string{"systemctl", "--user", "status", s.Name(profile)}
}

// UnloadCommand is disable --now: the unit is stopped — a drain — and no
// longer started at login, leaving its file for an uninstall to remove.
func (s *Systemd) UnloadCommand(profile string) []string {
	return []string{"systemctl", "--user", "disable", "--now", s.Name(profile)}
}

// unitLog is the log the unit appends to, with Render's specifier escaping
// still in it.
var unitLog = regexp.MustCompile(`(?m)^StandardOutput=append:(.*)$`)

func unescapeSpecifiers(s string) string { return strings.ReplaceAll(s, "%%", "%") }

// unitDirs are the directories under the owner's home a user manager may
// search, for when it cannot be asked which one it does: the config and the
// data directory, as XDG names them and as they default.
func (s *Systemd) unitDirs() []string {
	var dirs []string
	for _, base := range []string{s.Host.Getenv("XDG_CONFIG_HOME"), filepath.Join(s.Host.Home, ".config"), s.Host.Getenv("XDG_DATA_HOME"), filepath.Join(s.Host.Home, ".local", "share")} {
		if filepath.IsAbs(base) {
			dirs = append(dirs, filepath.Join(base, "systemd", "user"))
		}
	}
	return dirs
}

// Status asks the user manager about the unit. One that cannot be asked —
// no user bus — fails, and the Status it returns still says whether a unit
// file is there and what log it names, found in the directories a manager
// may search: that is what a start goes on to decide whether the profile may
// be a service (decision 0074).
func (s *Systemd) Status(ctx context.Context, profile string) (Status, error) {
	if err := RefuseRoot(s.Host); err != nil {
		return Status{}, err
	}
	if err := s.resolve(ctx, profile); err != nil {
		// Every directory is looked in: which one the manager reads is what
		// it cannot be asked, so a file earlier in the list may be a stale
		// one it never loads. Files that disagree on their log say nothing
		// about which is the runner's, and are reported with no log.
		st := Status{Name: s.Name(profile), File: s.File(profile)}
		for _, dir := range s.unitDirs() {
			file := filepath.Join(dir, s.Name(profile))
			ok, serr := exists(file)
			if serr != nil {
				// Unreadable is not absent: File names it, Installed is
				// false, and the caller's own look at it fails the same way.
				return Status{Name: st.Name, File: file}, err
			}
			if !ok {
				continue
			}
			log := installedLog(file, unitLog, unescapeSpecifiers)
			switch {
			case !st.Installed:
				st.File, st.Installed, st.LogFile = file, true, log
			case log != st.LogFile:
				st.LogFile = ""
			}
		}
		return st, err
	}
	st := Status{Name: s.Name(profile), File: s.File(profile)}
	var err error
	if st.Installed, err = exists(st.File); err != nil {
		return st, err
	}
	if st.Installed {
		st.LogFile = installedLog(st.File, unitLog, unescapeSpecifiers)
	}
	props, err := s.show(ctx, st.Name)
	if err != nil {
		return st, err
	}
	if props["LoadState"] == "not-found" || props["LoadState"] == "" {
		return st, nil
	}
	st.Loaded = true
	st.Running = props["ActiveState"] == "active" && props["SubState"] == "running"
	st.PID, _ = strconv.Atoi(props["MainPID"])
	st.Detail = props["ActiveState"] + " (" + props["SubState"] + ")"
	if r := props["Result"]; r != "" && r != "success" {
		st.Detail += ", result " + r
	}
	return st, nil
}
