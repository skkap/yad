package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
			// The version is the first line codex printed; the report's
			// version field carries it, and a warning quotes nothing a child
			// wrote.
			if tc.want != "" && strings.Contains(got, tc.version) {
				t.Errorf("warning %q repeats what codex printed for its version", got)
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

// A codex whose detached descendant holds stdout open is answered when codex
// itself exits: the check runs on the daemon's capability tick, and a read
// that waited for EOF would stop that loop for good.
func TestSchemaCheckDoesNotWaitForAHeldPipe(t *testing.T) {
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	pinnedFile, _ := filepath.Abs(filepath.Join(fixtures, schemaFile))
	t.Setenv("CODEX_TEST_SCHEMA", pinnedFile)
	t.Setenv("CODEX_TEST_SCHEMA_DETACH", "1")
	done := make(chan string, 1)
	go func() { done <- SchemaWarning(context.Background(), os.Args[0], "codex-cli detached") }()
	select {
	case w := <-done:
		if w != "" {
			t.Errorf("warning = %q", w)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the check waited on a pipe a detached process holds")
	}
}

// A FIFO where codex should have written its bundle is refused, not read: an
// open of one blocks until a writer comes, and this runs on the daemon's
// capability tick.
func TestSchemaCheckDoesNotWaitOnAFIFO(t *testing.T) {
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	t.Setenv("CODEX_TEST_SCHEMA", "fifo")
	done := make(chan string, 1)
	go func() { done <- SchemaWarning(context.Background(), os.Args[0], "codex-cli fifo") }()
	select {
	case w := <-done:
		if !strings.Contains(w, "could not check") {
			t.Errorf("warning = %q, want the check reported as not made", w)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the check blocked opening a FIFO in the bundle's place")
	}
}

// A check that could not run is believed only for schemaRetry, so a transient
// failure does not follow a correctly pinned codex around for the daemon's
// life; one cut short by its caller is not remembered at all.
func TestFailedSchemaCheckIsRetried(t *testing.T) {
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	clock := time.Now()
	defer func(f func() time.Time) { now = f }(now)
	now = func() time.Time { return clock }
	const version = "codex-cli retry"

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Setenv("CODEX_TEST_SCHEMA", "fail")
	if w := SchemaWarning(ctx, os.Args[0], version); w != "" {
		t.Errorf("a cancelled check warned %q", w)
	}
	if w := SchemaWarning(context.Background(), os.Args[0], version); !strings.Contains(w, "could not check") {
		t.Fatalf("warning = %q: the cancelled check was remembered", w)
	}
	pinnedFile, _ := filepath.Abs(filepath.Join(fixtures, schemaFile))
	t.Setenv("CODEX_TEST_SCHEMA", pinnedFile)
	if w := SchemaWarning(context.Background(), os.Args[0], version); !strings.Contains(w, "could not check") {
		t.Errorf("warning = %q: a failure is believed for schemaRetry", w)
	}
	clock = clock.Add(schemaRetry + time.Second)
	if w := SchemaWarning(context.Background(), os.Args[0], version); w != "" {
		t.Errorf("warning = %q after schemaRetry, when codex answers", w)
	}
	// A definite answer holds: no timer undoes it.
	t.Setenv("CODEX_TEST_SCHEMA", "fail")
	clock = clock.Add(100 * schemaRetry)
	if w := SchemaWarning(context.Background(), os.Args[0], version); w != "" {
		t.Errorf("warning = %q: a definite answer was asked again", w)
	}
}

// The warning reaches the capability document, which every connected hub
// reads, so however the check fails it says so in the runner's words: never
// what codex printed, never the path it was started from, never the temp
// directory it wrote into (DEV-67). Each case fails the check a different way,
// and the leaks list what the raw error would have carried.
func TestSchemaWarningQuotesNothingItWasTold(t *testing.T) {
	const secret = "https://user:hunter2@proxy.internal/"
	realHome, _ := os.UserHomeDir()
	for _, tc := range []struct {
		name string
		// script is the fake codex's body; "" leaves the file unexecutable.
		script string
		// noTemp points TMPDIR at a directory that is not there.
		noTemp bool
		want   string
		// action is where the warning must send the owner; empty is the
		// command run by hand.
		action string
	}{
		{name: "it will not start", want: "it would not start"},
		{name: "it fails, saying what it should not", want: "it exited with an error",
			script: "echo \"dyld: Library not loaded: $HOME/lib/libnode.dylib\" >&2\necho 'fatal: unable to access " + secret + "' >&2\nexit 2"},
		{name: "what it wrote cannot be read", want: "what it wrote could not be read",
			script: "mkdir \"$4/" + schemaFile + "\""},
		{name: "what it wrote is not a schema", want: "is not one yad can read",
			script: "echo '" + secret + "' > \"$4/" + schemaFile + "\""},
		{name: "the temp directory cannot be made", noTemp: true, want: "the temp directory", action: "TMPDIR in the runner's environment",
			script: "exit 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "Users", "someone")
			bin := filepath.Join(home, "bin", "codex")
			tmp := filepath.Join(home, "tmp")
			for _, d := range []string{filepath.Dir(bin), tmp} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("HOME", home)
			t.Setenv("TMPDIR", tmp)
			if tc.noTemp {
				t.Setenv("TMPDIR", filepath.Join(home, "gone"))
			}
			body, mode := "#!/bin/sh\n"+tc.script+"\n", os.FileMode(0o755)
			if tc.script == "" {
				mode = 0o644
			}
			if err := os.WriteFile(bin, []byte(body), mode); err != nil {
				t.Fatal(err)
			}

			got, _, err := schemaWarning(context.Background(), bin)
			if err != nil || !strings.Contains(got, tc.want) {
				t.Fatalf("warning = %q, %v; want %q", got, err, tc.want)
			}
			action := tc.action
			if action == "" {
				action = "generate-json-schema --out DIR` on this machine"
			}
			if !strings.Contains(got, action) {
				t.Errorf("warning = %q, want the next action %q", got, action)
			}
			leaks := []string{secret, "hunter2", "dyld", "fatal:", home, "/Users/", "fork/exec",
				"permission denied", "exit status", "is a directory", "no such file", "invalid character"}
			if realHome != "" && realHome != "/" {
				leaks = append(leaks, realHome)
			}
			for _, leak := range leaks {
				if strings.Contains(got, leak) {
					t.Errorf("warning carries %q: %q", leak, got)
				}
			}
		})
	}
}
