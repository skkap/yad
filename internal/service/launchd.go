package service

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"
)

// Launchd installs a runner as a launchd agent in the owner's GUI domain: it
// starts at login, runs as the owner, and is restarted when it crashes.
type Launchd struct {
	Host Host
	// settle is how long uninstall waits for launchd to let the job go; a
	// field so tests need not wait it out.
	settle time.Duration
}

// Name is the job's label. Nothing personal and nothing reverse-DNS for a
// domain yad does not own; one label per profile, so profiles coexist.
func (l *Launchd) Name(profile string) string { return "yad.runner." + profile }

func (l *Launchd) File(profile string) string {
	return filepath.Join(l.Host.Home, "Library", "LaunchAgents", l.Name(profile)+".plist")
}

func (l *Launchd) domain() string { return "gui/" + strconv.Itoa(l.Host.UID) }

func (l *Launchd) target(profile string) string { return l.domain() + "/" + l.Name(profile) }

var plistTmpl = template.Must(template.New("plist").Funcs(template.FuncMap{"x": xmlEscape}).Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by yad service install. Run it again to rewrite this file; yad service uninstall removes it. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{x .Label}}</string>
	<key>ProgramArguments</key>
	<array>
{{- range .Args}}
		<string>{{x .}}</string>
{{- end}}
	</array>
	<key>EnvironmentVariables</key>
	<dict>
{{- range .Env}}
		<key>{{x .K}}</key>
		<string>{{x .V}}</string>
{{- end}}
	</dict>
	<key>WorkingDirectory</key>
	<string>{{x .WorkingDir}}</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>{{.Throttle}}</integer>
	<key>ExitTimeOut</key>
	<integer>{{.ExitTimeout}}</integer>
	<key>ProcessType</key>
	<string>Standard</string>
	<key>StandardOutPath</key>
	<string>{{x .LogFile}}</string>
	<key>StandardErrorPath</key>
	<string>{{x .LogFile}}</string>
</dict>
</plist>
`))

type kv struct{ K, V string }

func sortedEnv(env map[string]string) []kv {
	var out []kv
	for k, v := range env {
		out = append(out, kv{k, v})
	}
	slices.SortFunc(out, func(a, b kv) int { return strings.Compare(a.K, b.K) })
	return out
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Render is the plist. KeepAlive restarts the runner only after a failure: a
// clean exit is an owner's stop, and restarting it would make the runner
// impossible to stop short of uninstalling. ThrottleInterval is launchd's only
// backoff — a fixed pause between starts. ProcessType Standard keeps launchd
// from throttling the CPU and I/O of the harnesses the runner starts, as it
// would for a Background job.
func (l *Launchd) Render(s Spec) ([]byte, error) {
	var b bytes.Buffer
	err := plistTmpl.Execute(&b, map[string]any{
		"Label": l.Name(s.Profile), "Args": s.Args(), "Env": sortedEnv(s.Env),
		"WorkingDir": s.WorkingDir, "LogFile": s.LogFile,
		"Throttle": int(restartDelay.Seconds()), "ExitTimeout": int(stopTimeout.Seconds()),
	})
	return b.Bytes(), err
}

func (l *Launchd) loaded(ctx context.Context, profile string) (bool, []byte) {
	out, err := l.Host.Run.Run(ctx, "launchctl", "print", l.target(profile))
	return err == nil, out
}

// Install replaces any loaded job with the new plist. launchd reads a plist
// only when it is bootstrapped, so rewriting the file under a loaded job
// changes nothing until it is booted out and in again.
func (l *Launchd) Install(ctx context.Context, s Spec) ([]string, error) {
	if err := RefuseRoot(l.Host); err != nil {
		return nil, err
	}
	data, err := l.Render(s)
	if err != nil {
		return nil, err
	}
	// launchd opens the log itself before starting the runner, and a missing
	// directory is a job that never starts, with nothing logged anywhere.
	if err := os.MkdirAll(filepath.Dir(s.LogFile), 0o700); err != nil {
		return nil, err
	}
	if ok, _ := l.loaded(ctx, s.Profile); ok {
		if err := l.bootout(ctx, s.Profile); err != nil {
			return nil, err
		}
	}
	file := l.File(s.Profile)
	if err := writeFile(file, data); err != nil {
		return nil, fmt.Errorf("write %s: %w", file, err)
	}
	// An owner who once ran `launchctl disable` on this label would otherwise
	// get a silent no-op from bootstrap.
	if _, err := l.Host.Run.Run(ctx, "launchctl", "enable", l.target(s.Profile)); err != nil {
		return nil, err
	}
	if _, err := l.Host.Run.Run(ctx, "launchctl", "bootstrap", l.domain(), file); err != nil {
		return nil, fmt.Errorf("%w — launchd agents need the owner's desktop session: log in to this Mac (over SSH alone there is no %s domain), then run yad service install again", err, l.domain())
	}
	return nil, nil
}

// bootout stops the job and waits until launchd has let it go, so that a
// bootstrap or a file removal straight after cannot race the old job.
func (l *Launchd) bootout(ctx context.Context, profile string) error {
	if _, err := l.Host.Run.Run(ctx, "launchctl", "bootout", l.target(profile)); err != nil {
		// The job may have gone between the check and the bootout.
		if ok, _ := l.loaded(ctx, profile); ok {
			return err
		}
		return nil
	}
	settle := l.settle
	if settle == 0 {
		settle = stopTimeout + 5*time.Second
	}
	deadline := time.Now().Add(settle)
	for {
		if ok, _ := l.loaded(ctx, profile); !ok {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("launchd still holds %s after %s — check `launchctl print %s`, then run this again", l.Name(profile), settle, l.target(profile))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (l *Launchd) Uninstall(ctx context.Context, profile string) error {
	if err := RefuseRoot(l.Host); err != nil {
		return err
	}
	if ok, _ := l.loaded(ctx, profile); ok {
		if err := l.bootout(ctx, profile); err != nil {
			return err
		}
	}
	if err := os.Remove(l.File(profile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

var (
	launchdState = regexp.MustCompile(`(?m)^\s*state = (.+)$`)
	launchdPID   = regexp.MustCompile(`(?m)^\s*pid = (\d+)$`)
	launchdExit  = regexp.MustCompile(`(?m)^\s*last exit code = (.+)$`)
)

// Status reads `launchctl print`, whose output Apple documents as not for
// parsing; only the three lines it has carried for years are read, and a
// change there shows as an unknown state, not a failure.
func (l *Launchd) Status(ctx context.Context, profile string) (Status, error) {
	if err := RefuseRoot(l.Host); err != nil {
		return Status{}, err
	}
	st := Status{Name: l.Name(profile), File: l.File(profile)}
	var err error
	if st.Installed, err = exists(st.File); err != nil {
		return st, err
	}
	ok, out := l.loaded(ctx, profile)
	if !ok {
		return st, nil
	}
	st.Loaded = true
	st.Detail = "unknown"
	if m := launchdState.FindSubmatch(out); m != nil {
		st.Detail = strings.TrimSpace(string(m[1]))
		st.Running = st.Detail == "running"
	}
	if m := launchdPID.FindSubmatch(out); m != nil {
		st.PID, _ = strconv.Atoi(string(m[1]))
	}
	if m := launchdExit.FindSubmatch(out); m != nil && !st.Running {
		st.Detail += ", last exit " + strings.TrimSpace(string(m[1]))
	}
	return st, nil
}
