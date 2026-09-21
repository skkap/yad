package capability

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/hostool"
)

// outcome is what a hub learns about one catalog entry from its override, in
// the three fields both kinds share. Read back through JSON because those are
// the names the document carries.
type outcome struct {
	Present  bool     `json:"present"`
	Error    string   `json:"error"`
	Warnings []string `json:"warnings"`
}

// entry is one harness or one host tool, seen the same way.
type entry struct {
	kind, id, env, binary, command string
}

func everyEntry() []entry {
	var out []entry
	for _, h := range harness.Catalog() {
		out = append(out, entry{"harness", h.ID, h.EnvPath, h.Binary, h.Binary + " " + strings.Join(h.VersionArgs, " ")})
	}
	for _, t := range hostool.Catalog() {
		out = append(out, entry{"host tool", t.ID, t.EnvPath, t.Binary, t.Binary + " " + strings.Join(t.VersionArgs, " ")})
	}
	return out
}

// answering is a binary every probe of every entry is happy with: a version,
// a gh signed out of everything, a docker whose daemon answers.
const answering = `#!/bin/sh
case "$*" in
--version) echo 'tool 1.2.3' ;;
*auth*) echo '{"hosts":{}}' ;;
*) echo '29.1.3' ;;
esac
`

// One rule for what an override means, whatever it overrides (DEV-68). A hub
// reads harnesses and host tools out of the same document and cannot know the
// rule differs by kind, so for every harness and every host tool each case
// must come out the same — present, error and warnings — once the entry's own
// names are taken out of the words.
func TestAnOverrideMeansTheSameForHarnessesAndHostTools(t *testing.T) {
	for _, tc := range []struct {
		name string
		// override is what YAD_<ID>_PATH is set to, given the directory the
		// case writes into: "" leaves it unset.
		override func(t *testing.T, dir, binary string) string
		onPATH   bool
		// wantPresent, and whether an error or a warning comes with it; each
		// must name the variable and say what to do. fromPATH says the binary
		// reported is PATH's, the override's being dead.
		wantPresent, wantError, wantWarning, fromPATH bool
		wantAction                                    string
	}{
		{name: "override names nothing, PATH has it", onPATH: true,
			override:    func(t *testing.T, dir, binary string) string { return filepath.Join(dir, "gone", binary) },
			wantPresent: true, wantWarning: true, fromPATH: true, wantAction: "unset it"},
		{name: "override names nothing, PATH has none",
			override:  func(t *testing.T, dir, binary string) string { return filepath.Join(dir, "gone", binary) },
			wantError: true, wantAction: "unset it and install"},
		{name: "override is a directory, PATH has it", onPATH: true,
			override:    func(t *testing.T, dir, binary string) string { return mkdir(t, dir) },
			wantPresent: true, wantWarning: true, fromPATH: true, wantAction: "unset it"},
		{name: "override is a directory, PATH has none",
			override:  func(t *testing.T, dir, binary string) string { return mkdir(t, dir) },
			wantError: true, wantAction: "unset it and install"},
		// Installed and broken: the file is there, which is not nothing, and
		// PATH is not consulted — the owner named this one.
		{name: "override names a file that is not executable", onPATH: true,
			override: func(t *testing.T, dir, binary string) string {
				return write(t, filepath.Join(dir, "own", binary), answering, 0o644)
			},
			wantPresent: true, wantError: true, wantAction: "point it at an executable"},
		{name: "override names a binary that will not start", onPATH: true,
			override: func(t *testing.T, dir, binary string) string {
				return write(t, filepath.Join(dir, "own", binary), "#!/nonexistent/interpreter\n", 0o755)
			},
			wantPresent: true, wantError: true, wantAction: "point it at an executable"},
		{name: "no override, PATH has it", onPATH: true, wantPresent: true, fromPATH: true},
		{name: "no override, PATH has none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noTools(t)
			dir := t.TempDir()
			pathDir := filepath.Join(dir, "path")
			if err := os.MkdirAll(pathDir, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", pathDir)
			entries := everyEntry()
			for _, e := range entries {
				if tc.onPATH {
					write(t, filepath.Join(pathDir, e.binary), answering, 0o755)
				}
				if tc.override != nil {
					t.Setenv(e.env, tc.override(t, filepath.Join(dir, e.id), e.binary))
				}
			}

			got := map[string]outcome{}
			paths := map[string]string{}
			for _, d := range harness.Detect(context.Background()) {
				got["harness "+d.ID], paths["harness "+d.ID] = read(t, d), d.Path
			}
			for _, d := range hostool.Detect(context.Background()) {
				got["host tool "+d.ID], paths["host tool "+d.ID] = read(t, d), d.Path
			}

			var first string
			var shape outcome
			for i, e := range entries {
				key := e.kind + " " + e.id
				o := got[key]
				if o.Present != tc.wantPresent || (o.Error != "") != tc.wantError || (len(o.Warnings) > 0) != tc.wantWarning {
					t.Errorf("%s = %+v, want present %v, an error %v, a warning %v", key, o, tc.wantPresent, tc.wantError, tc.wantWarning)
				}
				words := strings.Join(append([]string{o.Error}, o.Warnings...), "\n")
				if tc.wantError || tc.wantWarning {
					if !strings.Contains(words, e.env) || !strings.Contains(words, tc.wantAction) {
						t.Errorf("%s says %q, want it to name %s and %q", key, words, e.env, tc.wantAction)
					}
				}
				// The variable's name is safe to print where its value is not.
				if strings.Contains(words, dir) {
					t.Errorf("%s names a path on the machine: %q", key, words)
				}
				if tc.fromPATH && paths[key] != filepath.Join(pathDir, e.binary) {
					t.Errorf("%s runs %q, want PATH's", key, paths[key])
				}
				// Where a run starts it from is where detection found it.
				if tc.fromPATH && e.kind == "harness" {
					if bin, ok := harness.Locate(e.id); !ok || bin != filepath.Join(pathDir, e.binary) {
						t.Errorf("harness.Locate(%s) = %q, %v; want PATH's", e.id, bin, ok)
					}
				}
				norm := normalise(o, e)
				if i == 0 {
					first, shape = key, norm
					continue
				}
				if norm.Present != shape.Present || norm.Error != shape.Error || !slices.Equal(norm.Warnings, shape.Warnings) {
					t.Errorf("%s and %s disagree:\n  %+v\n  %+v", first, key, shape, norm)
				}
			}
		})
	}
}

func write(t *testing.T, path, body string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func read(t *testing.T, d any) outcome {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var o outcome
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	return o
}

// normalise takes an entry's own names out of what it says, so two entries
// saying the same thing about themselves compare equal.
func normalise(o outcome, e entry) outcome {
	swap := func(s string) string {
		s = strings.ReplaceAll(s, e.env, "<VAR>")
		s = strings.ReplaceAll(s, e.command, "<COMMAND>")
		return strings.ReplaceAll(s, e.binary, "<BINARY>")
	}
	o.Error = swap(o.Error)
	var ws []string
	for _, w := range o.Warnings {
		ws = append(ws, swap(w))
	}
	o.Warnings = ws
	return o
}
