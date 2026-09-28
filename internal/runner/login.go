package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/supervise"
)

// Logins is the daemon's hub logins (decision 0055): an account's login a hub
// started, by link or by token, and where each one is. The sync loops hand it
// the hub's login controls and report what it holds, per connection, until a
// sync carrying a login's end is answered.
//
// It is memory and nothing else. A login in flight is a harness process
// waiting on a code, and no restart can give that process back, so a restart
// forgets every login — the hub hears of it by the login's absence, and the
// owner starts another. What a login leaves behind is the account's own
// state: the credential the harness wrote in the home, and the account's row,
// which the daemon writes as it does for `yad account add` (decision 0043).
//
// The code and the token pass through here and go nowhere else: not to a log,
// an event, state.db, a report or an error.
type Logins struct {
	// Data is where the account homes live.
	Data string
	// Paths names the profile in the next actions a login's error carries.
	Paths config.Paths
	// Accounts is the owner's lists: a hub logs in only an account named
	// there, or a harness's own default login.
	Accounts *Accounts
	// Binary resolves a harness to its executable; nil is harness.Locate.
	Binary func(harness string) (string, bool)
	// Changed is told when a login took, so the capability document is built
	// again now rather than at the next probe. Nil tells nobody.
	Changed func()
	Log     *slog.Logger
	// The deadlines of a link login; zero is the shipped value of each.
	URLWait, CodeWait, ExitWait time.Duration

	once  sync.Once
	mu    sync.Mutex
	byKey map[loginKey]*hubLogin
	// live is the last login started for each account, the default login
	// keyed with an empty label, until its goroutine has let go of the home —
	// not only until it ends. A login cancelled or replaced is still in the
	// home while its process is stopped or its check runs, and the next
	// login for the account waits on it however the controls that ended it
	// and started the next arrived.
	live map[account.Ref]*hubLogin
	base context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup
}

// The deadlines of a link login.
const (
	// loginURLWait is how long the harness's login has to print its link.
	// Claude prints it at once; a login that has not in this time is one
	// whose output changed shape, and waiting longer would only hide it.
	loginURLWait = 30 * time.Second
	// loginCodeWait is how long the owner has to sign in and paste the code
	// back, decision 0055's ten minutes: longer than a person takes, short
	// enough that a login walked away from does not hold the account's home.
	loginCodeWait = 10 * time.Minute
	// loginExitWait is how long the login has, once it has the code, to
	// exchange it and exit. One round trip to the provider; a login still
	// there after a minute is asking again, and its answer is read from the
	// home either way.
	loginExitWait = time.Minute
)

// maxLogins bounds what one runner holds for its hubs. There is at most one
// login in flight per account; the rest are ends waiting for a sync to carry
// them, and a hub that starts more than this is heard again once those are
// answered — it repeats start_login until the login is reported.
const maxLogins = 64

// loginOutputCap bounds what is kept of the login's output while looking for
// its link. Claude prints three lines; the rest is read and dropped, so a
// login that prints without end neither fills memory nor blocks on its pipe.
const loginOutputCap = 64 << 10

// maxLoginURL bounds the link reported to a hub. Claude's is about 500 bytes.
const maxLoginURL = 4096

// maxLoginCode bounds a code written to the login. Claude's is under 200.
const maxLoginCode = 4096

// loginIDPattern is the shape a hub's login id must have: it keys the login
// and is echoed in every report.
var loginIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// linkPattern finds https URLs in the login's output. It stops at whitespace,
// quotes and control characters, so a link a terminal hyperlink escape (OSC 8)
// wraps is found inside it.
var linkPattern = regexp.MustCompile(`https://[^\s"'<>\x00-\x1f\x7f]+`)

type loginKey struct{ conn, id string }

