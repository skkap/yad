package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/hostool"
)

// shortDir is a private temporary directory short enough to hold the
// control socket: t.TempDir() under macOS's $TMPDIR, with a long test name,
// passes the 103-byte limit on its own.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "yad")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// noHostTools puts the host-tool probes out of reach. An empty PATH is not the
// whole of it (ARCHITECTURE.md §7): a path override is consulted first, so a
// developer with YAD_GH_PATH exported would have `yad harnesses` and every
// daemon tick probe the real gh — and `gh auth status` is an authenticated
// request to github.com with that owner's token. Harness overrides are left
// alone here: a test that wants a fake claude sets one and then calls a helper
// that runs the command straight away.
func noHostTools(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	for _, tool := range hostool.Catalog() {
		t.Setenv(tool.EnvPath, "")
	}
}

// noTools also clears the harness overrides, for the helpers that only set a
// profile up: whatever they run comes later, and sets its own.
func noTools(t *testing.T) {
	t.Helper()
	noHostTools(t)
	for _, h := range harness.Catalog() {
		t.Setenv(h.EnvPath, "")
	}
}

func yad(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("YAD_CONFIG_DIR", t.TempDir())
	t.Setenv("YAD_DATA_DIR", shortDir(t))
	noHostTools(t)
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestHarnessesPrintsTheCapabilityDocument(t *testing.T) {
	code, out, errs := yad(t, "harnesses")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var doc v1.Capabilities
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not a capability document: %v\n%s", err, out)
	}
	if doc.RunnerID == "" || len(doc.Harnesses) == 0 || doc.Capacity.Total < 1 {
		t.Errorf("document = %+v", doc)
	}
}

// A command that is not built yet must say which epic brings it.
func TestUnbuiltCommandsNameTheirEpic(t *testing.T) {
	for cmd := range arrivesIn {
		code, _, errs := yad(t, cmd)
		if code != 1 || !strings.Contains(errs, "arrives in epic E") {
			t.Errorf("yad %s: exit %d, %q", cmd, code, errs)
		}
	}
}

func TestProfileIsValidated(t *testing.T) {
	if code, _, _ := yad(t, "--profile", "../x", "version"); code != 2 {
		t.Errorf("a path-shaped profile exited %d", code)
	}
}

func TestDoctorRunsOnAnEmptyMachine(t *testing.T) {
	code, out, _ := yad(t, "doctor")
	if code != 0 || !strings.Contains(out, "No drivable harness") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}

// time.NewTicker panics on a non-positive duration; the flag must be refused
// with a next action instead of a stack trace.
func TestDaemonRefusesNonPositiveInterval(t *testing.T) {
	for _, v := range []string{"0", "-1s"} {
		code, _, errs := yad(t, "daemon", "start", "--foreground", "--interval", v)
		if code == 0 || !strings.Contains(errs, "--interval must be positive") {
			t.Errorf("--interval %s: exit %d, %q", v, code, errs)
		}
	}
}

