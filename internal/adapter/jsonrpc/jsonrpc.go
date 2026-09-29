// Package jsonrpc is the JSON-RPC 2.0 client both line-delimited harness
// protocols share: Codex's app-server (DEV-20) and the Agent Client Protocol
// (DEV-44). One JSON object per line in each direction; nothing here requires
// the "jsonrpc" member on what the other side sends, because Codex omits it.
//
// It is deliberately two halves. Conn writes: requests with ids,
// notifications and answers to the other side's own requests, serialised so
// no two lines interleave. Read reads: every line, in order, to one handler —
// the only ordering a caller can build on, because a resume or a fork replays
// history as notifications and the response that ends the replay is one line
// among them. A caller that must wait for an answer uses Call, whose response
// is routed to it instead of to the handler.
//
// Hand-written rather than a dependency (ARCHITECTURE.md §6): the whole of
// what either protocol needs is this file, and the Go SDKs for both lag the
// protocols they speak.
package jsonrpc

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

// Message is one line from the other side, decoded only as far as routing
// needs.
type Message struct {
	// ID is set on a response and on a request from the other side; raw,
	// because that side may number its own requests with strings.
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// IsRequest is a request from the other side — an approval, a permission —
// that needs an answer.
func (m *Message) IsRequest() bool { return m.Method != "" && len(m.ID) > 0 }

// IsNotification is a notification, which needs none.
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

// RPCError is a JSON-RPC error object. Data is kept raw: ACP puts the
// structure a failure is classified by there (an OpenCode prompt's
// errorName), and a caller decodes only what it reads.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

// JSON-RPC's own error codes.
const (
	// CodeMethodNotFound answers a request from the other side we cannot
	// serve.
	CodeMethodNotFound = -32601
	// CodeInvalidParams is a request the other side found malformed — ACP
	// answers a model or an effort it does not have with it.
	CodeInvalidParams = -32602
	// CodeInternal is a failure on the other side's end.
	CodeInternal = -32603
)

// ErrClosed is wrapped by the error of a call whose answer can no longer
// come: the other side's output has ended.
var ErrClosed = errors.New("closed its output before answering")

// Conn is the writing half, and the table of calls waiting on an answer.
type Conn struct {
	// Peer names the other side in errors: "codex app-server", "opencode".
	Peer string

	wmu sync.Mutex
	w   io.Writer
	// Trace, when set, sees every line in both directions — out is ours —
	// for the local diagnostic copy of the conversation (decision 0012) and
	// the fixture recorder. Set before the first read or write.
	Trace func(out bool, line []byte)

	mu      sync.Mutex
	next    int64
	waiting map[int64]chan *Message
	closed  bool
}

// NewConn writes to w; peer names the other side in errors.
func NewConn(w io.Writer, peer string) *Conn {
	return &Conn{Peer: peer, w: w, waiting: map[int64]chan *Message{}}
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
		return nil, c.closedErr()
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
			return nil, c.closedErr()
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

func (c *Conn) closedErr() error { return fmt.Errorf("%s %w", c.Peer, ErrClosed) }

// Notify writes a notification.
func (c *Conn) Notify(method string, params any) error {
	return c.write(request{Method: method, Params: params})
}

// Reply answers a request from the other side.
func (c *Conn) Reply(id json.RawMessage, result any) error {
	return c.write(reply{ID: id, Result: result})
}

// ReplyError refuses a request from the other side.
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
	if c.Trace != nil {
		c.Trace(true, b)
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
	// Raw is the line as the other side wrote it; nil for a skipped line.
	Raw []byte
	// Err is set for a line that could not be used: over the cap, or not
	// JSON-RPC. The stream carries on after it.
	Err error
}

// Read reads the other side's output until it ends, handing every line to
// handle in order — except the responses Call is waiting for. It returns once
// the output has ended, after failing any call still waiting.
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
		if c.Trace != nil {
			c.Trace(false, raw)
		}
		var m Message
		if err := json.Unmarshal(raw, &m); err != nil || (m.Method == "" && len(m.ID) == 0) {
			if err == nil {
				err = errors.New("neither a request, a notification nor a response")
			}
			handle(Line{Raw: raw, Err: fmt.Errorf("an unreadable line from %s was skipped: %w", c.Peer, err)})
			continue
		}
		if m.IsResponse() && c.deliver(&m) {
			continue
		}
		handle(Line{Msg: &m, Raw: raw})
	}
}

// Transcript writes both directions of a conversation to w, one JSON object
// per line, ours wrapped as {">": …}: the shape of the local diagnostic copy
// and of every recorded fixture.
func Transcript(w io.Writer) func(bool, []byte) {
	var mu sync.Mutex
	return func(out bool, line []byte) {
		mu.Lock()
		defer mu.Unlock()
		if out {
			w.Write([]byte(`{">":`))
			w.Write(line)
			w.Write([]byte("}\n"))
			return
		}
		w.Write(line)
		w.Write([]byte{'\n'})
	}
}