// hubLogin is one login, as the runner reports it.
type hubLogin struct {
	conn, id string
	ref      account.Ref
	method   v1.LoginMethod
	state    v1.LoginState
	url      string
	errMsg   string
	updated  time.Time
	// code carries the owner's code to the goroutine driving the login.
	code chan string
	// cancel ends the goroutine, and with it the harness's process group.
	cancel context.CancelFunc
	// done is closed once the goroutine has let go of the account's home.
	done chan struct{}
}

func (m *Logins) init() {
	m.once.Do(func() {
		m.byKey = map[loginKey]*hubLogin{}
		m.live = map[account.Ref]*hubLogin{}
		if m.base == nil {
			m.base, m.stop = context.WithCancel(context.Background())
		}
		if m.Binary == nil {
			m.Binary = harness.Locate
		}
		if m.Log == nil {
			m.Log = slog.New(slog.DiscardHandler)
		}
		if m.URLWait <= 0 {
			m.URLWait = loginURLWait
		}
		if m.CodeWait <= 0 {
			m.CodeWait = loginCodeWait
		}
		if m.ExitWait <= 0 {
			m.ExitWait = loginExitWait
		}
	})
}

// bind ties every login to ctx: when it ends, every harness login is killed.
// Serve binds the runner's own context before any loop runs.
func (m *Logins) bind(ctx context.Context) {
	m.base, m.stop = context.WithCancel(ctx)
	m.init()
}

// Close ends every login in flight and waits for each to let go of its home.
func (m *Logins) Close() {
	if m == nil {
		return
	}
	m.init()
	m.stop()
	m.wg.Wait()
}

// Control acts on one of a hub's login controls. It returns at once: a login
// runs on its own, and the loop reports where it is at each sync.
func (m *Logins) Control(conn string, c v1.Control) {
	m.init()
	if !loginIDPattern.MatchString(c.LoginID) {
		// Nothing can be reported for an id a hub could not have given.
		m.Log.Warn("the hub sent a login control with no usable login id; ignored", "connection", conn, "kind", c.Kind)
		return
	}
	switch c.Kind {
	case v1.ControlStartLogin:
		m.start(conn, c, v1.LoginByLink)
	case v1.ControlLoginToken:
		m.start(conn, c, v1.LoginByToken)
	case v1.ControlLoginCode:
		m.takeCode(conn, c)
	case v1.ControlCancelLogin:
		m.cancelLogin(conn, c.LoginID)
	}
}

// Reports is every login this connection's hub has not yet heard the end of,
// in a stable order.
func (m *Logins) Reports(conn string) []v1.LoginReport {
	if m == nil {
		return nil
	}
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []v1.LoginReport
	for k, l := range m.byKey {
		if k.conn == conn {
			out = append(out, l.report())
		}
	}
	slices.SortFunc(out, func(a, b v1.LoginReport) int { return strings.Compare(a.LoginID, b.LoginID) })
	return out
}

// Reported forgets the logins whose end a sync the hub answered carried.
func (m *Logins) Reported(conn string, sent []v1.LoginReport) {
	if m == nil {
		return
	}
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range sent {
		if !r.State.IsTerminal() {
			continue
		}
		k := loginKey{conn, r.LoginID}
		if l, ok := m.byKey[k]; ok && l.state.IsTerminal() {
			delete(m.byKey, k)
		}
	}
}

func (l *hubLogin) report() v1.LoginReport {
	return v1.LoginReport{
		LoginID: l.id, Harness: l.ref.Harness, Account: l.ref.Label, Method: l.method,
		State: l.state, URL: l.url, Error: l.errMsg, UpdatedAt: l.updated.UTC(),
	}
}

