package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"

	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/supervise"
)

// Codex names no models in the catalog: which it offers depends on the
// account's plan, and the list moves with Codex releases rather than with
// yad's. Codex fetches that list itself and keeps it in its home, and this
// reads it back, so a runner reports what its logins are actually offered
// without spending a request of its own (DEV-124).

// modelsCache is the file in a Codex home holding the model list Codex last
// fetched for that login.
const modelsCache = "models_cache.json"

// modelsCacheCap bounds the read. Codex keeps each model's whole instructions
// template in the file, so it runs to a few hundred kilobytes; a file past
// this is not one Codex wrote, and reporting nothing is the safe answer.
const modelsCacheCap = 8 << 20

// maxModels bounds what one runner reports. Codex lists a handful; the bound
// keeps a damaged cache from growing every capability document.
const maxModels = 64

// modelName is what a model slug looks like: gpt-5.5, gpt-5.6-luna,
// o4-mini. The charset is the guarantee that nothing else from the file —
// a path, a URL, a sentence — reaches the capability document every
// connected hub reads (DEV-67).
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// cachedModel is the part of an entry in models_cache.json that is read.
// Visibility is Codex's own: "list" is shown in its picker, "hide" is a
// model it keeps for itself, such as the one that reviews its own work.
type cachedModel struct {
	Slug       string `json:"slug"`
	Visibility string `json:"visibility"`
	Priority   int    `json:"priority"`
}

// DefaultHome is the home Codex uses for a run with no account: CODEX_HOME
// in the runner's environment, which a run inherits, or ~/.codex.
func DefaultHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

// Models is every model Codex lists in these homes' caches, each once, in
// Codex's own order: the priority it gives each model, which is the order of
// its picker. A model two logins both offer sorts where the first one puts
// it. A home with no cache, or one that does not parse, adds nothing:
// absence is reported by leaving the list short, never as an error, because
// a runner that has not yet run Codex under a login knows no models for it.
func Models(homes ...string) []string {
	type ranked struct {
		name     string
		priority int
	}
	seen := map[string]bool{}
	var all []ranked
	for _, home := range homes {
		if home == "" {
			continue
		}
		for _, m := range readModels(filepath.Join(home, modelsCache)) {
			if seen[m.Slug] || m.Visibility != "list" || !modelName.MatchString(m.Slug) {
				continue
			}
			seen[m.Slug] = true
			all = append(all, ranked{m.Slug, m.Priority})
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].priority < all[j].priority })
	var out []string
	for _, r := range all {
		if len(out) == maxModels {
			break
		}
		out = append(out, r.name)
	}
	return out
}

func readModels(path string) []cachedModel {
	b, err := readRegular(path, modelsCacheCap)
	if err != nil {
		return nil
	}
	var cache struct {
		Models []cachedModel `json:"models"`
	}
	if json.Unmarshal(b, &cache) != nil {
		return nil
	}
	return cache.Models
}

// Asking rather than reading (DEV-50): model/list is Codex's own answer for
// the login the environment points it at — its plan, its config's provider
// and its own filtering — where models_cache.json is only what it last
// fetched, and is not there at all for a login that has never run. Measured
// on 0.157.1, it is answered from that same cache, or with one GET of Codex's
// model catalog when the cache is older than Codex keeps it: no thread is
// started and no token is spent. Models above stays the fallback for a Codex
// that cannot be asked.

// maxModelPages bounds the paging. Codex answers a handful of models in one
// page; the bound is for a server that keeps handing back a cursor.
const maxModelPages = 8

// errNoModels is an answer that named no model yad can report.
var errNoModels = adapter.ModelsError(adapter.ErrModelsUnread, "codex app-server listed no models", nil)

// ListModels asks the codex at bin for the models it offers the login env
// points it at, over its app-server, in Codex's own order and without the
// models it hides from its picker. It starts one app-server and ends it
// before returning; ctx bounds the whole of it.
//
// env is the one a run on that login gets (account.Env), so the answer is
// about the login the run would use (DEV-62: the rule supervise.Spec states for what a run keeps). The
// error is yad's own words and never Codex's: nothing it printed is kept.
func ListModels(ctx context.Context, bin, dir string, env []string) ([]string, error) {
	return listModels(ctx, bin, dir, env, nil)
}

