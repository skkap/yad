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
		http: &http.Client{
			// A poll waits up to hubapi.MaxWait by design; the margin is for
			// the answer itself.
			Timeout: hubapi.MaxWait + 30*time.Second,
			// Refused, not followed: Go would carry the token across a
			// same-host redirect to plain http.
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return fmt.Errorf("the hub redirected to %s — use the hub's final address", req.URL.Redacted())
			},
		},
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
		// url.Error carries the URL, never the headers: the token stays out.
		return err
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