// Installed but without an adapter is a different fact from not installed, and
// needs a different next action.
func TestDoctorSaysWhyNothingIsDrivable(t *testing.T) {
	code, out, _ := yad(t, "doctor") // yad() empties PATH
	if code != 0 || !strings.Contains(out, "Install Claude Code or Codex") {
		t.Errorf("empty machine: exit %d:\n%s", code, out)
	}
	dir := t.TempDir()
	gemini := dir + "/gemini"
	if err := os.WriteFile(gemini, []byte("#!/bin/sh\necho '0.9.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAD_GEMINI_PATH", gemini)
	var o, e bytes.Buffer
	run(context.Background(), []string{"doctor"}, &o, &e)
	if !strings.Contains(o.String(), "no adapter in this yad yet") || !strings.Contains(o.String(), "install Claude Code or Codex") {
		t.Errorf("gemini installed, no adapter:\n%s", o.String())
	}

	bin := dir + "/claude"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho '2.1.276 (Claude Code)'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAD_CLAUDE_PATH", bin)
	o.Reset()
	run(context.Background(), []string{"doctor"}, &o, &e)
	if !strings.Contains(o.String(), "1 harness(es) this runner can be given work for") {
		t.Errorf("claude installed, with its adapter:\n%s", o.String())
	}
	if !regexp.MustCompile(`Claude Code +ready`).MatchString(o.String()) {
		t.Errorf("claude not reported ready:\n%s", o.String())
	}

	// A broken Claude beside a recognised Gemini: the fix is the probe
	// error, not an install.
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	o.Reset()
	run(context.Background(), []string{"doctor"}, &o, &e)
	if !strings.Contains(o.String(), "failed its version probe") || strings.Contains(o.String(), "install Claude Code") {
		t.Errorf("claude broken, gemini present:\n%s", o.String())
	}

	os.Remove(gemini)
	o.Reset()
	run(context.Background(), []string{"doctor"}, &o, &e)
	if !strings.Contains(o.String(), "failed its version probe") {
		t.Errorf("claude broken:\n%s", o.String())
	}
}

// Codex is first-class: an installed one is ready, and one whose app-server
// protocol is not the pinned one is still ready, with the drift said as a
// warning in doctor and in the capability document (decision 0037).
func TestDoctorReportsCodexProtocolDrift(t *testing.T) {
	codex := fakeCodexBin(t)
	t.Setenv("YAD_CODEX_PATH", codex)
	schema, err := filepath.Abs(codexSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_TEST_SCHEMA", schema)
	code, out, errs := yad(t, "doctor")
	if code != 0 || !regexp.MustCompile(`Codex +ready +codex-cli 0.147.0`).MatchString(out) || strings.Contains(out, "warning:") {
		t.Fatalf("pinned codex: exit %d:\n%s%s", code, out, errs)
	}

	drifted := filepath.Join(t.TempDir(), "drifted.json")
	b, err := os.ReadFile(schema)
	if err != nil {
		t.Fatal(err)
	}
	// Any change to a status a turn can end in is drift.
	b = bytes.Replace(b, []byte(`"interrupted",`), []byte(`"interrupted","paused",`), 1)
	if err := os.WriteFile(drifted, b, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_TEST_SCHEMA", drifted)
	// Another path, so the check the first doctor remembered is not reused.
	t.Setenv("YAD_CODEX_PATH", fakeCodexBin(t))
	code, out, errs = yad(t, "doctor")
	if code != 0 || !regexp.MustCompile(`Codex +ready`).MatchString(out) || !strings.Contains(out, "warning: Codex — the app-server protocol of codex-cli 0.147.0 differs") {
		t.Fatalf("drifted codex: exit %d:\n%s%s", code, out, errs)
	}
	code, out, errs = yad(t, "harnesses")
	if code != 0 || !strings.Contains(out, `"warnings": [`) || !strings.Contains(out, "differs from the one this yad was built against") {
		t.Fatalf("harnesses: exit %d:\n%s%s", code, out, errs)
	}
}

// `yad sessions` on a profile whose runner never ran lists nothing and
// creates nothing; closing one says there is nothing to close, or that the
// daemon is the one to do it.
func TestSessionsOnAFreshProfile(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		out  string
		errs string
	}{
		{[]string{"sessions"}, 0, "no sessions", ""},
		{[]string{"sessions", "--json"}, 0, "[]", ""},
		{[]string{"sessions", "close", "s1"}, 1, "", "no session"},
		{[]string{"sessions", "close"}, 1, "", "usage"},
		{[]string{"sessions", "close", "--connection", "home", "s1"}, 1, "", "yad daemon start"},
		{[]string{"sessions", "s1"}, 1, "", "unexpected"},
	} {
		code, out, errs := yad(t, tc.args...)
		if code != tc.code || !strings.Contains(out, tc.out) || !strings.Contains(errs, tc.errs) {
			t.Errorf("yad %v: exit %d, %q, %q", tc.args, code, out, errs)
		}
		if _, err := os.Stat(os.Getenv("YAD_DATA_DIR") + "/state.db"); !os.IsNotExist(err) {
			t.Errorf("yad %v created the state database", tc.args)
		}
	}
}

// The table's error column is cut to fit, so the message that carries the next
// action (DEV-60) is printed in full underneath — the footer tells the owner to
// fix the errors above, and half a sentence is not a fix. The cut itself lands
// on a rune boundary: both messages carry an em dash, and for `claude` the old
// byte slice put it across byte 39, printing a replacement character in the
// first diagnostic anyone runs on a new machine.
func TestDoctorPrintsTheWholeHarnessError(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		// viaPATH finds the binary the other way, which is the message whose
		// em dash lands on the old slice's boundary.
		viaPATH bool
	}{
		{name: "a harness that exits non-zero",
			body: "#!/bin/sh\necho 'fatal: unable to access https://user:hunter2@proxy.internal/' >&2\nexit 128\n",
			want: "error: Claude Code — `claude --version` exited with an error — run it on this machine to see why"},
		{name: "a harness on PATH that will not start", viaPATH: true,
			body: "#!/nonexistent/interpreter\n",
			want: "error: Claude Code — the claude on PATH will not start — run `claude --version` on this machine to see what stops it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := dir + "/claude"
			if err := os.WriteFile(bin, []byte(tc.body), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("YAD_CONFIG_DIR", t.TempDir())
			t.Setenv("YAD_DATA_DIR", shortDir(t))
			noHostTools(t) // and empties PATH
			if tc.viaPATH {
				t.Setenv("YAD_CLAUDE_PATH", "")
				t.Setenv("PATH", dir)
			} else {
				t.Setenv("YAD_CLAUDE_PATH", bin)
			}
			var o, e bytes.Buffer
			if code := run(context.Background(), []string{"doctor"}, &o, &e); code != 0 {
				t.Fatalf("exit %d: %s", code, e.String())
			}
			out := o.String()
			if !strings.Contains(out, tc.want) {
				t.Errorf("doctor did not print the error in full, want %q:\n%s", tc.want, out)
			}
			if !utf8.ValidString(out) || strings.ContainsRune(out, utf8.RuneError) {
				t.Errorf("doctor printed a broken rune:\n%q", out)
			}
			// What the harness printed, and where it lives, stay on the machine
			// here too.
			for _, leak := range []string{"hunter2", "proxy.internal", "fatal:", "fork/exec"} {
				if strings.Contains(out, leak) {
					t.Errorf("doctor printed %q:\n%s", leak, out)
				}
			}
		})
	}
}

// truncate's cut, pinned here rather than through a message: what doctor prints
// is worded for its reader and has already moved once, so a case that happens
// to straddle the boundary today would stop testing anything the next time
// someone rewrites a sentence. A byte slice at the cut printed a replacement
// character in the first diagnostic anyone runs on a new machine.
func TestTruncateCutsOnARuneBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, s, want string
		n             int
	}{
		{name: "shorter than the cap", s: "claude 2.1.0", n: 40, want: "claude 2.1.0"},
		{name: "exactly the cap", s: strings.Repeat("a", 40), n: 40, want: strings.Repeat("a", 40)},
		// The em dash occupies bytes 10-12, so a cut at n-1 = 11 lands inside
		// it — which is how the harness messages reached this function.
		{name: "a rune across the cut", s: "installed — and broken", n: 12, want: "installed "},
		{name: "the cut inside the first of many", s: "ab—cdefgh", n: 5, want: "ab"},
		{name: "multibyte throughout", s: "日本語のバージョン", n: 8, want: "日本"},
		// n-1 already starts a rune, so nothing is backed off.
		{name: "the cut on a boundary", s: "ab—cdefgh", n: 6, want: "ab—"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.s, tc.n)
			want := tc.want
			if len(tc.s) > tc.n {
				want += "…"
			}
			if got != want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tc.s, tc.n, got, want)
			}
			if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("truncate(%q, %d) = %q, which is not valid UTF-8", tc.s, tc.n, got)
			}
		})
	}
}
