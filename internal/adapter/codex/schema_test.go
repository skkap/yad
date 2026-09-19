package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recordedSchema(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtures, schemaFile))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The pin in schema.go is the recorded schema's: a changed surface that was
// not re-pinned, or a pin typed by hand, fails here.
func TestPinnedSchema(t *testing.T) {
	sum, err := SchemaHash(recordedSchema(t))
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := pinned[sum]; !ok || v != strings.TrimPrefix(filepath.Base(fixtures), "codex-") {
		t.Fatalf("the recorded schema hashes to %s, which schema.go pins as %q", sum, v)
	}
}

// edit changes a copy of the recorded schema.
func edit(t *testing.T, change func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(recordedSchema(t), &doc); err != nil {
		t.Fatal(err)
	}
	change(doc)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func v2(doc map[string]any) map[string]any {
	return doc["definitions"].(map[string]any)["v2"].(map[string]any)
}

// top is the bundle's own definitions, where the unions live.
func top(doc map[string]any) map[string]any {
	return doc["definitions"].(map[string]any)
}

// Only the adapter's surface counts: what it does not read may change freely,
// and so may any description.
func TestSchemaHashCoversTheSurface(t *testing.T) {
	base, err := SchemaHash(recordedSchema(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(doc map[string]any)
		moves  bool
	}{
		{name: "a definition nothing used reaches", change: func(doc map[string]any) {
			v2(doc)["FuzzyFileSearchParams"] = map[string]any{"type": "string"}
		}},
		{name: "a new method", change: func(doc map[string]any) {
			cr := top(doc)["ClientRequest"].(map[string]any)
			cr["oneOf"] = append(cr["oneOf"].([]any), map[string]any{"properties": map[string]any{"method": map[string]any{"enum": []any{"thread/teleport"}}}})
		}},
		{name: "a reworded description", change: func(doc map[string]any) {
			v2(doc)["TurnStatus"].(map[string]any)["description"] = "something else entirely"
		}},
		{name: "a turn status added", moves: true, change: func(doc map[string]any) {
			ts := v2(doc)["TurnStatus"].(map[string]any)
			ts["enum"] = append(ts["enum"].([]any), "paused")
		}},
		{name: "a field of a response", moves: true, change: func(doc map[string]any) {
			props := v2(doc)["ThreadStartResponse"].(map[string]any)["properties"].(map[string]any)
			delete(props, "model")
		}},
		{name: "a method removed", moves: true, change: func(doc map[string]any) {
			cr := top(doc)["ClientRequest"].(map[string]any)
			var kept []any
			for _, e := range cr["oneOf"].([]any) {
				if !strings.Contains(mustJSON(e), `"turn/steer"`) {
					kept = append(kept, e)
				}
			}
			cr["oneOf"] = kept
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sum, err := SchemaHash(edit(t, tc.change))
			if err != nil {
				t.Fatal(err)
			}
			if moved := sum != base; moved != tc.moves {
				t.Errorf("hash moved = %v, want %v", moved, tc.moves)
			}
		})
	}
	if _, err := SchemaHash([]byte(`{"title":"not a bundle"}`)); err == nil {
		t.Error("a document with no definitions hashed")
	}
}

// What doctor and the capability document see: nothing for a pinned
// protocol, a warning naming the version for drift, and one saying the check
// could not run for a codex that cannot generate a schema.
func TestSchemaWarning(t *testing.T) {
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	drifted := filepath.Join(t.TempDir(), "drifted.json")
	if err := os.WriteFile(drifted, edit(t, func(doc map[string]any) {
		ts := v2(doc)["TurnStatus"].(map[string]any)
		ts["enum"] = append(ts["enum"].([]any), "paused")
	}), 0o600); err != nil {
		t.Fatal(err)
	}
	pinnedFile, _ := filepath.Abs(filepath.Join(fixtures, schemaFile))
	for _, tc := range []struct {
		name, schema, version, want string
	}{
		{name: "pinned", schema: pinnedFile, version: "codex-cli 0.147.0"},
		{name: "a newer codex, same surface", schema: pinnedFile, version: "codex-cli 0.148.0"},
		{name: "drift", schema: drifted, version: "codex-cli 0.149.0", want: "differs from the one this yad was built against (codex 0.147.0)"},
		{name: "no schema at all", schema: "fail", version: "codex-cli 0.9.0", want: "could not check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_TEST_SCHEMA", tc.schema)
			got := SchemaWarning(context.Background(), os.Args[0], tc.version)
			if tc.want == "" && got != "" || !strings.Contains(got, tc.want) {
				t.Fatalf("warning = %q, want %q", got, tc.want)
			}
			if tc.want != "" && !strings.Contains(got, tc.version) && tc.schema != "fail" {
				t.Errorf("warning %q does not name the installed version", got)
			}
		})
	}
	// Asked again for the same binary and version, the answer is remembered:
	// the probe runs every sync.
	t.Setenv("CODEX_TEST_SCHEMA", "fail")
	if got := SchemaWarning(context.Background(), os.Args[0], "codex-cli 0.147.0"); got != "" {
		t.Errorf("a remembered answer was checked again: %q", got)
	}
}
