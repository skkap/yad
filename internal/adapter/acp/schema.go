package acp

// Pinning the protocol (decisions 0067 and 0072). The core was written
// against one release of the ACP schema, kept whole in
// testdata/acp-schema-<version>/: the unstable bundle, because session/fork
// and a prompt's usage are in it and not yet in the stable one. Only the
// core's surface is pinned — the requests it sends, the notifications and
// requests it reads, and every definition they reach — so a schema release
// that adds an unrelated method, or rewords a description, is not drift and
// one that reshapes session/prompt's answer is. schema_test.go holds the
// committed bundle to the hash below; pinning a new release replaces both,
// and drops the older bundle in the same change.
//
// An agent does not print the schema it speaks, so nothing compares an
// installed agent with this pin at run time: initialize's protocolVersion
// is checked, and each agent's release is pinned beside its adapter
// (opencode.PinnedVersion).
const (
	SchemaVersion = "1.23.0"
	schemaSum     = "98c741da69c5cdd26b306007a6f75ba8ea96ba6ea42989841c3f711c447f91a8"
)

// surface is every definition the core reads or writes, by the schema's
// name for it.
var surface = []string{
	"InitializeRequest", "InitializeResponse",
	"NewSessionRequest", "NewSessionResponse",
	"ResumeSessionRequest", "ResumeSessionResponse",
	"ForkSessionRequest", "ForkSessionResponse",
	"ListSessionsRequest", "ListSessionsResponse",
	"SetSessionConfigOptionRequest", "SetSessionConfigOptionResponse",
	"PromptRequest", "PromptResponse",
	"CancelNotification", "SessionNotification",
	"RequestPermissionRequest", "RequestPermissionResponse",
}

// methods is every method the core sends or answers, by its wire name: each
// must be in the pinned release's method table.
var methods = []string{
	"initialize", "session/new", "session/resume", "session/fork", "session/list",
	"session/set_config_option", "session/prompt", "session/cancel",
	"session/update", "session/request_permission",
}
