package codex

import (
	"encoding/json"
	"io"

	"github.com/skkap/yad/internal/adapter/jsonrpc"
)

// The JSON-RPC 2.0 client for `codex app-server --listen stdio://` (DEV-20)
// is the one ACP uses too (internal/adapter/jsonrpc); these names keep the
// adapter reading as it did before the two shared it.
type (
	Message  = jsonrpc.Message
	RPCError = jsonrpc.RPCError
	Conn     = jsonrpc.Conn
	Line     = jsonrpc.Line
)

// codeMethodNotFound answers a server request we cannot serve.
const codeMethodNotFound = jsonrpc.CodeMethodNotFound

// NewConn writes to w, naming the app-server in errors.
func NewConn(w io.Writer) *Conn { return jsonrpc.NewConn(w, "codex app-server") }

// transcript writes both directions of the conversation to w, ours wrapped.
func transcript(w io.Writer) func(bool, []byte) { return jsonrpc.Transcript(w) }

// threadOf is the thread a notification or server request names, or "".
// Codex runs subagents as threads of their own on the same pipe, so anything
// naming another thread is not this run's. The older approval requests
// (execCommandApproval, applyPatchApproval) name it conversationId.
func threadOf(params json.RawMessage) string {
	var p struct {
		ThreadID       string `json:"threadId"`
		ConversationID string `json:"conversationId"`
	}
	if len(params) == 0 || json.Unmarshal(params, &p) != nil {
		return ""
	}
	if p.ThreadID != "" {
		return p.ThreadID
	}
	return p.ConversationID
}