// start takes a start_login or a login_token. A repeat of one already taken
// is the same instruction, and changes nothing.
func (m *Logins) start(conn string, c v1.Control, method v1.LoginMethod) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := loginKey{conn, c.LoginID}
	if _, ok := m.byKey[k]; ok {
		return
	}
	if len(m.byKey) >= maxLogins {
		m.Log.Warn("a hub started more logins than this runner holds at once; it is taken once the hub has heard how the others ended",
			"connection", conn, "login", c.LoginID)
		return
	}
	ref := account.Ref{Harness: c.Harness, Label: c.Account}
	l := &hubLogin{conn: conn, id: c.LoginID, ref: ref, method: method, state: v1.LoginStarting,
		updated: time.Now(), code: make(chan string, 1), done: make(chan struct{})}
	m.byKey[k] = l
	m.Log.Info("a hub started a login", "connection", conn, "login", l.id, "harness", ref.Harness, "account", ref.Label, "method", method)
	bin, refusal := m.refusal(ref, method, c.Token)
	if refusal != "" {
		close(l.done)
		m.endLocked(l, v1.LoginFailed, refusal)
		return
	}
	// One login per account: the newer one is what the owner is looking at.
	// The older one may already have ended — cancelled by a cancel_login
	// earlier in the same answer — and still be letting go of the home.
	prev := m.live[ref]
	if prev != nil {
		m.endLocked(prev, v1.LoginCancelled, fmt.Sprintf("login %s for the same account replaced it — follow that one", l.id))
	}
	m.live[ref] = l
	ctx, cancel := context.WithCancel(m.base)
	l.cancel = cancel
	token := c.Token
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(l.done)
		defer func() {
			m.mu.Lock()
			if m.live[ref] == l {
				delete(m.live, ref)
			}
			m.mu.Unlock()
		}()
		defer cancel()
		// Nothing here touches the home until the one before has let go of
		// it — waited for even when this one is cancelled meanwhile, since a
		// login after this one waits only on this one's done.
		if prev != nil {
			<-prev.done
		}
		if ctx.Err() != nil {
			return
		}
		if method == v1.LoginByToken {
			m.byToken(ctx, l, bin, token)
			return
		}
		m.byLink(ctx, l, bin)
	}()
}

// refusal is why this runner will not start a login, judged before anything
// is touched, with the binary that would run it; "" when it will. Its words
// travel to the hub: no path, and each next action a command for whoever
// walks to the machine.
func (m *Logins) refusal(ref account.Ref, method v1.LoginMethod, token string) (string, string) {
	h := ref.Harness
	if h == "" {
		return "", "the hub named no harness to log in — start the login again naming one: hub login logs in " + strings.Join(hubLoginHarnesses, " and ")
	}
	if h == "codex" {
		// The wire carries Codex's device code (user_code); driving its login
		// from here is the follow-up decision 0055 leaves open.
		if ref.Label == "" {
			return "", "logging Codex in from a hub is not built yet — at the machine, `" + m.Paths.RemoteCommand("doctor") + "` names Codex's own login command; with --device-auth it takes a code entered on any other machine"
		}
		return "", "logging Codex in from a hub is not built yet — at the machine, `" + m.Paths.RemoteCommand("account", "add", "codex", ref.Label, "--device") + "` logs it in with a code entered on any other machine"
	}
	if !slices.Contains(hubLoginHarnesses, h) {
		return "", fmt.Sprintf("hub login logs in %s, and %s is not one of them — start the login again naming one, or see at the machine what this runner drives with `%s`",
			strings.Join(hubLoginHarnesses, " and "), named(h), m.Paths.RemoteCommand("doctor"))
	}
	if method == v1.LoginByToken {
		if !account.CanUseToken(h) {
			return "", fmt.Sprintf("%s does not run on a stored token — log it in by link instead, starting the login without one", h)
		}
		if ref.Label == "" {
			// A token is always an account (decision 0054): the default
			// login is the harness's own, and yad keeps nothing in it.
			return "", "a token is stored as an account, and this login names none — name an account the owner listed, or add one at the machine with `" + m.Paths.RemoteCommand("account", "add", h, "<label>", "--token", "-") + "`"
		}
		if err := account.CheckToken(token); err != nil {
			return "", "the token was not stored: " + err.Error()
		}
	}
	if ref.Label != "" {
		if config.ValidName(ref.Label) != nil || !m.Accounts.Lists().Has(ref) {
			// A hub logs in what the owner listed and never adds an account:
			// what is on this machine is the owner's to decide (0055).
			return "", fmt.Sprintf("%s account %q is not one this runner's owner listed, and a hub never adds one — at the machine, `%s` adds it",
				h, ref.Label, m.Paths.RemoteCommand("account", "add", h, ref.Label))
		}
	}
	bin, ok := m.Binary(h)
	if !ok {
		return "", fmt.Sprintf("%s is not installed on this machine — `%s` at the machine shows where it was looked for", h, m.Paths.RemoteCommand("doctor"))
	}
	return bin, ""
}