func listModels(ctx context.Context, bin, dir string, env []string, raw io.Writer) ([]string, error) {
	p, err := supervise.Start(ctx, supervise.Spec{
		Path: bin, Args: []string{"app-server", "--listen", "stdio://"},
		Dir: dir, Env: env, Stdin: true, NoTTY: true,
	})
	if err != nil {
		return nil, adapter.ModelsError(adapter.ErrModelsNoStart, "codex would not start to list its models", err)
	}
	conn := NewConn(p.Stdin())
	if raw != nil {
		conn.trace = transcript(raw)
	}
	eof := make(chan struct{})
	go func() {
		defer close(eof)
		conn.Read(p.Stdout(), func(l Line) {
			// Nothing is asked that needs approving; a request that comes
			// anyway gets a refusal rather than silence, which would hold it.
			if l.Msg != nil && l.Msg.IsRequest() {
				conn.ReplyError(l.Msg.ID, codeMethodNotFound, "yad only lists models here")
			}
		})
	}()
	defer func() {
		// Its input closed, the app-server exits by itself, as it does after
		// a run; one that does not is stopped.
		p.Stdin().Close()
		select {
		case <-p.Done():
		case <-time.After(exitGrace):
			p.Stop(supervise.Ladder{TermGrace: termGrace})
		}
		select {
		case <-eof:
		case <-time.After(drainGrace):
		}
		p.Stdout().Close()
	}()

	if _, err := conn.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": "yad", "title": "YAD", "version": buildinfo.Version},
	}); err != nil {
		return nil, callError("initialize", err)
	}
	if err := conn.Notify("initialized", nil); err != nil {
		return nil, callError("initialized", err)
	}
	seen := map[string]bool{}
	var out []string
	cursor := ""
	for range maxModelPages {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := conn.Call(ctx, "model/list", params)
		if err != nil {
			return nil, listError(err)
		}
		var page struct {
			Data []struct {
				Model  string `json:"model"`
				Hidden bool   `json:"hidden"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			return nil, adapter.ModelsError(adapter.ErrModelsUnread, "codex app-server answered model/list with something that is not a model list", nil)
		}
		for _, m := range page.Data {
			if m.Hidden || seen[m.Model] || !modelName.MatchString(m.Model) || len(out) == maxModels {
				continue
			}
			seen[m.Model] = true
			out = append(out, m.Model)
		}
		if page.NextCursor == nil || *page.NextCursor == "" || len(out) == maxModels {
			break
		}
		cursor = *page.NextCursor
	}
	if len(out) == 0 {
		return nil, errNoModels
	}
	return out, nil
}

// callError is a failed step in yad's words. An error Codex answered with is
// its message, which is not kept: only that it refused.
func callError(method string, err error) error {
	if _, ok := errors.AsType[*RPCError](err); ok {
		return fmt.Errorf("codex app-server refused %s", method)
	}
	return fmt.Errorf("codex app-server did not answer %s: %w", method, err)
}

// listError is callError for model/list itself. Only a refusal of the request
// as such says this Codex is older than it (adapter.ErrModelsRefused): Codex
// reads a method it does not know as an unknown variant of its request enum
// and answers -32600 (recorded in login-device-unsupported.jsonl), and
// -32601 is JSON-RPC's own word for it. Any other error — bad params from a
// Codex newer than this adapter, an internal one — is an answer yad could not
// use, which an upgrade of Codex would not fix.
func listError(err error) error {
	if e, ok := errors.AsType[*RPCError](err); ok {
		if e.Code == codeInvalidRequest || e.Code == codeMethodNotFound {
			return adapter.ModelsError(adapter.ErrModelsRefused, "codex app-server refused model/list", nil)
		}
		return adapter.ModelsError(adapter.ErrModelsUnread, "codex app-server answered model/list with an error", nil)
	}
	return callError("model/list", err)
}

// errNotRegular is a path that is not a plain file.
var errNotRegular = errors.New("not a regular file")

// readRegular reads a file Codex wrote, refusing anything but a regular file
// of at most max bytes. What sits at the path is the harness's, or whatever
// put it in the harness's home, and a plain open of a FIFO blocks until a
// writer comes — which on the capability probe would stall the runner's
// every sync. Opened without blocking and checked on the descriptor, so a
// path swapped between a check and the open cannot slip past it.
func readRegular(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errNotRegular
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("larger than %d bytes", max)
	}
	return b, nil
}
