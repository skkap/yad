package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/skkap/yad/internal/supervise"
)

// Pinning the app-server protocol (DEV-22, decision 0037). The app-server is
// marked experimental and Codex ships weekly; the adapter was written against
// what `codex app-server generate-json-schema` says for the versions below.
// A runner checks the installed version's schema against them and reports
// drift as a warning in its capability document, before a run finds it.
//
// Only the adapter's surface is hashed — the methods it sends, the
// notifications and requests it reads, and every definition they reach. A
// release that adds an unrelated method, or rewords a description, is not
// drift; one that changes the shape of turn/completed is.

// pinned maps each known surface hash to the Codex version it was recorded
// from, in testdata/codex-<version>/codex_app_server_protocol.schemas.json.
var pinned = map[string]string{
	"6ea9f30289e32723059a16e951c8069a5e93ce0223083e6dc0d78e3bc3a4ca3c": "0.147.0",
}

// schemaFile is the bundle generate-json-schema writes, holding every
// definition.
const schemaFile = "codex_app_server_protocol.schemas.json"

// surface is what the adapter speaks: each method under the union that
// defines it, and the responses, which the unions do not name.
var surface = struct {
	methods   map[string][]string
	responses []string
}{
	methods: map[string][]string{
		"ClientRequest": {"initialize", "thread/start", "thread/resume", "turn/start", "turn/steer",
			"turn/interrupt", "account/rateLimits/read"},
		"ClientNotification": {"initialized"},
		"ServerNotification": {"turn/started", "turn/completed", "item/started", "item/completed",
			"item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/reasoning/textDelta",
			"thread/tokenUsage/updated", "account/rateLimits/updated", "error", "warning",
			"model/rerouted", "thread/compacted"},
		"ServerRequest": {"item/commandExecution/requestApproval", "item/fileChange/requestApproval",
			"item/permissions/requestApproval", "mcpServer/elicitation/request",
			"execCommandApproval", "applyPatchApproval"},
	},
	responses: []string{"InitializeResponse", "ThreadStartResponse", "ThreadResumeResponse",
		"TurnStartResponse", "TurnSteerResponse", "GetAccountRateLimitsResponse",
		"CommandExecutionRequestApprovalResponse", "FileChangeRequestApprovalResponse",
		"PermissionsRequestApprovalResponse", "McpServerElicitationRequestResponse",
		"ExecCommandApprovalResponse", "ApplyPatchApprovalResponse"},
}

// SchemaHash hashes the adapter's surface of a generated schema bundle. What
// the surface names and the bundle lacks is hashed as missing, so a removed
// method moves the hash too.
func SchemaHash(bundle []byte) (string, error) {
	var doc map[string]any
	if err := json.Unmarshal(bundle, &doc); err != nil {
		return "", fmt.Errorf("the codex schema is not JSON: %w", err)
	}
	defs, _ := doc["definitions"].(map[string]any)
	if defs == nil {
		return "", errors.New("the codex schema has no definitions — is it generate-json-schema's bundle?")
	}
	resolve := func(ref string) any {
		var node any = doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, ok := node.(map[string]any)
			if !ok {
				return nil
			}
			node = m[part]
		}
		return node
	}
	byName := func(name string) (string, any) {
		for _, ref := range []string{"#/definitions/v2/" + name, "#/definitions/" + name} {
			if d := resolve(ref); d != nil {
				return ref, d
			}
		}
		return "#/definitions/" + name, nil
	}

	roots := map[string]any{}
	for union, methods := range surface.methods {
		_, u := byName(union)
		for _, m := range methods {
			roots[union+" "+m] = unionEntry(u, m)
		}
	}
	for _, name := range surface.responses {
		ref, d := byName(name)
		roots[ref] = d
	}

	reached := map[string]any{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if ref, ok := x["$ref"].(string); ok {
				if _, seen := reached[ref]; !seen {
					d := resolve(ref)
					reached[ref] = d
					walk(d)
				}
			}
			for k, val := range x {
				if k != "$ref" {
					walk(val)
				}
			}
		case []any:
			for _, val := range x {
				walk(val)
			}
		}
	}
	walk(roots)
	for ref, d := range roots {
		if strings.HasPrefix(ref, "#/") {
			reached[ref] = d
		}
	}
	// encoding/json writes map keys sorted, so this is canonical.
	b, err := json.Marshal(map[string]any{"methods": stripDocs(roots), "definitions": stripDocs(reached)})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// unionEntry is the member of a request or notification union for one
