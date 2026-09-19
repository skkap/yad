package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/skkap/yad/internal/adapter"
)

// The JSON-RPC 2.0 client for `codex app-server --listen stdio://` (DEV-20):
// one JSON object per line in each direction. Codex omits the "jsonrpc"
// member from what it sends, so nothing here requires it.
//
// It is deliberately two halves. Conn writes: requests with ids, notifications
// and answers to the server's own requests, serialised so no two lines
// interleave. Read reads: every line, in order, to one handler — the only
// ordering a caller can build on, because a resume replays history as
// notifications and the response that ends the replay is one line among them.
// A caller that must wait for an answer uses Call, whose response is routed to
// it instead of to the handler.

// Message is one line from the server, decoded only as far as routing needs.
type Message struct {
	// ID is set on a response and on a server request; raw, because the server
	// may number its own requests with strings.
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// IsRequest is a server-initiated request — an approval — that needs an answer.
func (m *Message) IsRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// IsNotification is a server notification, which needs none.
func (m *Message) IsNotification() bool { return m.Method != "" && len(m.ID) == 0 }

// IsResponse answers one of our requests.
func (m *Message) IsResponse() bool { return m.Method == "" && len(m.ID) > 0 }

// RequestID is a response's id as ours are numbered, or -1.
func (m *Message) RequestID() int64 {
	n, err := strconv.ParseInt(string(m.ID), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// JSON-RPC's own error codes, for answering a server request we cannot serve.
const (
	codeMethodNotFound = -32601
)

// ErrClosed is returned for a call whose answer can no longer come: the
// server's output has ended.
var ErrClosed = errors.New("codex app-server closed its output before answering")

// Conn is the writing half, and the table of calls waiting on an answer.
type Conn struct {
	wmu sync.Mutex
	w   io.Writer
	// trace, when set, sees every line in both directions — out is ours —
	// for the local diagnostic copy of the conversation (decision 0012) and
	// the fixture recorder. Set before the first read or write.
	trace func(out bool, line []byte)

	mu      sync.Mutex
	next    int64
	waiting map[int64]chan *Message
	closed  bool
}

// NewConn writes to w.
func NewConn(w io.Writer) *Conn {
	return &Conn{w: w, waiting: map[int64]chan *Message{}}
}

// Send writes a request and returns its id without waiting. Its response goes
// to Read's handler.
func (c *Conn) Send(method string, params any) (int64, error) {
	c.mu.Lock()
	c.next++
	id := c.next
	c.mu.Unlock()
	return id, c.write(request{ID: &id, Method: method, Params: params})
}

// Call writes a request and waits for its response, which Read hands here
// rather than to the handler.
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.next++
	id := c.next
	ch := make(chan *Message, 1)
	c.waiting[id] = ch
	c.mu.Unlock()
	if err := c.write(request{ID: &id, Method: method, Params: params}); err != nil {
		c.forget(id)
		return nil, err
	}
	select {
	case m := <-ch:
		if m == nil {
			return nil, ErrClosed
		}
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-ctx.Done():
		c.forget(id)
		return nil, ctx.Err()
	}
}

// Notify writes a notification.
func (c *Conn) Notify(method string, params any) error {
	return c.write(request{Method: method, Params: params})
}

// Reply answers a server request.
func (c *Conn) Reply(id json.RawMessage, result any) error {
	return c.write(reply{ID: id, Result: result})
}

// ReplyError refuses a server request.
func (c *Conn) ReplyError(id json.RawMessage, code int, msg string) error {
	return c.write(reply{ID: id, Error: &RPCError{Code: code, Message: msg}})
}

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type reply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

func (c *Conn) write(v any) error {
	switch m := v.(type) {
	case request:
		m.JSONRPC = "2.0"
		v = m
	case reply:
		m.JSONRPC = "2.0"
		if m.Result == nil && m.Error == nil {
			m.Result = struct{}{}
		}
		v = m
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.trace != nil {
		c.trace(true, b)
	}
	_, err = c.w.Write(append(b, '\n'))
	return err
}

func (c *Conn) forget(id int64) {
	c.mu.Lock()
	delete(c.waiting, id)
	c.mu.Unlock()
}

// deliver hands a response to the Call waiting for it, if one is.
func (c *Conn) deliver(m *Message) bool {
	id := m.RequestID()
	c.mu.Lock()
	ch, ok := c.waiting[id]
	delete(c.waiting, id)
	c.mu.Unlock()
	if ok {
		ch <- m
	}
	return ok
}

// shut fails every call still waiting, and every later one.
func (c *Conn) shut() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for id, ch := range c.waiting {
		close(ch)
		delete(c.waiting, id)
	}
}

// Line is what Read hands its handler: a message, or why a line was not one.
type Line struct {
	Msg *Message
	// Raw is the line as the server wrote it; nil for a skipped line.
	Raw []byte
	// Err is set for a line that could not be used: over the cap, or not
	// JSON-RPC. The stream carries on after it.
	Err error
}

// Read reads the server's output until it ends, handing every line to handle
// in order — except the responses Call is waiting for. It returns once the
// output has ended, after failing any call still waiting.
func (c *Conn) Read(r io.Reader, handle func(Line)) {
	defer c.shut()
	lr := adapter.NewLineReader(r)
	for {
		line, err := lr.Next()
		if tooLong, ok := errors.AsType[*adapter.ErrLineTooLong](err); ok {
			handle(Line{Err: tooLong})
			continue
		}
		if err != nil {
			return // EOF, or the pipe closed under us
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		// line aliases the reader's buffer, which the next read reuses.
		raw := bytes.Clone(line)
		if c.trace != nil {
			c.trace(false, raw)
		}
		var m Message
		if err := json.Unmarshal(raw, &m); err != nil || (m.Method == "" && len(m.ID) == 0) {
			if err == nil {
				err = errors.New("neither a request, a notification nor a response")
			}
			handle(Line{Raw: raw, Err: fmt.Errorf("an unreadable line from codex was skipped: %w", err)})
			continue
		}
		if m.IsResponse() && c.deliver(&m) {
			continue
		}
		handle(Line{Msg: &m, Raw: raw})
	}
}

// threadOf is the threadId a notification or server request names, or "".
// Codex runs subagents as threads of their own on the same pipe, so anything
// naming another thread is not this run's.
func threadOf(params json.RawMessage) string {
	var p struct {
		ThreadID string `json:"threadId"`
	}
	if len(params) == 0 || json.Unmarshal(params, &p) != nil {
		return ""
	}
	return p.ThreadID
}
