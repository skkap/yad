package service

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

// Systemd installs a runner as a systemd --user unit: it runs in the owner's
// user manager, as the owner, and is restarted when it crashes.
type Systemd struct {
	Host Host
}

// Name is the unit's name; one unit per profile, so profiles coexist.
func (s *Systemd) Name(profile string) string { return "yad-runner-" + profile + ".service" }

// File is under $XDG_CONFIG_HOME/systemd/user, the user manager's own
// directory, so installing needs no privilege.
func (s *Systemd) File(profile string) string {
	base := s.Host.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		base = filepath.Join(s.Host.Home, ".config")
	}
	return filepath.Join(base, "systemd", "user", s.Name(profile))
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
		"StopSec":    strconv.Itoa(int(stopTimeout.Seconds())) + "s",
	})
	return b.Bytes(), err
}

func (s *Systemd) systemctl(ctx context.Context, args ...string) ([]byte, error) {
	out, err := s.Host.Run.Run(ctx, "systemctl", append([]string{"--user"}, args...)...)
	if err != nil && strings.Contains(err.Error(), "bus") {
		return out, fmt.Errorf("%w — systemctl --user needs the owner's user manager: run this in a login session of that user (not su or sudo), or export XDG_RUNTIME_DIR=/run/user/%d", err, s.Host.UID)
	}
	return out, err
}

// Install writes the unit, reloads the user manager and restarts the unit:
// restart starts a stopped unit and replaces a running one, so running
// install again is how a moved binary or a new PATH takes effect.
func (s *Systemd) Install(ctx context.Context, sp Spec) ([]string, error) {
	if err := RefuseRoot(s.Host); err != nil {
		return nil, err
	}
	data, err := s.Render(sp)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(sp.LogFile), 0o700); err != nil {
		return nil, err
	}
	file := s.File(sp.Profile)
	if err := writeFile(file, data); err != nil {
		return nil, fmt.Errorf("write %s: %w", file, err)
	}
	name := s.Name(sp.Profile)
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
		fmt.Sprintf("to keep it running with nobody logged in — and after a reboot — run: loginctl enable-linger %s", user),
	}
}

func (s *Systemd) show(ctx context.Context, name string) (map[string]string, error) {
	out, err := s.systemctl(ctx, "show", name, "--property=LoadState,ActiveState,SubState,MainPID,Result")
	if err != nil {
		return nil, err
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

func (s *Systemd) Status(ctx context.Context, profile string) (Status, error) {
	if err := RefuseRoot(s.Host); err != nil {
		return Status{}, err
	}
	st := Status{Name: s.Name(profile), File: s.File(profile)}
	var err error
	if st.Installed, err = exists(st.File); err != nil {
		return st, err
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
