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
	// secrets says this call's answer is one the protocol itself puts a secret
	// in: register answers with the runner credential, and a sync with the
	// grants of every run it offers. A body of one of these that cannot be
	// read is never printed, because what cannot be read cannot be redacted,
	// and these are the two that can hold a secret this suite has never seen
	// — a grant value, or a credential under a name of the hub's own — which
	// no list of known strings could remove afterwards.
	//
	// The other calls carry a credential as a bearer, so a hub could quote one
	// back in an error there too. That one this suite holds, so it is removed
	// from every body whatever the call, and what is left needs a body that
	// will not parse *and* an escaping the raw search misses. Against that the
	// cost of widening is real: a proxy's error page on a wrong URL is the
	// commonest thing this suite prints, and describing it instead would make
	// the most ordinary misconfiguration harder to see.
	secrets bool
	// credential says the answer is a register 200, whose body is a credential
	// under whatever name the hub chose and nothing else worth printing: its
	// timings are read from the decoded value by the checks that judge them.
	credential bool
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

// learn adds a secret this suite holds to what is hidden from the report.
// Length is not a test of one: the protocol puts no floor on a registration
// token, and a hub free to issue a six-character one is free to echo it. A
// short secret that turns a report into a row of redactions is a bad report;
// a printed token is a worse one.
func (c *client) learn(secret string) {
	if secret != "" {
		c.known = append(c.known, secret)
	}
}

// hide removes every secret this suite presented from text that is no longer
// JSON — a body that would not parse, and the sentences a check writes around
// it. It runs before the excerpt is cut, so a secret goes whole rather than
// leaving its head showing.
//
// Each secret is looked for twice: as it is, and as JSON writes it. Go's
// encoder turns & < > into \u0026 \u003c \u003e, so a body this suite
// re-marshalled has escaped anything it did not redact — the redaction step
// itself is what would otherwise put a second secret beyond reach. Two exact
// needles, and no pattern matching: a redactor with false positives is one
// somebody switches off.
func (c *client) hide(text string) string {
	text = c.hideKnown(text)
	for _, secret := range c.known {
		if needle := encoded(secret); needle != "" {
			text = strings.ReplaceAll(text, needle, redactedText)
		}
	}
	return text
}

// hideKnown replaces the secrets this suite presented in text as it stands,
// which for a decoded string is every form a hub could have written.
func (c *client) hideKnown(text string) string {
	for _, secret := range c.known {
		text = strings.ReplaceAll(text, secret, redactedText)
	}
	return text
}

// encoded is a string as JSON writes it, without the quotes.
func encoded(s string) string {
	b, err := json.Marshal(s)
	if err != nil || len(b) < 2 {
		return ""
	}
	quoted := string(b[1 : len(b)-1])
	if quoted == s {
		return "" // nothing was escaped; the raw needle already covers it
	}
	return quoted
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
		secrets:    in.path == registerPath || strings.HasSuffix(in.path, "/sync"),
		credential: in.path == registerPath && res.StatusCode/100 == 2,
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

// redacted is a body with every secret taken out of it. Half the failures here
// print the answer that broke the rule, and the answer that breaks "a token
// registers one runner once" is a 200 carrying a working credential — which
// would then be in the terminal, and in the log of whatever pipeline ran the
// suite, for as long as that log is kept. CLAUDE.md's guardrail is that a
// secret is never printed, and a report about a hub's mistakes is not an
// exception to it.
//
// It returns whether the body could be read at all, because a body that is not
// JSON is not thereby free of secrets: a UTF-8 BOM before the first brace is
// enough for encoding/json to refuse a register answer whose first field is the
// credential, and so is a body over readLimit, which arrives cut mid-object.
// This is the half of the guard that has to fail closed.
func (c *client) redacted(body []byte) (string, bool) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return string(body), false
	}
	scrubbed, found := c.scrub(v, "")
	if !found {
		return string(body), true
	}
	out, err := json.Marshal(scrubbed)
	if err != nil {
		// Unreachable for a value that came out of Unmarshal; if it ever is
		// reached, say nothing rather than print what was being redacted.
		return "[a body this suite could not print without its secrets]", true
	}
	return string(out), true
}

// scrub takes every secret out of a decoded body and returns it with whether
// it took any — which is what decides between re-marshalling the document and
// printing the hub's own bytes, so a replacement that does not say so is a
// replacement thrown away.
//
// Three kinds of secret. The fields v1 puts one in, whatever the value there
// turns out to be: a hub is not obliged to send a string, and a credential
// inside an object under runner_credential is still a credential. The secrets
// this suite itself presented, in any string anywhere — a hub that quotes one
// back inside a message has put it where no walk by field name will look. And
// the same, in an object's keys, because a key is a string a hub wrote and
// `{"<the token> is not valid": true}` is as readable as any message.
//
// Matching the decoded text is what makes the last two exact: Unmarshal has
// already collapsed \u0026, \/ and every other escape some encoder chose, so
// one comparison covers all of them. Matching the encoded text could only ever
// cover the forms this suite thought of, and the hubs it exists for are
// written by other people in other languages.
//
// The grant value is matched by the key it sits under rather than by its own
// name, because "value" alone is a field name any hub might use for something
// harmless.
func (c *client) scrub(v any, under string) (any, bool) {
	switch t := v.(type) {
	case string:
		// Every string reaches this case, including a body that is one: the
		// walk must not depend on a secret being wrapped in an object.
		if hidden := c.hideKnown(t); hidden != t {
			return hidden, true
		}
	case map[string]any:
		found := false
		renamed := map[string]string{}
		for k, child := range t {
			if k == "runner_credential" || under == "grants" && k == "value" {
				t[k], found = json.RawMessage(redaction), true
				continue
			}
			// A source's URL is the one field v1 carries that holds a secret
			// inside a larger string a reader still wants: a run's git source
			// may arrive as https://user:token@host/repo.git, and the host
			// and path are worth printing while the credentials are not.
			if text, ok := child.(string); ok && k == "url" {
				if hidden := redactedURL(text); hidden != text {
					t[k], found = hidden, true
					continue
				}
			}
			if scrubbed, ok := c.scrub(child, k); ok {
				t[k], found = scrubbed, true
			}
			if hidden := c.hideKnown(k); hidden != k {
				renamed[k] = hidden
			}
		}
		// After the walk: a map must not be written to while it is ranged.
		for from, to := range renamed {
			t[to], found = t[from], true
			delete(t, from)
		}
		return t, found
	case []any:
		found := false
		for i, child := range t {
			if scrubbed, ok := c.scrub(child, under); ok {
				t[i], found = scrubbed, true
			}
		}
		return t, found
	}
	return v, false
}

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
	if a.credential {
		// Never printed, by any check: a register that answered 200 has put a
		// credential in the body under a name this suite may not know, and
		// every failure that prints such an answer is a failure about a hub
		// that registered something it should not have.
		return fmt.Sprintf("%s -> %d, and the body is not printed because a register answer carries a credential", a.Call, a.Status)
	}
	text, read := a.c.redacted(a.Body)
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