// takeCode hands the owner's code to a login waiting for one. A repeat, or a
// code for a login already past waiting, is dropped: the hub sends the code
// while it last heard the login waiting, and this runner has moved on.
func (m *Logins) takeCode(conn string, c v1.Control) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.byKey[loginKey{conn, c.LoginID}]
	if !ok {
		m.echo(conn, c.LoginID, v1.LoginFailed, "this runner has no login "+c.LoginID+" in progress — it restarted since, and a login in flight does not survive that; start a new login")
		return
	}
	if l.method != v1.LoginByLink || l.state != v1.LoginWaiting {
		return
	}
	code := strings.TrimSpace(c.Code)
	switch {
	case code == "":
		m.endLocked(l, v1.LoginFailed, "the code was empty — start a new login and paste the whole code")
		return
	case len(code) > maxLoginCode || strings.ContainsFunc(code, func(r rune) bool { return r <= ' ' || r == 0x7f }):
		// Written to the login's stdin, a line break would be a second
		// answer to a question the login did not ask.
		m.endLocked(l, v1.LoginFailed, "the code holds a space or a line break — start a new login and paste only the code")
		return
	}
	m.setLocked(l, v1.LoginChecking, "")
	l.code <- code
}

// cancelLogin ends a login. One this runner never had is reported cancelled
// all the same, so the hub stops asking.
func (m *Logins) cancelLogin(conn, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.byKey[loginKey{conn, id}]
	if !ok {
		m.echo(conn, id, v1.LoginCancelled, "cancelled before this runner had it — start a new login to log the account in")
		return
	}
	m.endLocked(l, v1.LoginCancelled, "cancelled from the hub — start a new login to log the account in")
}

// echo reports a login this runner never had, once, so a hub waiting on it
// hears something rather than repeating the control for ever.
func (m *Logins) echo(conn, id string, state v1.LoginState, msg string) {
	if len(m.byKey) >= maxLogins {
		return
	}
	l := &hubLogin{conn: conn, id: id, state: state, errMsg: msg, updated: time.Now(), done: make(chan struct{})}
	close(l.done)
	m.byKey[loginKey{conn, id}] = l
}

func (m *Logins) set(l *hubLogin, state v1.LoginState, url string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setLocked(l, state, url)
}

// setLocked moves a login that is not over. An end already reported stands.
func (m *Logins) setLocked(l *hubLogin, state v1.LoginState, url string) {
	if l.state.IsTerminal() {
		return
	}
	l.state, l.updated = state, time.Now()
	if url != "" {
		l.url = url
	}
}

func (m *Logins) end(l *hubLogin, state v1.LoginState, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endLocked(l, state, msg)
}

// endLocked ends a login, once, and stops its process.
func (m *Logins) endLocked(l *hubLogin, state v1.LoginState, msg string) {
	if l.state.IsTerminal() {
		return
	}
	l.state, l.errMsg, l.updated = state, msg, time.Now()
	// The link is spent either way, and a stale one shown beside an end
	// would invite the owner to follow it.
	l.url = ""
	if l.cancel != nil {
		l.cancel()
	}
	log := m.Log.With("connection", l.conn, "login", l.id, "harness", l.ref.Harness, "account", l.ref.Label)
	if state == v1.LoginSucceeded {
		log.Info("a hub login took")
		return
	}
	// The reason is an attr: it names the account, and a warning's message
	// is carried to every hub in health.
	log.Warn("a hub login ended without taking", "state", state, "reason", msg)
}

