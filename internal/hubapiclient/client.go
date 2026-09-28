// Package hubapiclient is the caller's side of yad hub's service API
// (protocol/hubapi): submit a run, read it, follow its events. It is what
// `yad hub submit` and `yad hub watch` use; a TypeScript service generates the
// same calls from protocol/hubapi/openapi.yaml.
package hubapiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/hubclient"
)

// Client talks to one hub's service API.
type Client struct {
	base  string
	token string
	http  *http.Client
}

// New returns a client for the hub at hubURL — its root, as `yad hub serve`
// prints it, without the API's base path — authenticating with an admin token.
func New(hubURL, token string) (*Client, error) {
	// The admin token rides on every request, so the same rule as for a
	// runner credential: https, or plain http to this machine only.
	if err := config.CheckHubURL(hubURL); err != nil {
		return nil, err
	}
	return &Client{
		base:  strings.TrimRight(hubURL, "/") + hubapi.BasePath,
		token: token,
		// A poll waits up to hubapi.MaxWait by design; the margin is for the
		// answer itself.
		http: config.HubHTTPClient(hubapi.MaxWait + 30*time.Second),
	}, nil
}

// Submit queues a run.
func (c *Client) Submit(ctx context.Context, req hubapi.SubmitRequest) (hubapi.Run, error) {
	var out hubapi.Run
	err := c.do(ctx, http.MethodPost, "/runs", req, &out)
	return out, err
}

// Run reads a run's state.
func (c *Client) Run(ctx context.Context, runID string) (hubapi.Run, error) {
	var out hubapi.Run
	err := c.do(ctx, http.MethodGet, "/runs/"+url.PathEscape(runID), nil, &out)
	return out, err
}

// Cancel cancels a run: at once when no runner has started it, otherwise at
// its runner's next sync.
func (c *Client) Cancel(ctx context.Context, runID string) (hubapi.Run, error) {
	var out hubapi.Run
	err := c.do(ctx, http.MethodPost, "/runs/"+url.PathEscape(runID)+"/cancel", struct{}{}, &out)
	return out, err
}

// Interrupt ends a running run's turn and keeps its session.
func (c *Client) Interrupt(ctx context.Context, runID string) (hubapi.Run, error) {
	var out hubapi.Run
	err := c.do(ctx, http.MethodPost, "/runs/"+url.PathEscape(runID)+"/interrupt", struct{}{}, &out)
	return out, err
}

// Steer adds input to a running run's turn.
func (c *Client) Steer(ctx context.Context, runID, text string) (hubapi.Run, error) {
	var out hubapi.Run
	err := c.do(ctx, http.MethodPost, "/runs/"+url.PathEscape(runID)+"/steer", hubapi.SteerRequest{Text: text}, &out)
	return out, err
}

// Runners lists every runner the hub knows, with the health each one last
// reported, the ones still syncing first.
func (c *Client) Runners(ctx context.Context) ([]hubapi.Runner, error) {
	var out hubapi.RunnerList
	err := c.do(ctx, http.MethodGet, "/runners", nil, &out)
	return out.Runners, err
}

// Runner reads one runner and the health its last sync carried.
func (c *Client) Runner(ctx context.Context, runnerID string) (hubapi.Runner, error) {
	var out hubapi.Runner
	err := c.do(ctx, http.MethodGet, "/runners/"+url.PathEscape(runnerID), nil, &out)
	return out, err
}

// Drain asks a runner to drain: it takes no new runs, lets those it holds
// finish, and exits.
func (c *Client) Drain(ctx context.Context, runnerID string) (hubapi.Runner, error) {
	var out hubapi.Runner
	err := c.do(ctx, http.MethodPost, "/runners/"+url.PathEscape(runnerID)+"/drain", struct{}{}, &out)
	return out, err
}

// CloseSession asks for a session to close: at once when no runner holds it,
// otherwise at its runner's next sync once no run of it is held there.
func (c *Client) CloseSession(ctx context.Context, sessionID string) (hubapi.Session, error) {
	var out hubapi.Session
	err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(sessionID)+"/close", struct{}{}, &out)
	return out, err
}

// Session reads a session.
func (c *Client) Session(ctx context.Context, sessionID string) (hubapi.Session, error) {
	var out hubapi.Session
	err := c.do(ctx, http.MethodGet, "/sessions/"+url.PathEscape(sessionID), nil, &out)
	return out, err
}

// StartLogin logs an account in on a runner (decision 0055): by link, or by
// token when the request carries one.
func (c *Client) StartLogin(ctx context.Context, runnerID string, req hubapi.LoginRequest) (hubapi.Login, error) {
	var out hubapi.Login
	err := c.do(ctx, http.MethodPost, "/runners/"+url.PathEscape(runnerID)+"/logins", req, &out)
	return out, err
}

