package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeCache puts a models_cache.json in a fresh home, shaped as Codex 0.147.0
// writes it: each model with its slug, visibility, priority, its reasoning
// levels and more besides.
func writeCache(t *testing.T, body string) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, modelsCache), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

const cache = `{"fetched_at":"2026-09-23T12:01:41Z","etag":"W/\"x\"","client_version":"0.147.0","models":[
 {"slug":"gpt-reserve","visibility":"hide","priority":3,"default_reasoning_level":"medium"},
 {"slug":"gpt-5.5","visibility":"list","priority":12,"default_reasoning_level":"medium",
  "supported_reasoning_levels":[{"effort":"low"},{"effort":"xhigh"}]},
 {"slug":"gpt-5.6-sol","visibility":"list","priority":4,"default_reasoning_level":"low",
  "supported_reasoning_levels":[{"effort":"low"},{"effort":"ultra"}],
  "model_messages":{"instructions_template":"You are Codex, an agent based on GPT-5."}},
 {"slug":"gpt-5.6-luna","visibility":"list","priority":8},
 {"slug":"codex-auto-review","visibility":"hide","priority":43}
]}`

func TestModelsAreCodexsListInItsOrder(t *testing.T) {
	got := Models(writeCache(t, cache))
	want := []string{"gpt-5.6-sol", "gpt-5.6-luna", "gpt-5.5"}
	if !slices.Equal(got, want) {
		t.Errorf("models = %v, want %v: listed ones only, by Codex's priority", got, want)
	}
}

// Two logins on different plans each offer what theirs has; the runner
// reports every model some run of it could name, once.
func TestModelsAreTheUnionOfEveryHome(t *testing.T) {
	a := writeCache(t, `{"models":[{"slug":"gpt-5.5","visibility":"list","priority":12},{"slug":"gpt-5.6-luna","visibility":"list","priority":8}]}`)
	b := writeCache(t, `{"models":[{"slug":"gpt-5.6-sol","visibility":"list","priority":4},{"slug":"gpt-5.5","visibility":"list","priority":12}]}`)
	got := Models(a, b)
	want := []string{"gpt-5.6-sol", "gpt-5.6-luna", "gpt-5.5"}
	if !slices.Equal(got, want) {
		t.Errorf("models = %v, want %v", got, want)
	}
}

// Absence is reported by leaving the list short: a login Codex has not run
// under has no cache, and a damaged one is no reason to fail a capability
// document.
func TestModelsMissingOrBrokenAddNothing(t *testing.T) {
	for name, home := range map[string]string{
		"no home":     "",
		"no cache":    t.TempDir(),
		"not json":    writeCache(t, "{"),
		"no models":   writeCache(t, `{"etag":"x"}`),
		"wrong shape": writeCache(t, `{"models":"gpt-5.5"}`),
	} {
		if got := Models(home); len(got) != 0 {
			t.Errorf("%s: models = %v, want none", name, got)
		}
	}
}

// The document reaches every connected hub, so only what looks like a model
// name leaves the file: never a path, a URL or a sentence someone wrote into
// it (DEV-67).
func TestModelsCarryOnlyModelNames(t *testing.T) {
	home := writeCache(t, `{"models":[
 {"slug":"/Users/someone/.codex/auth.json","visibility":"list","priority":1},
 {"slug":"https://user:hunter2@proxy.internal/","visibility":"list","priority":2},
 {"slug":"a model name with spaces","visibility":"list","priority":3},
 {"slug":"`+strings.Repeat("x", 65)+`","visibility":"list","priority":4},
 {"slug":"gpt-5.5","visibility":"list","priority":5}
]}`)
	if got := Models(home); !slices.Equal(got, []string{"gpt-5.5"}) {
		t.Errorf("models = %v, want only gpt-5.5", got)
	}
}

func TestModelsAreBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"models":[`)
	for i := range maxModels + 10 {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"slug":"m%d","visibility":"list","priority":1}`, i)
	}
	b.WriteString("]}")
	if got := Models(writeCache(t, b.String())); len(got) != maxModels {
		t.Errorf("%d models reported, want the bound of %d", len(got), maxModels)
	}
}

func TestDefaultHomeIsCodexs(t *testing.T) {
	t.Setenv("CODEX_HOME", "/srv/codex")
	if got := DefaultHome(); got != "/srv/codex" {
		t.Errorf("with CODEX_HOME set, the default home is %q", got)
	}
	t.Setenv("CODEX_HOME", "")
	t.Setenv("HOME", "/home/someone")
	if got := DefaultHome(); got != "/home/someone/.codex" {
		t.Errorf("without CODEX_HOME, the default home is %q", got)
	}
}
