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
	// archives carries session archives, whose time is their size's: it
	// has no timeout of its own, and each transfer is given one by
	// archiveTimeout.
	archives *http.Client
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
		archives:   config.HubHTTPClient(0),
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

// archiveRate is the slowest a session archive is let go at, and
// archiveTimeout the time one of size bytes is given: a minute, and a second
// for each archiveRate's worth. 256 KiB/s is a slow uplink's; an archive at
// yad hub's default cap then has about seventeen minutes.
const archiveRate = 256 << 10

func archiveTimeout(size int64) time.Duration {
	return time.Minute + time.Duration(size/archiveRate)*time.Second
}

// PutArchive uploads a session archive of size bytes (decision 0081). The
// hub checks the body against sha, and an upload of the same bytes again is
// answered as the first was.
func (c *Client) PutArchive(ctx context.Context, sessionID, sha string, size int64, body io.Reader) (v1.SessionArchive, error) {
	ctx, cancel := context.WithTimeout(ctx, archiveTimeout(size))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+"/sessions/"+url.PathEscape(sessionID)+"/archive", body)
	if err != nil {
		return v1.SessionArchive{}, err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", v1.ArchiveContentType)
	req.Header.Set("Accept", "application/json")
	req.Header.Set(v1.HeaderArchiveSHA256, sha)
	c.headers(req, c.credential)
	resp, err := c.archives.Do(req)
	if err != nil {
		return v1.SessionArchive{}, config.RedactURLError(err)
	}
	defer resp.Body.Close()
	var out v1.SessionArchive
	return out, answer(resp, &out)
}

// GetArchive downloads a session archive into w, at most limit bytes, and
// returns the sha256 the hub says it has and how many bytes came. Checking
// the bytes against the run's session.archive is the caller's.
func (c *Client) GetArchive(ctx context.Context, sessionID string, limit int64, w io.Writer) (string, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, archiveTimeout(limit))
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/sessions/"+url.PathEscape(sessionID)+"/archive", nil)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Accept", v1.ArchiveContentType)
	c.headers(req, c.credential)
	resp, err := c.archives.Do(req)
	if err != nil {
		return "", 0, config.RedactURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", 0, answer(resp, nil)
	}
	// One byte past the limit says it was past it.
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return "", n, config.RedactURLError(err)
	}
	if n > limit {
		return "", n, fmt.Errorf("the hub's archive is larger than the %d bytes the run said it is", limit)
	}
	return resp.Header.Get(v1.HeaderArchiveSHA256), n, nil
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
	c.headers(req, bearer)
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error carries the URL, never the headers, so the bearer stays
		// out; the redaction is for whatever URL a later change lets in.
		return config.RedactURLError(err)
	}
	defer resp.Body.Close()
	return answer(resp, out)
}

// headers are the ones every call carries.
func (c *Client) headers(req *http.Request, bearer string) {
	req.Header.Set(v1.HeaderProtocol, v1.Version)
	req.Header.Set("User-Agent", "yad/"+buildinfo.Version)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
}

// answer reads a hub's JSON answer into out, or its error envelope into a
// StatusError; out nil reads an error alone.
func answer(resp *http.Response, out any) error {
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
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("hub answered %d with a body that is not the protocol's: %w", resp.StatusCode, err)
	}
	return nil
}
