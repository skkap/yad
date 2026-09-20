package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/buildinfo"
)

// requestTimeout bounds one call. The suite waits on a lease, never on a
// single request: a hub that takes longer than this to answer a sync is down
// for the purposes of the check that asked.
const requestTimeout = 30 * time.Second

// readLimit caps what one answer contributes to memory. Every body in the
// protocol is small, and a hub answering a sync with a gigabyte is a finding
// the truncated excerpt still shows.
const readLimit = 1 << 20

// bodyExcerpt is how much of a body a failure prints: enough for the whole
// error envelope, which is what most failures are reading, and short of the
// wall of JSON a sync answer full of run specifications would be.
const bodyExcerpt = 300

// call is one protocol request. Its fields exist so a check can send what a
// typed client would not let it: a missing protocol header, a body no Go type
// produces, a method the protocol does not use.
type call struct {
	path   string
	bearer string
	body   any
	// raw is sent instead of body when set, for the rules about fields the
	// protocol's own types cannot carry.
	raw json.RawMessage
	// method is POST unless a check is proving another one is refused.
	method string
	// protocol is the Yad-Protocol header. nil sends the version this suite
	// speaks; it is a pointer so a check can send an empty one.
	protocol *string
}

// answer is a hub's response as a check reads it: the status, the headers and
// the bytes. Nothing is decoded away, because half of §2's wire rules are
// about which fields the JSON does and does not carry.
type answer struct {
	// c is the client that made the call, for the secrets it holds.
	c *client
	// Call is the request that got it — "POST /runners/r-1/sync" — because a
	// failure has to say which call broke the rule.
	Call   string
	Status int
	Body   []byte
	// secrets says this call's answer is one the protocol lets carry a secret:
	// register answers with the runner credential, and a sync with the grants
	// of every run it offers. A body of one of these that cannot be read is
	// never printed, because what cannot be read cannot be redacted.
	secrets bool
}

// client is the suite's whole HTTP surface. It is deliberately not
// internal/hubclient: that one hides the status, the raw body and the headers,
// and those three are what the checks read.
type client struct {
	base string
	http *http.Client
	// seen is every answer in order, for the checks that judge all of them —
	// the error envelope, and the controls a hub may send.
	seen []*answer
	// known is the secrets this suite itself presented: the registration
	// token, and the credential the hub gave back for it. Redacting by field
	// name cannot catch a hub that echoes one inside a message — "token sk-…
	// is not valid" is valid JSON with no field a walk would know — and that
	// is one of the commonest ways a token reaches a log. Matching strings
	// the suite already holds is exact; guessing at token-shaped text is not,
	// and a redactor that hides hub identifiers is one somebody turns off.
	known []string
}

func newClient(base string, known ...string) *client {
	c := &client{
		base: strings.TrimRight(base, "/"),
		http: &http.Client{
			Timeout: requestTimeout,
			// Go keeps the Authorization header across a same-host redirect
			// whatever the scheme, so a hub answering 307 to http:// would be
			// handed the credential in cleartext. No protocol call redirects.
			CheckRedirect: func(req *http.Request, _ []*http.Request) error {
				return fmt.Errorf("the hub redirected to %s; no protocol call redirects", req.URL.Redacted())
			},
		},
	}
	for _, secret := range known {
		c.learn(secret)
	}
	return c
}

// shortestSecret is the length below which a string is not treated as one. A
// secret this short is not a secret, and replacing every occurrence of a short
// string would take the report apart.
const shortestSecret = 8

// learn adds a secret this suite holds to what is hidden from the report.
func (c *client) learn(secret string) {
	if len(secret) >= shortestSecret {
		c.known = append(c.known, secret)
	}
}

// hide removes every secret this suite presented from text, wherever in it
// they are and whatever the shape around them. It runs before the excerpt is
// cut, so a secret is removed whole rather than left with its head showing.
func (c *client) hide(text string) string {
	for _, secret := range c.known {
		text = strings.ReplaceAll(text, secret, redactedText)
	}
	return text
}

func (c *client) do(ctx context.Context, in call) (*answer, error) {
	body := []byte(in.raw)
	if in.raw == nil && in.body != nil {
		b, err := json.Marshal(in.body)
		if err != nil {
			return nil, err
		}
		body = b
	}
	method := in.method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+in.path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "yad/"+buildinfo.Version)
	switch {
	case in.protocol == nil:
		req.Header.Set(v1.HeaderProtocol, v1.Version)
	case *in.protocol != "":
		req.Header.Set(v1.HeaderProtocol, *in.protocol)
	}
	if in.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+in.bearer)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, in.path, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, readLimit))
	if err != nil {
		return nil, fmt.Errorf("%s %s: reading the answer: %w", method, in.path, err)
	}
	a := &answer{
		c: c, Call: method + " " + in.path, Status: res.StatusCode, Body: raw,
		secrets: in.path == registerPath || strings.HasSuffix(in.path, "/sync"),
	}
	c.seen = append(c.seen, a)
	return a, nil
}

