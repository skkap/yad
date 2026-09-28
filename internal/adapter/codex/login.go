package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/supervise"
)

// DeviceLogin is Codex's own device-code login, driven over its app-server
// for a hub login (DEV-135, decision 0057): account/login/start with
// chatgptDeviceCode answers with a link and a code for the owner to type
// there, and account/login/completed says whether they did. Both are
// structure, so nothing Codex words is read — only the link and the code
// leave this process, and they are what the owner is shown.
//
// Whether the login took is not decided here: completion is a condition, and
// the caller's own login check (`codex login status`) is the answer.
//
// One app-server per login, so every completion it sends is this login's.
type DeviceLogin struct {
	p    *supervise.Process
	conn *Conn
	eof  chan struct{}

	mu        sync.Mutex
	id        string
	completed chan struct{}
	done      bool
	succeeded bool
	closed    sync.Once
}

// DeviceCode is what the owner is shown: where to go, and what to type there.
type DeviceCode struct {
	URL, UserCode string
}

// The bounds on what the app-server's answer may carry. They are Codex's
// output travelling to a hub, so one past them is not a device code.
const (
	maxDeviceURL  = 4096
	maxDeviceCode = 64
)

// ErrNoDeviceCode is an answer to account/login/start that is not a device
// code: the wrong type, no login id, a link that is not https, or a code that
// is not one short line.
var ErrNoDeviceCode = errors.New("codex app-server answered account/login/start with no usable device code")

// cancelWait bounds account/login/cancel. The app-server answers from memory;
// a login ended from the hub does not wait long on one that will not.
const cancelWait = 2 * time.Second

// StartDeviceLogin starts `codex app-server --listen stdio://` for a login.
// Ending ctx kills it at once, with no account/login/cancel first: a caller
// that means to cancel the login passes a context it ends only after Close.
func StartDeviceLogin(ctx context.Context, bin, dir string, env []string) (*DeviceLogin, error) {
	p, err := supervise.Start(ctx, supervise.Spec{
		Path: bin, Args: []string{"app-server", "--listen", "stdio://"},
		Dir: dir, Env: env, Stdin: true, NoTTY: true,
	})
	if err != nil {
		return nil, err
	}
	d := &DeviceLogin{p: p, conn: NewConn(p.Stdin()), eof: make(chan struct{}), completed: make(chan struct{})}
	go func() {
		defer close(d.eof)
		d.conn.Read(p.Stdout(), d.handle)
	}()
	return d, nil
}

// Begin is the handshake and the login's start. An error the app-server
// answered with is an *RPCError, carrying Codex's words: the caller keeps
// them to itself.
func (d *DeviceLogin) Begin(ctx context.Context) (DeviceCode, error) {
	if _, err := d.conn.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "yad", "title": "YAD", "version": buildinfo.Version},
	}); err != nil {
		return DeviceCode{}, err
	}
	if err := d.conn.Notify("initialized", nil); err != nil {
		return DeviceCode{}, err
	}
	raw, err := d.conn.Call(ctx, "account/login/start", map[string]any{"type": "chatgptDeviceCode"})
	if err != nil {
		return DeviceCode{}, err
	}
	var r struct {
		Type            string `json:"type"`
		LoginID         string `json:"loginId"`
		VerificationURL string `json:"verificationUrl"`
		UserCode        string `json:"userCode"`
	}
	// Without its id the login could not be cancelled, and a code typed after
	// the runner gave up on it would still log the account in.
	if err := json.Unmarshal(raw, &r); err != nil || r.Type != "chatgptDeviceCode" || r.LoginID == "" || !usableURL(r.VerificationURL) || !usableCode(r.UserCode) {
		return DeviceCode{}, ErrNoDeviceCode
	}
	d.mu.Lock()
	d.id = r.LoginID
	d.mu.Unlock()
	return DeviceCode{URL: r.VerificationURL, UserCode: r.UserCode}, nil
}

func usableURL(s string) bool {
	if s == "" || len(s) > maxDeviceURL || strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

func usableCode(s string) bool {
	return s != "" && len(s) <= maxDeviceCode && !strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f })
}

// handle reads the app-server's lines for the one that matters. A request
// from the server gets a refusal rather than silence, which would hold it.
func (d *DeviceLogin) handle(l Line) {
	switch {
	case l.Msg == nil:
	case l.Msg.IsRequest():
		d.conn.ReplyError(l.Msg.ID, codeMethodNotFound, "yad drives only a login here")
	case l.Msg.Method == "account/login/completed":
		var p struct {
			Success bool `json:"success"`
		}
		ok := json.Unmarshal(l.Msg.Params, &p) == nil && p.Success
		d.mu.Lock()
		defer d.mu.Unlock()
		if !d.done {
			d.done, d.succeeded = true, ok
			close(d.completed)
		}
	}
}

// Completed is closed once the app-server says the login is over, either way.
func (d *DeviceLogin) Completed() <-chan struct{} { return d.completed }

// Succeeded says whether the completion said success. The error Codex gave
// with a failure is Codex's words, and is not kept.
func (d *DeviceLogin) Succeeded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.succeeded
}

// Exited is closed once the app-server's output has ended.
func (d *DeviceLogin) Exited() <-chan struct{} { return d.eof }

// Cancel asks the app-server to stop polling for the code, so a login given
// up on here is not finished by an owner who types the code a minute later.
func (d *DeviceLogin) Cancel() error {
	d.mu.Lock()
	id := d.id
	d.mu.Unlock()
	if id == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), cancelWait)
	defer cancel()
	_, err := d.conn.Call(ctx, "account/login/cancel", map[string]any{"loginId": id})
	return err
}

// Close ends the app-server: its input closed, which is how it learns to
// exit — after writing whatever the login left to write — then signals if it
// has not within grace.
func (d *DeviceLogin) Close(grace time.Duration) {
	d.closed.Do(func() {
		d.p.Stdin().Close()
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-d.p.Done():
		case <-timer.C:
			d.p.Stop(supervise.Ladder{TermGrace: termGrace})
		}
		select {
		case <-d.eof:
		case <-time.After(drainGrace):
		}
		d.p.Stdout().Close()
	})
}
