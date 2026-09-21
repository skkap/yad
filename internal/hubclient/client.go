// Package hubclient is the runner's side of the v1 protocol: typed calls to one
// hub over HTTPS, the protocol headers on every request, and one decoder for the
// error envelope every hub must use.
package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/config"
)

// Client talks to one hub. The zero value is not usable; use New.
type Client struct {
	base       string
	credential string
	http       *http.Client
}

// requestTimeout bounds one call. A sync is small; a hub that takes longer than
// this is down for our purposes, and the sync loop's backoff takes over.
const requestTimeout = 30 * time.Second

// New returns a client for the hub at baseURL, authenticating with the runner
// credential (empty before registration).
func New(baseURL, credential string) (*Client, error) {
	if err := config.CheckHubURL(baseURL); err != nil {
		return nil, err
	}
	return &Client{
		base:       strings.TrimRight(baseURL, "/"),
		credential: credential,
		http:       config.HubHTTPClient(requestTimeout),
	}, nil
}

// Register exchanges a one-time registration token for a runner credential.
// The token is used for this one request and never stored.
func (c *Client) Register(ctx context.Context, registrationToken string, req v1.RegisterRequest) (v1.RegisterResponse, error) {
	var out v1.RegisterResponse
	err := c.do(ctx, registrationToken, "/runners/register", req, &out)
	return out, err
}

// Sync is the periodic call.
func (c *Client) Sync(ctx context.Context, runnerID string, req v1.SyncRequest) (v1.SyncResponse, error) {
	var out v1.SyncResponse
	err := c.do(ctx, c.credential, "/runners/"+url.PathEscape(runnerID)+"/sync", req, &out)
	return out, err
}

// Events uploads a batch from the spool and returns the hub's authoritative
// acknowledgement.
func (c *Client) Events(ctx context.Context, runID string, batch v1.EventBatch) (v1.EventAck, error) {
	var out v1.EventAck
	err := c.do(ctx, c.credential, "/runs/"+url.PathEscape(runID)+"/events", batch, &out)
	return out, err
}

// Result reports a terminal state.
func (c *Client) Result(ctx context.Context, runID string, res v1.Result) error {
	return c.do(ctx, c.credential, "/runs/"+url.PathEscape(runID)+"/result", res, &v1.Ack{})
}

// Deregister retires this runner's credential.
func (c *Client) Deregister(ctx context.Context, runnerID, reason string) error {
	return c.do(ctx, c.credential, "/runners/"+url.PathEscape(runnerID)+"/deregister", v1.DeregisterRequest{Reason: reason}, &v1.Ack{})
}

// StatusError is a non-2xx answer. Protocol carries the hub's envelope when it
// sent one; a proxy's HTML error page leaves it nil.
type StatusError struct {
	Status   int
	Protocol *v1.Error
}

func (e *StatusError) Error() string {
	if e.Protocol != nil {
		return fmt.Sprintf("hub answered %d: %s", e.Status, e.Protocol.Error())
	}
	return fmt.Sprintf("hub answered %d with no protocol error — is the connection URL a YAD hub?", e.Status)
}

// Code returns the protocol error code, or "" when the hub sent none.
func Code(err error) string {
	var se *StatusError
	if errors.As(err, &se) && se.Protocol != nil {
		return se.Protocol.Code
	}
	return ""
}

func (c *Client) do(ctx context.Context, bearer, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set(v1.HeaderProtocol, v1.Version)
	req.Header.Set("User-Agent", "yad/"+buildinfo.Version)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error carries the URL, never the headers, so the bearer stays
		// out; the redaction is for whatever URL a later change lets in.
		return config.RedactURLError(err)
	}
	defer resp.Body.Close()
	// A hub's answer is small; anything past this is a misbehaving server, and
	// reading it unbounded would let one grow the runner's memory.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := &StatusError{Status: resp.StatusCode}
		var env v1.ErrorEnvelope
		if json.Unmarshal(raw, &env) == nil && env.Error.Code != "" {
			se.Protocol = &env.Error
		}
		return se
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("hub answered %d with a body that is not the protocol's: %w", resp.StatusCode, err)
	}
	return nil
}
