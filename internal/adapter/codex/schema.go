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
// what `codex app-server generate-json-schema` says for the version below.
// A runner checks the installed version's schema against it and reports
// drift as a warning in its capability document, before a run finds it.
//
// Only the adapter's surface is hashed — the methods it sends, the
// notifications and requests it reads, and every definition they reach. A
// release that adds an unrelated method, or rewords a description, is not
// drift; one that changes the shape of turn/completed is.

// The one Codex release the adapter is pinned to, and its surface hash, from
// testdata/codex-<version>/codex_app_server_protocol.schemas.json. Only the
// latest recorded release is pinned (decision 0067): pinning a new one
// replaces both, so a request may use what that release takes without a
// branch per version, and an older codex gets the drift warning.
const (
	pinnedVersion = "0.157.1"
	pinnedSum     = "12304d548db40115a52970f347f208559bc933dcf4743770bc2182140dfa7586"
)

// schemaFile is the bundle generate-json-schema writes, holding every
// definition.
const schemaFile = "codex_app_server_protocol.schemas.json"

// schemaFileCap bounds the bundle read back. 0.157.1's is under a megabyte;
// one past this is not a schema, and the check reports a failed read.
const schemaFileCap = 64 << 20

// surface is what the adapter speaks: each method under the union that
// defines it, and the responses, which the unions do not name.
var surface = struct {
	methods   map[string][]string
	responses []string
}{
	methods: map[string][]string{
		"ClientRequest": {"initialize", "thread/start", "thread/resume", "thread/fork", "thread/inject_items", "turn/start", "turn/steer",
			"turn/interrupt", "account/rateLimits/read", "account/login/start", "account/login/cancel", "model/list"},
		"ClientNotification": {"initialized"},
		"ServerNotification": {"turn/started", "turn/completed", "item/started", "item/completed",
			"item/agentMessage/delta", "item/reasoning/summaryTextDelta", "item/reasoning/textDelta",
			"thread/tokenUsage/updated", "account/rateLimits/updated", "error", "warning",
			"model/rerouted", "thread/compacted", "account/login/completed"},
		"ServerRequest": {"item/commandExecution/requestApproval", "item/fileChange/requestApproval",
			"item/permissions/requestApproval", "mcpServer/elicitation/request",
			"execCommandApproval", "applyPatchApproval"},
	},
	responses: []string{"InitializeResponse", "ThreadStartResponse", "ThreadResumeResponse", "ThreadForkResponse", "ThreadInjectItemsResponse",
		"TurnStartResponse", "TurnSteerResponse", "GetAccountRateLimitsResponse",
		"CommandExecutionRequestApprovalResponse", "FileChangeRequestApprovalResponse",
		"PermissionsRequestApprovalResponse", "McpServerElicitationRequestResponse",
		"ExecCommandApprovalResponse", "ApplyPatchApprovalResponse", "LoginAccountResponse", "CancelLoginAccountResponse",
		"ModelListResponse"},
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

// failure is why the check could not run, in the runner's own words. The
// warning it becomes is copied into the capability document, which every
// connected hub reads, so it is a type of its own rather than an error: an
// error's text is where a child's stderr, the exec path under the owner's home
// and the temp directory's absolute path end up (DEV-67; DEV-60 found the same
// in Error). Every value is one of those below. What codex printed and what
// the OS said are not kept: the warning names where the owner looks instead —
// the command, run by hand, which prints all of it, or for the temp directory,
// which codex never saw, the variable that names it.
type failure string

var (
	failTempDir = failure("the temp directory it writes into could not be made")
	failStart   = failure("it would not start")
	failTimeout = failure(fmt.Sprintf("it gave no answer within %s", schemaTimeout))
	failExit    = failure("it exited with an error")
	failNoFile  = failure("it wrote no " + schemaFile)
	failRead    = failure("what it wrote could not be read")
)

// schemaRetry is how long a failed check is believed. A check that could not
// run — a loaded machine at boot, a full temp directory — may run next time,
// and a warning that outlives its cause misleads the owner and every hub; a
// codex that can never generate a schema is asked again only this often.
const schemaRetry = 10 * time.Minute

// generateSchema runs `codex app-server generate-json-schema` into a
// throwaway directory and returns its bundle, or why there is none. A parent
// context that ended comes back as a failure too; the caller asks the context.
func generateSchema(parent context.Context, bin string) ([]byte, failure) {
	dir, err := os.MkdirTemp("", "yad-codex-schema-*")
	if err != nil {
		return nil, failTempDir
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(parent, schemaTimeout)
	defer cancel()
	p, err := supervise.Start(ctx, supervise.Spec{Path: bin, Args: []string{"app-server", "generate-json-schema", "--out", dir}})
	if err != nil {
		return nil, failStart
	}
	// What it prints is not wanted, and the read must not wait for EOF: a
	// descendant that left the group with setsid can hold the pipe open for
	// ever, and this runs on the daemon's capability tick. The leader's exit
	// decides, and closing our end ends the read.
	go io.Copy(io.Discard, p.Stdout())
	werr := p.Wait()
	p.Stdout().Close()
	switch {
	case parent.Err() != nil:
		return nil, failStart
	case ctx.Err() != nil:
		return nil, failTimeout
	case werr != nil:
		return nil, failExit
	}
	// Read as models_cache.json is: the file is Codex's, and a FIFO left in
	// its place would hold this read, and the capability tick, for ever.
	b, err := readRegular(filepath.Join(dir, schemaFile), schemaFileCap)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, failNoFile
	case err != nil:
		return nil, failRead
	}
	return b, ""
}

type check struct {
	warning string
	// until is when a failed check is asked again; zero for an answer that
	// holds for the binary and version it was given.
	until time.Time
}

var (
	checkMu sync.Mutex
	checked = map[string]check{}
	now     = time.Now
)

// SchemaWarning compares the installed codex's app-server protocol with the
// pinned one and returns a readiness warning, or "" when it matches. It is
// asked on every capability probe, so an answer is kept per binary and
// version — an upgrade changes the version and is checked again — and a
// check that could not run is kept only for schemaRetry.
func SchemaWarning(ctx context.Context, bin, version string) string {
	key := bin + "\x00" + version
	checkMu.Lock()
	c, ok := checked[key]
	checkMu.Unlock()
	if ok && (c.until.IsZero() || now().Before(c.until)) {
		return c.warning
	}
	w, final, err := schemaWarning(ctx, bin)
	if err != nil {
		// The caller stopped asking; that says nothing about codex.
		return ""
	}
	c = check{warning: w}
	if !final {
		c.until = now().Add(schemaRetry)
	}
	checkMu.Lock()
	checked[key] = c
	checkMu.Unlock()
	return w
}

// schemaWarning checks once. final says the answer holds for this binary and
// version; err, that ctx ended and there is no answer at all.
func schemaWarning(ctx context.Context, bin string) (warning string, final bool, err error) {
	b, f := generateSchema(ctx, bin)
	if ctx.Err() != nil {
		return "", false, ctx.Err()
	}
	if f != "" {
		action := "run `codex app-server generate-json-schema --out DIR` on this machine to see why"
		if f == failTempDir {
			// Codex never ran, so running it by hand shows nothing: the
			// directory is the runner's, named by its environment.
			action = "check that TMPDIR in the runner's environment, or /tmp without it, is a writable directory"
		}
		return "yad could not check this codex's app-server protocol: " + string(f) + " — runs may still work; " + action, false, nil
	}
	sum, err := SchemaHash(b)
	if err != nil {
		// The same bytes hash the same way next time. The error stays
		// unquoted: a JSON syntax error quotes what codex wrote.
		return "yad could not check this codex's app-server protocol: the schema it generated is not one yad can read — runs may still work; run `codex app-server generate-json-schema --out DIR` on this machine to see what it writes", true, nil
	}
	if sum == pinnedSum {
		return "", true, nil
	}
	// The installed version is not repeated here: the report's version field
	// beside the warning carries it, and the warning's text stays wholly the
	// runner's.
	return fmt.Sprintf("this codex's app-server protocol differs from the one this yad was built against (codex %s) in the parts the adapter uses — runs may fail; install codex %s or a yad that knows this one", pinnedVersion, pinnedVersion), true, nil
}