// errRemoved is an account the owner removed before its login could begin.
var errRemoved = errors.New("removed")

// claim is where the login happens: the account's home, made if the owner
// listed it by hand and it was never made, or "" for the harness's default.
// An account is held (Accounts.takeForLogin) until release: the check that
// it is still listed, made after any login it replaced has let go, and the
// hold that keeps a removal from deleting the home under the login — the
// home goes when release lets go of it, with whatever the login wrote there.
//
// Making the home is itself a change of state: an account with a home and no
// row reads free (account.Load). So the needs_login the missing home meant is
// written down before the home is made, and release, on a login that ended any
// way but succeeded, has the account's state written from the harness's own
// check before the hold goes (DEV-138) — in every login's one way out,
// whichever method drove it and however it ended, a daemon stopping included.
func (m *Logins) claim(l *hubLogin) (home string, release func(), err error) {
	if l.ref.Label == "" {
		return "", func() {}, nil
	}
	hold, ok := m.Accounts.takeForLogin(l.ref, l.id)
	if !ok {
		return "", nil, errRemoved
	}
	_, statErr := os.Stat(account.HomeDir(m.Data, l.ref.Harness, l.ref.Label))
	made := errors.Is(statErr, os.ErrNotExist)
	if made {
		m.Accounts.beforeLoginMakesHome(l.ref)
	}
	release = func() {
		m.mu.Lock()
		took := l.state == v1.LoginSucceeded
		m.mu.Unlock()
		if !took {
			m.Accounts.loginNotTaken(l.ref, made)
		}
		m.Accounts.release(hold)
	}
	if home, err = account.Ensure(m.Data, l.ref.Harness, l.ref.Label); err != nil {
		release()
		return "", nil, err
	}
	return home, release, nil
}

// claimed is claim, ending the login when it cannot begin. ok false means it
// has ended.
func (m *Logins) claimed(l *hubLogin, log *slog.Logger) (home string, release func(), ok bool) {
	home, release, err := m.claim(l)
	switch {
	case errors.Is(err, errRemoved):
		m.end(l, v1.LoginCancelled, m.removedReason(l.ref))
		return "", nil, false
	case err != nil:
		log.Error("a hub login could not prepare the account's home", "err", err)
		m.end(l, v1.LoginFailed, "the account's home could not be prepared — `"+m.Paths.RemoteCommand("daemon", "logs")+"` at the machine says why")
		return "", nil, false
	}
	return home, release, true
}

func (m *Logins) removedReason(r account.Ref) string {
	return fmt.Sprintf("%s account %q was removed on the machine, and a hub never adds one back — at the machine, `%s` adds it again",
		r.Harness, r.Label, m.Paths.RemoteCommand("account", "add", r.Harness, r.Label))
}

// hubLoginHarnesses are the harnesses a hub login can log in here. Codex's
// device code has its place on the wire and waits on its runner side
// (decision 0055).
var hubLoginHarnesses = []string{"claude"}

// named is a name a hub sent, fit to quote in words that go back to it: one
// line, bounded, and with no backtick to be read as the start of a command.
func named(s string) string {
	s = strings.ReplaceAll(s, "`", "'")
	if len(s) > 64 {
		s = s[:64] + "…"
	}
	return strconv.Quote(s)
}

// accountRemoved ends the login in flight on an account the owner has just
// removed. Its hold keeps the home until its process has stopped, and the
// home goes then with anything the login wrote in it.
func (m *Logins) accountRemoved(r account.Ref) {
	m.init()
	m.mu.Lock()
	defer m.mu.Unlock()
	if l := m.live[r]; l != nil {
		m.endLocked(l, v1.LoginCancelled, m.removedReason(r))
	}
}

