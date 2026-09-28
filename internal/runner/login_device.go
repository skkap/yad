package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter/codex"
)

// byDevice drives Codex's device-code login over its app-server (decision
// 0057): a link and a code the owner types there, reported as a link login's
// url and user_code, and no code back through the hub. The app-server answers
// in structure, so nothing Codex prints or words is read; the completion it
// sends is a condition of success, and the harness's own check is the answer.
func (m *Logins) byDevice(ctx context.Context, l *hubLogin, bin string) {
	log := m.Log.With("connection", l.conn, "login", l.id, "harness", l.ref.Harness, "account", l.ref.Label)
	h := l.ref.Harness
	home, release, ok := m.claimed(l, log)
	if !ok {
		return
	}
	// Deferred first, so it runs last: the app-server is stopped before the
	// home can go.
	defer release()
	dir := home
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	// Apart from ctx, which kills the process group the moment it ends: a
	// login ended from the hub first cancels Codex's own, so a code typed
	// after it logs nothing in. Close stops it on every way out.
	d, err := codex.StartDeviceLogin(context.WithoutCancel(ctx), bin, dir, append(account.LoginEnv(h, home), noBrowser))
	if err != nil {
		log.Error("a hub login could not start codex's app-server", "err", err)
		m.end(l, v1.LoginFailed, fmt.Sprintf("%s's app-server could not be started for the login — `%s` at the machine says why", h, m.Paths.RemoteCommand("daemon", "logs")))
		return
	}
	defer d.Close(m.ExitWait)

	begin, cancel := context.WithTimeout(ctx, m.URLWait)
	dc, err := d.Begin(begin)
	cancel()
	switch {
	case ctx.Err() != nil:
		return
	case err != nil:
		// Codex's own words stay here: an error the app-server answered with
		// is its text, and goes to no hub.
		if _, refused := errors.AsType[*codex.RPCError](err); refused {
			m.end(l, v1.LoginFailed, fmt.Sprintf("%s's app-server would not start a device-code login: this %s may not have one — `%s` at the machine warns when its protocol is not one yad knows — or its ChatGPT workspace has device-code login turned off, which the workspace's admin can change; %s",
				h, h, m.Paths.RemoteCommand("doctor"), m.atTheMachine(l)))
			return
		}
		m.end(l, v1.LoginFailed, fmt.Sprintf("%s's app-server gave no device code yad could show within %s — `%s` at the machine shows whether this %s is one yad knows; %s",
			h, m.URLWait, m.Paths.RemoteCommand("doctor"), h, m.atTheMachine(l)))
		return
	}
	m.showCode(l, dc)

	codeWait := time.NewTimer(m.CodeWait)
	defer codeWait.Stop()
	select {
	case <-d.Completed():
	case <-d.Exited():
		// A completion read before the output ended is still the answer.
		select {
		case <-d.Completed():
		default:
			m.end(l, v1.LoginFailed, fmt.Sprintf("%s's app-server stopped while the code waited to be entered — start a new login", h))
			return
		}
	case <-codeWait.C:
		m.end(l, v1.LoginExpired, fmt.Sprintf("the code was not entered within %s of the link — start a new login", m.CodeWait))
		d.Cancel()
		return
	case <-ctx.Done():
		d.Cancel()
		return
	}
	if !d.Succeeded() {
		m.end(l, v1.LoginFailed, fmt.Sprintf("the code did not log %s in: it was refused, or ran out at OpenAI — an account in a ChatGPT Business, Enterprise or Edu workspace needs its admin to turn device-code login on first; start a new login, or %s",
			h, m.atTheMachine(l)))
		return
	}
	m.set(l, v1.LoginChecking, "")
	// Asked only once the app-server has exited, which it does on its input
	// closing, after writing what the login left to write: its completion
	// is not a promise that auth.json is already on disk.
	d.Close(m.ExitWait)
	m.check(ctx, l, bin, home, log)
}

// showCode moves a device-code login to waiting, with what the owner is shown.
func (m *Logins) showCode(l *hubLogin, dc codex.DeviceCode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l.state.IsTerminal() {
		return
	}
	m.setLocked(l, v1.LoginWaiting, dc.URL)
	l.userCode = dc.UserCode
}