// method, or nil when the union has none.
func unionEntry(union any, method string) any {
	u, _ := union.(map[string]any)
	for _, e := range asSlice(u["oneOf"]) {
		props, _ := e.(map[string]any)["properties"].(map[string]any)
		m, _ := props["method"].(map[string]any)
		if slices.Contains(asSlice(m["enum"]), any(method)) || m["const"] == method {
			return e
		}
	}
	return nil
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// stripDocs drops what only documents: a reworded description is not a
// protocol change.
func stripDocs(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if k != "description" && k != "title" {
				out[k] = stripDocs(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = stripDocs(val)
		}
		return out
	}
	return v
}

// schemaTimeout bounds generating the schema. It takes tens of milliseconds;
// a codex that takes seconds is broken in a way its version probe reports.
const schemaTimeout = 10 * time.Second

// generateSchema runs `codex app-server generate-json-schema` into a
// throwaway directory and returns its bundle.
func generateSchema(ctx context.Context, bin string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "yad-codex-schema-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, schemaTimeout)
	defer cancel()
	p, err := supervise.Start(ctx, supervise.Spec{Path: bin, Args: []string{"app-server", "generate-json-schema", "--out", dir}})
	if err != nil {
		return nil, err
	}
	io.Copy(io.Discard, io.LimitReader(p.Stdout(), 1<<20))
	p.Stdout().Close()
	if err := p.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("no answer within %s", schemaTimeout)
		}
		return nil, fmt.Errorf("%v %s", err, strings.TrimSpace(p.Stderr()))
	}
	b, err := os.ReadFile(filepath.Join(dir, schemaFile))
	if errors.Is(err, fs.ErrNotExist) {
		// The throwaway directory's path would say nothing useful.
		return nil, errors.New("it wrote no " + schemaFile)
	}
	return b, err
}

var (
	checkMu sync.Mutex
	checked = map[string]string{}
)

// SchemaWarning compares the installed codex's app-server protocol with the
// pinned ones and returns a readiness warning, or "" when it matches. It is
// asked on every capability probe, so the answer is kept per binary and
// version; an upgrade changes the version and is checked again.
func SchemaWarning(ctx context.Context, bin, version string) string {
	key := bin + "\x00" + version
	checkMu.Lock()
	w, ok := checked[key]
	checkMu.Unlock()
	if ok {
		return w
	}
	w = schemaWarning(ctx, bin, version)
	checkMu.Lock()
	checked[key] = w
	checkMu.Unlock()
	return w
}

func schemaWarning(ctx context.Context, bin, version string) string {
	b, err := generateSchema(ctx, bin)
	if err != nil {
		return fmt.Sprintf("yad could not check this codex's app-server protocol (%v) — runs may still work; `codex app-server generate-json-schema --out DIR` shows the error", err)
	}
	sum, err := SchemaHash(b)
	if err != nil {
		return fmt.Sprintf("yad could not check this codex's app-server protocol: %v", err)
	}
	if _, ok := pinned[sum]; ok {
		return ""
	}
	known := make([]string, 0, len(pinned))
	for _, v := range pinned {
		known = append(known, v)
	}
	slices.Sort(known)
	return fmt.Sprintf("the app-server protocol of %s differs from the one this yad was built against (codex %s) in the parts the adapter uses — runs may fail; install a pinned codex or a yad that knows this one", version, strings.Join(known, ", "))
}