func (a *answer) ok() bool { return a.Status/100 == 2 }

// What replaces a secret in a printed body: redactedText inside a string a hub
// wrote, and redaction where a whole JSON value is being replaced. They are the
// same words, and the quotes belong to the JSON rather than to the words —
// putting the quoted form inside someone's message would leave the printed
// body malformed for the reader.
const (
	redactedText = "[redacted by yad conformance]"
	redaction    = `"` + redactedText + `"`
)

// registerPath is the one call whose answer carries the runner credential.
const registerPath = "/runners/register"

// redacted is a body with the two secrets v1 carries removed: the runner
// credential a register answers with, and the value of every grant in a run a
// sync offers. Half the failures here print the answer that broke the rule,
// and the answer that breaks "a token registers one runner once" is a 200
// carrying a working credential — which would then be in the terminal, and in
// the log of whatever pipeline ran the suite, for as long as that log is kept.
// CLAUDE.md's guardrail is that a secret is never printed, and a report about
// a hub's mistakes is not an exception to it.
// It returns whether the body could be read at all, because a body that is not
// JSON is not thereby free of secrets: a UTF-8 BOM before the first brace is
// enough for encoding/json to refuse a register answer whose first field is the
// credential, and so is a body over readLimit, which arrives cut mid-object.
// This is the half of the guard that has to fail closed.
func redacted(body []byte) (string, bool) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return string(body), false
	}
	if !scrub(v, "") {
		return string(body), true
	}
	out, err := json.Marshal(v)
	if err != nil {
		// Unreachable for a value that came out of Unmarshal; if it ever is
		// reached, say nothing rather than print what was being redacted.
		return "[a body this suite could not print without its secrets]", true
	}
	return string(out), true
}

// scrub replaces every secret v1 carries, wherever in the body it is, and
// reports whether it replaced any. The grant value is matched by the key it
// sits under rather than by its own name, because "value" alone is a field
// name any hub might use for something harmless.
func scrub(v any, under string) bool {
	found := false
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if _, isString := child.(string); isString && (k == "runner_credential" || under == "grants" && k == "value") {
				t[k], found = json.RawMessage(redaction), true
				continue
			}
			found = scrub(child, k) || found
		}
	case []any:
		for _, child := range t {
			found = scrub(child, under) || found
		}
	}
	return found
}

// decode reads the body into v. A hub that answers 200 with something else has
// broken the call, so the failure says that rather than leaking a Go type name.
func (a *answer) decode(v any) error {
	if err := json.Unmarshal(a.Body, v); err != nil {
		return brokenf("the body is not the JSON this call answers with (%s): %s", err, a)
	}
	return nil
}

// fields is the body as it arrived, for the rules about which keys are present.
func (a *answer) fields() (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(a.Body, &m); err != nil {
		return nil, brokenf("the body is not a JSON object: %s", a)
	}
	return m, nil
}

// envelope is the error this answer carries, and whether it carries one at all.
func (a *answer) envelope() (v1.Error, bool) {
	var e v1.ErrorEnvelope
	if err := json.Unmarshal(a.Body, &e); err != nil {
		return v1.Error{}, false
	}
	if e.Error.Code == "" && e.Error.Message == "" && e.Error.NextAction == "" {
		return v1.Error{}, false
	}
	return e.Error, true
}

// String is what a failure prints: the call, the status, and enough of the
// body to show what happened — with the secrets the protocol carries taken
// out of it first.
func (a *answer) String() string {
	text, read := redacted(a.Body)
	text = a.c.hide(text)
	if !read && a.secrets {
		// A proxy's error page would have been worth printing. It is not
		// worth a credential, and from outside there is no telling the two
		// apart — so this call's unreadable bodies are described, not shown.
		return fmt.Sprintf("%s -> %d with %d bytes that are not JSON; this call's answer can carry a secret, so it is described rather than printed", a.Call, a.Status, len(a.Body))
	}
	body := strings.TrimSpace(text)
	if len(body) > bodyExcerpt {
		body = strings.ToValidUTF8(body[:bodyExcerpt], "") + "..."
	}
	if body == "" {
		return fmt.Sprintf("%s -> %d with an empty body", a.Call, a.Status)
	}
	return fmt.Sprintf("%s -> %d %s", a.Call, a.Status, body)
}