// Login reads a login: its state, and its link while it waits for a code.
func (c *Client) Login(ctx context.Context, runnerID, loginID string) (hubapi.Login, error) {
	var out hubapi.Login
	err := c.do(ctx, http.MethodGet, c.loginPath(runnerID, loginID), nil, &out)
	return out, err
}

// SendLoginCode sends the code the owner got after signing in.
func (c *Client) SendLoginCode(ctx context.Context, runnerID, loginID, code string) (hubapi.Login, error) {
	var out hubapi.Login
	err := c.do(ctx, http.MethodPost, c.loginPath(runnerID, loginID)+"/code", hubapi.LoginCodeRequest{Code: code}, &out)
	return out, err
}

// CancelLogin ends a login at its runner's next sync.
func (c *Client) CancelLogin(ctx context.Context, runnerID, loginID string) (hubapi.Login, error) {
	var out hubapi.Login
	err := c.do(ctx, http.MethodPost, c.loginPath(runnerID, loginID)+"/cancel", struct{}{}, &out)
	return out, err
}

// RemoveAccount asks a runner to remove an account (decision 0057): it is
// sent at the runner's next syncs until its reports leave the account out.
func (c *Client) RemoveAccount(ctx context.Context, runnerID, harness, label string) (hubapi.Account, error) {
	var out hubapi.Account
	err := c.do(ctx, http.MethodPost, c.accountPath(runnerID, harness, label)+"/remove", struct{}{}, &out)
	return out, err
}

// Account reads an account as the runner's last health has it, and whether
// a removal is waiting.
func (c *Client) Account(ctx context.Context, runnerID, harness, label string) (hubapi.Account, error) {
	var out hubapi.Account
	err := c.do(ctx, http.MethodGet, c.accountPath(runnerID, harness, label), nil, &out)
	return out, err
}

func (c *Client) accountPath(runnerID, harness, label string) string {
	return "/runners/" + url.PathEscape(runnerID) + "/accounts/" + url.PathEscape(harness) + "/" + url.PathEscape(label)
}

func (c *Client) loginPath(runnerID, loginID string) string {
	return "/runners/" + url.PathEscape(runnerID) + "/logins/" + url.PathEscape(loginID)
}

// Events long-polls once for the events after the cursor, waiting up to wait
// when there are none.
func (c *Client) Events(ctx context.Context, runID string, after int64, wait time.Duration) (hubapi.EventPage, error) {
	q := url.Values{}
	q.Set("after", strconv.FormatInt(after, 10))
	q.Set("wait_ms", strconv.FormatInt(wait.Milliseconds(), 10))
	var out hubapi.EventPage
	err := c.do(ctx, http.MethodGet, "/runs/"+url.PathEscape(runID)+"/events?"+q.Encode(), nil, &out)
	return out, err
}

// Follow polls a run's events from the cursor until the run is done, handing
// each page to fn as it arrives, and returns the run as it ended. A failed
// poll is retried with backoff unless the hub refused it outright: a network
// blip must not end a watch on a run that takes hours.
func (c *Client) Follow(ctx context.Context, runID string, after int64, fn func(hubapi.EventPage) error) (hubapi.Run, error) {
	backoff := time.Second
	for {
		page, err := c.Events(ctx, runID, after, hubapi.DefaultWait)
		if err != nil {
			if ctx.Err() != nil || permanent(err) {
				return hubapi.Run{}, err
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return hubapi.Run{}, ctx.Err()
			}
			backoff = min(2*backoff, 30*time.Second)
			continue
		}
		backoff = time.Second
		if err := fn(page); err != nil {
			return page.Run, err
		}
		after = page.NextAfter
		if page.Done {
			return page.Run, nil
		}
	}
}

// permanent is an answer that asking again will not change: the request
// itself is wrong. A 5xx or no answer at all may be gone on the next try, and
// so may a 408 or 429 — the hub sends neither, but a proxy or rate limiter in
// front of it does, and that is a blip, not a refusal.
func permanent(err error) bool {
	var se *hubclient.StatusError
	if !errors.As(err, &se) {
		return false
	}
	switch se.Status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests:
		return false
	}
	return se.Status >= 400 && se.Status < 500
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "yad/"+buildinfo.Version)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error carries the URL, never the headers, so the bearer stays
		// out; the redaction is for whatever URL a later change lets in.
		return config.RedactURLError(err)
	}
	defer resp.Body.Close()
	// A page is at most hubapi.MaxPage events of at most a few tens of KiB.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := &hubclient.StatusError{Status: resp.StatusCode}
		var env v1.ErrorEnvelope
		if json.Unmarshal(raw, &env) == nil && env.Error.Code != "" {
			se.Protocol = &env.Error
		}
		return se
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("hub answered %d with a body that is not the service API's: %w", resp.StatusCode, err)
	}
	return nil
}