// byLink drives the harness's own login over pipes: the link out, the code
// in, and the harness's own check to say whether it took. Only the link is
// read from what it prints (decision 0055); its wording decides nothing.
func (m *Logins) byLink(ctx context.Context, l *hubLogin, bin string) {
	log := m.Log.With("connection", l.conn, "login", l.id, "harness", l.ref.Harness, "account", l.ref.Label)
	h := l.ref.Harness
	home, release, ok := m.claimed(l, log)
	if !ok {
		return
	}
	// Deferred first, so it runs last: the process is stopped before the
	// home can go.
	defer release()
	dir := home
	if dir == "" {
		dir, _ = os.UserHomeDir()
	}
	// No terminal: the login is driven over pipes, and one that wanted a
	// person at a terminal fails at once rather than waiting for nobody.
	//
	// A stored token stays in its file, for the runs still on the account,
	// and out of this environment: handed to the login it would outrank the
	// login being made (decision 0054). It goes once the new login is
	// confirmed (check).
	//
	// And no browser (DEV-133). Over pipes claude still opens the machine's
	// browser before it prints the link, and a browser here that is signed
	// in to claude.ai finishes the login through its own callback — as
	// whoever is signed in there, with no code at all. A hub login is signed
	// in by the person at the hub, so the harness is handed a browser that
	// opens nothing, and the only way through is the code it was sent.
	// `yad account add`, with the owner at the machine, keeps the browser.
	proc, err := supervise.Start(ctx, supervise.Spec{
		Path: bin, Args: account.LoginArgs(h), Dir: dir, Env: append(account.LoginEnv(h, home), noBrowser),
		Stdin: true, NoTTY: true, MergeStderr: true,
	})
	if err != nil {
		log.Error("a hub login could not start the harness's own login", "err", err)
		m.end(l, v1.LoginFailed, fmt.Sprintf("%s's own login could not be started — `%s` at the machine says why", h, m.Paths.RemoteCommand("daemon", "logs")))
		return
	}
	// Stopped on every way out: a login cancelled, expired or replaced is a
	// process still waiting on a code nobody will send.
	defer func() {
		proc.Stop(supervise.Ladder{TermGrace: 2 * time.Second})
		proc.Stdout().Close()
	}()
	links := make(chan string, 1)
	go readLink(proc.Stdout(), links)

	urlWait := time.NewTimer(m.URLWait)
	defer urlWait.Stop()
	select {
	case link := <-links:
		if link == "" {
			m.end(l, v1.LoginFailed, fmt.Sprintf("%s's own login ended without printing a link to sign in at — its output may have changed; %s",
				h, m.atTheMachine(l)))
			return
		}
		m.set(l, v1.LoginWaiting, link)
	case <-urlWait.C:
		m.end(l, v1.LoginFailed, fmt.Sprintf("%s's own login printed no link to sign in at within %s — its output may have changed; %s",
			h, m.URLWait, m.atTheMachine(l)))
		return
	case <-ctx.Done():
		return
	}

	codeWait := time.NewTimer(m.CodeWait)
	defer codeWait.Stop()
	var code string
	select {
	case code = <-l.code:
	case <-proc.Done():
		if proc.Wait() == nil {
			m.signedInWithoutCode(ctx, l, bin, home, log)
			return
		}
		m.end(l, v1.LoginFailed, fmt.Sprintf("%s's own login stopped while it waited for the code — start a new login", h))
		return
	case <-codeWait.C:
		m.end(l, v1.LoginExpired, fmt.Sprintf("no code arrived within %s of the link — start a new login", m.CodeWait))
		return
	case <-ctx.Done():
		return
	}
	if _, err := io.WriteString(proc.Stdin(), code+"\n"); err != nil {
		// The error names a pipe, never the code; nothing else is logged.
		log.Warn("a hub login could not hand the code to the harness's own login", "err", err)
		m.end(l, v1.LoginFailed, fmt.Sprintf("%s's own login stopped before it took the code — start a new login", h))
		return
	}
	// The login's own success is a condition of this one, never its words:
	// a login that exits non-zero, or is still asking when the deadline
	// comes, did not take this code, whatever the home holds. The check
	// alone would say yes on a credential already there — an account's own
	// login from before it ran on a token — and a mistyped code would then
	// end succeeded and remove the token the owner chose.
	exitWait := time.NewTimer(m.ExitWait)
	defer exitWait.Stop()
	select {
	case <-proc.Done():
	case <-exitWait.C:
		m.end(l, v1.LoginFailed, fmt.Sprintf("%s's own login had not finished %s after the code, so it most likely did not accept it — start a new login", h, m.ExitWait))
		return
	case <-ctx.Done():
		return
	}
	if err := proc.Wait(); err != nil {
		m.end(l, v1.LoginFailed, fmt.Sprintf("%s's own login did not accept the code — it may have been mistyped, cut short or used already; start a new login", h))
		return
	}
	m.check(ctx, l, bin, home, log)
}

