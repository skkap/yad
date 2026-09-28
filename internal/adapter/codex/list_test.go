package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/adapter"
)

// listed is what the recorded login offered on 0.157.1, in Codex's order.
var listed = []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5"}

// list runs ListModels against the fake playing fixture, and returns what it
// answered and what the fake saw.
func list(t *testing.T, fixture string, env map[string]string, timeout time.Duration) ([]string, seen, error) {
	t.Helper()
	h := &harness{fixture: fixture, env: env}
	spec := h.spec(t)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	got, err := ListModels(ctx, spec.Binary, spec.Workdir, spec.Env)
	return got, h.seen(t), err
}

// The list is model/list's answer, asked over the app-server a run uses, with
// no thread started: nothing is run, so nothing is spent (DEV-50).
func TestListModelsAsksTheAppServer(t *testing.T) {
	got, s, err := list(t, fixture("list-models"), nil, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, listed) {
		t.Errorf("models = %v, want %v", got, listed)
	}
	if strings.Join(s.argv, " ") != "app-server --listen stdio://" {
		t.Errorf("argv = %v", s.argv)
	}
	for _, m := range s.stdin {
		if method, _ := m["method"].(string); strings.HasPrefix(method, "thread/") || strings.HasPrefix(method, "turn/") {
			t.Errorf("sent %s: listing models must start no thread and no turn", method)
		}
	}
	if len(s.sent("model/list")) != 1 {
		t.Errorf("model/list sent %d times, want once", len(s.sent("model/list")))
	}
}

// derived writes a fixture: the recorded handshake, then these lines.
func derived(t *testing.T, lines ...string) string {
	t.Helper()
	b, err := os.ReadFile(fixture("list-models"))
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Split(strings.TrimSpace(string(b)), "\n")
	var head []string
	for _, l := range all {
		if strings.Contains(l, `"model/list"`) {
			break
		}
		head = append(head, l)
	}
	path := filepath.Join(t.TempDir(), "list.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(append(head, lines...), "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func asked(id int, cursor string) string {
	params := "{}"
	if cursor != "" {
		params = `{"cursor":"` + cursor + `"}`
	}
	return `{">":{"jsonrpc":"2.0","id":` + itoa(id) + `,"method":"model/list","params":` + params + `}}`
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// Codex's picker is what is reported: a model it hides stays out, and so does
// anything that is not shaped like a model name, since the document reaches
// every hub (DEV-67). A second page is followed to its end.
func TestListModelsKeepsOnlyListedNamesAcrossPages(t *testing.T) {
	path := derived(t,
		asked(2, ""),
		`{"id":2,"result":{"data":[{"model":"gpt-5.5","hidden":false},{"model":"codex-auto-review","hidden":true},{"model":"/Users/someone/.codex/auth.json","hidden":false}],"nextCursor":"p2"}}`,
		asked(3, "p2"),
		`{"id":3,"result":{"data":[{"model":"gpt-5.6-luna","hidden":false},{"model":"gpt-5.5","hidden":false}],"nextCursor":null}}`,
	)
	got, s, err := list(t, path, nil, 20*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gpt-5.5", "gpt-5.6-luna"}; !slices.Equal(got, want) {
		t.Errorf("models = %v, want %v", got, want)
	}
	pages := s.sent("model/list")
	if len(pages) != 2 || !strings.Contains(mustJSON(pages[1]), `"cursor":"p2"`) {
		t.Errorf("model/list requests = %v, want the second to carry the cursor", pages)
	}
}

// A Codex that cannot answer is an error, in yad's words: what the caller
// reports instead is its own fallback, and nothing Codex said travels.
func TestListModelsFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture func(t *testing.T) string
		env     map[string]string
		want    string
		// kind is what the caller words its reason by (DEV-146); nil is a
		// Codex that ended before it answered, which wraps none.
		kind error
	}{
		{"refused", func(t *testing.T) string {
			return derived(t, asked(2, ""), `{"id":2,"error":{"code":-32601,"message":"secret at /Users/someone"}}`)
		}, nil, "refused model/list", adapter.ErrModelsRefused},
		// How Codex itself refuses a method it does not know, an unknown
		// variant of its request enum (login-device-unsupported.jsonl) — and
		// how model/list fails to load its configuration, with the same code.
		{"unknown to it", func(t *testing.T) string {
			return derived(t, asked(2, ""), `{"id":2,"error":{"code":-32600,"message":"Invalid request: unknown variant `+"`model/list`"+` at /Users/someone"}}`)
		}, nil, "refused model/list", adapter.ErrModelsRefused},
		{"failed inside", func(t *testing.T) string {
			return derived(t, asked(2, ""), `{"id":2,"error":{"code":-32603,"message":"secret at /Users/someone"}}`)
		}, nil, "refused model/list", adapter.ErrModelsRefused},
		{"nothing listed", func(t *testing.T) string {
			return derived(t, asked(2, ""), `{"id":2,"result":{"data":[],"nextCursor":null}}`)
		}, nil, "listed no models", adapter.ErrModelsUnread},
		{"not a list", func(t *testing.T) string {
			return derived(t, asked(2, ""), `{"id":2,"result":{"data":"gpt-5.5"}}`)
		}, nil, "not a model list", adapter.ErrModelsUnread},
		{"died", func(t *testing.T) string { return derived(t) }, map[string]string{"CODEX_TEST_MODE": "died"}, "did not answer model/list", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := list(t, tc.fixture(t), tc.env, 20*time.Second)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("models %v, err %v; want an error saying %q", got, err, tc.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Errorf("the error quotes Codex: %v", err)
			}
			for _, k := range []error{adapter.ErrModelsNoStart, adapter.ErrModelsRefused, adapter.ErrModelsUnread} {
				if got, want := errors.Is(err, k), k == tc.kind; got != want {
					t.Errorf("errors.Is(err, %v) = %v, want %v; err %v", k, got, want, err)
				}
			}
		})
	}
}

// One that never answers is given up on when the caller's context ends, and
// its process is stopped, not waited for.
func TestListModelsDoesNotWaitOnASilentCodex(t *testing.T) {
	start := time.Now()
	_, _, err := list(t, fixture("list-models"), map[string]string{"CODEX_TEST_MODE": "silent"}, time.Second)
	if err == nil {
		t.Fatal("a silent codex listed models")
	}
	if took := time.Since(start); took > exitGrace+termGrace+drainGrace+5*time.Second {
		t.Errorf("gave up after %s", took)
	}
}
