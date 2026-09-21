package capability

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/harness"
	"github.com/skkap/yad/internal/hostool"
)

// noTools puts every probe out of reach: an empty PATH is not enough on its
// own, because a path override left in the environment would have Build spawn
// the machine's real docker, and reach its daemon.
func noTools(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	for _, h := range harness.Catalog() {
		t.Setenv(h.EnvPath, "")
	}
	for _, tool := range hostool.Catalog() {
		t.Setenv(tool.EnvPath, "")
	}
}

func TestFingerprintIgnoresTimeOnly(t *testing.T) {
	noTools(t)
	cfg := config.Default()
	a := Build(context.Background(), "r1", cfg, nil)
	b := a
	b.ObservedAt = a.ObservedAt.Add(time.Hour)
	if Fingerprint(a) != Fingerprint(b) {
		t.Error("a timestamp change moved the fingerprint")
	}
	b.Labels = []string{"gpu"}
	if Fingerprint(a) == Fingerprint(b) {
		t.Error("a label change did not move the fingerprint")
	}
}

// Label order is the owner's typing order, not a change in the machine.
func TestLabelOrderDoesNotMoveFingerprint(t *testing.T) {
	noTools(t)
	cfg := config.Default()
	cfg.Labels = []string{"b", "a"}
	x := Build(context.Background(), "r", cfg, nil)
	cfg.Labels = []string{"a", "b"}
	if Fingerprint(x) != Fingerprint(Build(context.Background(), "r", cfg, nil)) {
		t.Error("label order moved the fingerprint")
	}
}

// The document is public: it must carry account labels and never the binary
// path lookup internals or anything credential-shaped.
func TestDocumentIsPublicSafe(t *testing.T) {
	cfg := config.Default()
	cfg.Harness = map[string]config.HarnessConfig{"claude": {Accounts: []string{"personal"}, Cap: 2}}
	found := []harness.Detected{{Harness: harness.Catalog()[0], Present: true, Path: "/x/claude", Version: "2.1"}}
	reps := Harnesses(found, cfg, nil)
	b, _ := json.Marshal(reps)
	s := string(b)
	if !strings.Contains(s, `"label":"personal"`) {
		t.Errorf("account label missing: %s", s)
	}
	for _, leak := range []string{"YAD_CLAUDE_PATH", "/x/claude", "--version"} {
		if strings.Contains(s, leak) {
			t.Errorf("document leaks %q: %s", leak, s)
		}
	}
}

// The host-tool half of the document is public too: it carries what a hub
// routes on and nothing about the machine's own arrangements — never a path,
// and never the account a tool is logged in as (CLAUDE.md).
func TestHostToolReportIsPublicSafe(t *testing.T) {
	in, out := true, false
	reps := HostTools([]hostool.Detected{
		{ID: "git", Path: "/opt/homebrew/bin/git", Present: true, Version: "git version 2.51.0"},
		{ID: "gh", Path: "/opt/homebrew/bin/gh", Present: true, Version: "gh 2.98.0", LoggedIn: &in, LoginHosts: []string{"ghe.example.com", "github.com"}},
		{ID: "docker", Path: "/usr/local/bin/docker", Present: true, Version: "Docker 29.1.3", Error: "the Docker daemon is not answering — start it"},
		{ID: "podman", Present: false, LoggedIn: &out},
	})
	b, _ := json.Marshal(reps)
	s := string(b)
	// Every field the mapping copies, so dropping one from it cannot stay green:
	// a hub told nothing about a git's version cannot tell one too old for
	// --filter from a current one.
	for _, want := range []string{
		`"id":"git"`, `"present":true`, `"version":"git version 2.51.0"`,
		`"login_hosts":["ghe.example.com","github.com"]`, `"logged_in":true`,
		`"error":"the Docker daemon`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s: %s", want, s)
		}
	}
	for _, leak := range []string{"/opt/homebrew", "/usr/local", `"path"`} {
		if strings.Contains(s, leak) {
			t.Errorf("the report leaks %q: %s", leak, s)
		}
	}
}

// Every host tool in the catalog reaches the document, present or not: a hub
// routing on docker has to be able to tell "no docker here" from "this runner
// is too old to know the question".
func TestBuildReportsEveryHostTool(t *testing.T) {
	noTools(t)
	doc := Build(context.Background(), "r1", config.Default(), nil)
	if len(doc.HostTools) != len(hostool.Catalog()) {
		t.Fatalf("document carries %d host tools, want %d", len(doc.HostTools), len(hostool.Catalog()))
	}
	for i, tool := range doc.HostTools {
		if tool.ID != hostool.Catalog()[i].ID || tool.Present {
			t.Errorf("host tool %d = %+v, want %s absent", i, tool, hostool.Catalog()[i].ID)
		}
	}
}

// The guard on the document as a whole, because that is what every connected
// hub reads. DEV-60 kept child output out of a harness's Error and DEV-67
// found the same leak in its Warnings: sweeping by field misses a sibling
// field, so this sweeps by destination. Every binary the runner probes lives
// under a home named like an owner's, and every one fails printing a
// credential and that home; a Codex answers its version and then fails the
// protocol check the same way. Whatever field a future probe adds, none of it
// may reach the marshalled document.
func TestDocumentCarriesNoChildOutputOrHomePath(t *testing.T) {
	const secret = "https://user:hunter2@proxy.internal/"
	realHome, _ := os.UserHomeDir()
	home := filepath.Join(t.TempDir(), "Users", "someone")
	bin := filepath.Join(home, "bin")
	tmp := filepath.Join(home, "tmp")
	for _, d := range []string{bin, tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	noTools(t)
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", tmp)
	t.Setenv("PATH", bin)
	fail := "echo \"dyld: Library not loaded: $HOME/lib/libnode.dylib\" >&2\necho 'fatal: unable to access " + secret + "' >&2\nexit 2\n"
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range harness.Catalog() {
		write(h.Binary, fail)
	}
	for _, tool := range hostool.Catalog() {
		write(tool.Binary, fail)
	}
	// Past its version probe, so the protocol check runs and fails.
	write("codex", "if [ \"$1\" = --version ]; then echo 'codex-cli 0.147.0'; exit 0; fi\n"+fail)

	cfg := config.Default()
	cfg.Name = "r1"
	doc := Build(context.Background(), "r1", cfg, nil)
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	// Not vacuous: the failures are reported, in the runner's words.
	if !strings.Contains(s, `"warnings":["yad could not check this codex`) || !strings.Contains(s, `"error":"`) {
		t.Fatalf("the failures are not in the document: %s", s)
	}
	leaks := []string{secret, "hunter2", "dyld", "fatal:", home, bin, tmp, "/Users/", "fork/exec",
		"permission denied", "exit status", "no such file"}
	if realHome != "" && realHome != "/" {
		leaks = append(leaks, realHome)
	}
	for _, leak := range leaks {
		if strings.Contains(s, leak) {
			t.Errorf("the document carries %q: %s", leak, s)
		}
	}
}