// noBrowser is the browser a hub login's harness is given: `true`, which opens
// nothing and exits 0, so the harness goes on to wait for the code. A command
// name rather than a path, found on the login's PATH as any tool of its is.
const noBrowser = "BROWSER=true"

// signedInWithoutCode ends a link login whose harness succeeded before any code
// arrived. The login did not take the hub's code, so it is not this login's
// success; but the home may now hold a login all the same — made by something
// on this machine, as whoever it was signed in as. Saying only that the login
// failed would leave the owner believing the account is still logged out
// while it takes runs on an account they did not choose (DEV-133).
func (m *Logins) signedInWithoutCode(ctx context.Context, l *hubLogin, bin, home string, log *slog.Logger) {
	h := l.ref.Harness
	in, err := account.OwnLogin(ctx, h, bin, home)
	if ctx.Err() != nil {
		return
	}
	if err != nil || !in {
		if err != nil {
			log.Warn("a hub login ended before its code, and whether the account is logged in could not be read", "err", err)
		}
		m.end(l, v1.LoginFailed, fmt.Sprintf("%s's own login stopped while it waited for the code — start a new login", h))
		return
	}
	m.end(l, v1.LoginFailed, fmt.Sprintf("%s's own login finished before any code arrived: something on this machine signed it in, not this login, so the account may now run as whoever that was — check it at the machine with `%s`, and log it in again there if it is not the account you meant",
		h, m.Paths.RemoteCommand("account", "list")))
}

// byToken stores the token as the account's login (decision 0054) and asks
// the harness's own check whether the account now runs.
func (m *Logins) byToken(ctx context.Context, l *hubLogin, bin, token string) {
	log := m.Log.With("connection", l.conn, "login", l.id, "harness", l.ref.Harness, "account", l.ref.Label)
	home, release, ok := m.claimed(l, log)
	if !ok {
		return
	}
	defer release()
	m.set(l, v1.LoginChecking, "")
	if err := account.SetToken(home, token); err != nil {
		// SetToken's own errors name a file, never its contents.
		log.Error("a hub login could not store the token", "err", err)
		m.end(l, v1.LoginFailed, "the token could not be stored — `"+m.Paths.RemoteCommand("daemon", "logs")+"` at the machine says why")
		return
	}
	m.check(ctx, l, bin, home, log)
}

// check ends a login by the harness's own answer, and on a yes puts the
// account in service the way an added account is.
//
// A link login is asked about as the harness's own (account.OwnLogin): with a
// token stored beside it, claude's check says yes to the token whatever the
// login did. Only once that says yes does the token go, since from then it
// would outrank the login in every run. A token login is asked about as its
// runs will run.
func (m *Logins) check(ctx context.Context, l *hubLogin, bin, home string, log *slog.Logger) {
	h := l.ref.Harness
	ask := account.LoggedIn
	if l.method == v1.LoginByLink {
		ask = account.OwnLogin
	}
	in, err := ask(ctx, h, bin, home)
	switch {
	case ctx.Err() != nil:
		return
	case err != nil:
		log.Warn("a hub login could not read whether it took", "err", err)
		m.end(l, v1.LoginFailed, fmt.Sprintf("whether the login took could not be read from %s — `%s` at the machine shows the account's state", h, m.Paths.RemoteCommand("account", "list")))
		return
	case !in:
		m.end(l, v1.LoginFailed, fmt.Sprintf("%s's own check finds no login after it — start a new login, or %s", h, m.atTheMachine(l)))
		return
	}
	if l.method == v1.LoginByLink && home != "" && account.TokenFileExists(home) {
		if err := account.ClearToken(home); err != nil {
			log.Error("a hub login took, and the account's stored token could not be removed", "err", err)
			m.end(l, v1.LoginFailed, "the login took, and the token the account ran on, which would outrank it, could not be removed — `"+m.Paths.RemoteCommand("daemon", "logs")+"` at the machine says why")
			return
		}
	}
	if l.ref.Label == "" {
		// The capability document keeps its answer about a default login
		// for a minute; the owner who just logged in should not wait on it.
		capability.ForgetDefaultLogin(h)
	} else if _, err := m.Accounts.LoggedInAgain(ctx, l.ref); err != nil {
		// Only a removal, or a label no listing could hold, gets here.
		log.Warn("a hub login took on an account no longer listed", "err", err)
		m.end(l, v1.LoginFailed, m.removedReason(l.ref))
		return
	}
	if m.Changed != nil {
		m.Changed()
	}
	m.end(l, v1.LoginSucceeded, "")
}

// atTheMachine is how the owner logs this login's account in at the machine
// instead: `yad account add` for an account, and for the harness's own
// default login `yad doctor`, which names the harness's own command as that
// machine resolves it.
func (m *Logins) atTheMachine(l *hubLogin) string {
	if l.ref.Label == "" {
		return "at the machine, `" + m.Paths.RemoteCommand("doctor") + "` names its own login command"
	}
	return "log it in at the machine with `" + m.Paths.RemoteCommand("account", "add", l.ref.Harness, l.ref.Label) + "`"
}

// readLink reads the login's output until the first link to an authorize
// page, sends it, and drains the rest unread so the login never blocks on a
// full pipe. It sends "" if the output ends first.
func readLink(r io.Reader, links chan<- string) {
	var buf []byte
	chunk := make([]byte, 4096)
	sent := false
	for {
		n, err := r.Read(chunk)
		if !sent && n > 0 && len(buf) < loginOutputCap {
			buf = append(buf, chunk[:min(n, loginOutputCap-len(buf))]...)
			// Only whole lines: a link cut by a read would be sent cut.
			if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
				if link := authorizeLink(buf[:i]); link != "" {
					links <- link
					sent = true
				}
			}
		}
		if err != nil {
			if !sent {
				links <- authorizeLink(buf)
			}
			return
		}
	}
}

// authorizeLink is the first https link in out whose path is an OAuth
// authorize page — claude.com's, claude.ai's or platform.claude.com's today,
// and any other host tomorrow — or "". Nothing else in the output means
// anything to yad.
func authorizeLink(out []byte) string {
	for _, m := range linkPattern.FindAll(out, -1) {
		if len(m) > maxLoginURL {
			continue
		}
		u, err := url.Parse(string(m))
		if err != nil || u.Scheme != "https" || u.Host == "" || !strings.Contains(u.Path, "/oauth/authorize") {
			continue
		}
		return string(m)
	}
	return ""
}
