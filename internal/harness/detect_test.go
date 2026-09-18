package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseVersion(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{"bare", "2.4.1\n", "2.4.1"},
		{"banner", "claude 2.4.1 (Claude Code)\n", "claude 2.4.1 (Claude Code)"},
		{"update notice below", "codex 0.58.0\n\nA new version is available\n", "codex 0.58.0"},
		{"leading blank line", "\n  opencode 1.2.3  \n", "opencode 1.2.3"},
		{"empty", "\n\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseVersion(tc.raw); got != tc.want {
				t.Errorf("ParseVersion(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// A machine with nothing installed must still produce a full, ordered report —
// absence is a capability fact, not a failure.
func TestDetectReportsAbsentHarnesses(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, h := range Catalog() {
		t.Setenv(h.EnvPath, "")
	}
	got := Detect(context.Background())
	if len(got) != len(Catalog()) {
		t.Fatalf("Detect returned %d entries, want %d", len(got), len(Catalog()))
	}
	for i, d := range got {
		if d.ID != Catalog()[i].ID {
			t.Errorf("entry %d is %q, want catalog order %q", i, d.ID, Catalog()[i].ID)
		}
		if d.Present || d.Ready() {
			t.Errorf("%s reported present with an empty PATH", d.ID)
		}
	}
}

// A present harness whose version probe fails is reported broken, not dropped,
// and is never Ready — a runner must not take work for it.
func TestDetectReportsBrokenHarness(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("YAD_CLAUDE_PATH", "")
	for _, d := range Detect(context.Background()) {
		if d.ID != "claude" {
			continue
		}
		if !d.Present || d.Error == "" || d.Ready() {
			t.Errorf("broken claude: present=%v error=%q ready=%v", d.Present, d.Error, d.Ready())
		}
		return
	}
	t.Fatal("claude missing from Detect")
}

func TestDetectReadsVersionAndHonoursEnvPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "anything")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho 'codex-cli 0.147.0'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("YAD_CODEX_PATH", bin)
	for _, d := range Detect(context.Background()) {
		if d.ID != "codex" {
			continue
		}
		if !d.Present || d.Error != "" || d.Version != "codex-cli 0.147.0" || d.Path != bin {
			t.Errorf("codex = %+v", d)
		}
		return
	}
	t.Fatal("codex missing from Detect")
}

func TestLookup(t *testing.T) {
	// Recognised until DEV-5 lands the adapter; nothing becomes first-class
	// without one (CLAUDE.md).
	if h, ok := Lookup("claude"); !ok || h.Kind != Recognised {
		t.Errorf("Lookup(claude) = %+v, %v", h, ok)
	}
	if _, ok := Lookup("nope"); ok {
		t.Error("Lookup(nope) found something")
	}
}

// A probe that hangs is reported, not waited on, and it takes its descendants
// with it: a launcher that forks and hangs would otherwise leave one orphan per
// daemon tick.
func TestHangingProbeIsBoundedAndLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "grandchild.pid")
	script := filepath.Join(dir, "codex")
	body := "#!/bin/sh\nsleep 60 &\necho $! > " + pidfile + "\nsleep 60\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YAD_CODEX_PATH", script)
	old := versionTimeout
	versionTimeout = 1500 * time.Millisecond
	t.Cleanup(func() { versionTimeout = old })

	h, _ := Lookup("codex")
	start := time.Now()
	d := detectOne(context.Background(), h)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("probe took %s; the timeout is not bounding it", took)
	}
	if !strings.Contains(d.Error, "no answer") {
		t.Errorf("Error = %q, want a timeout report", d.Error)
	}
	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("grandchild never started: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d survived the probe", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Ready is the gate on accepting a run, so every way to fail it is a case.
func TestReady(t *testing.T) {
	first := Harness{Kind: FirstClass}
	rec := Harness{Kind: Recognised}
	for _, tc := range []struct {
		name string
		d    Detected
		want bool
	}{
		{"first-class, present, answering", Detected{Harness: first, Present: true}, true},
		{"recognised, present, answering", Detected{Harness: rec, Present: true}, false},
		{"first-class, present, broken", Detected{Harness: first, Present: true, Error: "exit 1"}, false},
		{"first-class, absent", Detected{Harness: first}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.Ready(); got != tc.want {
				t.Errorf("Ready() = %v, want %v", got, tc.want)
			}
		})
	}
}

// DOMAIN.md's **First-class harness** entry names the closed set of kinds; the
// capability document publishes these strings, so the two must not drift.
func TestKindsMatchDomain(t *testing.T) {
	raw, err := os.ReadFile("../../DOMAIN.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(raw), "**First-class harness**")
	if !ok {
		t.Fatal("DOMAIN.md has no **First-class harness** entry")
	}
	_, kinds, ok := strings.Cut(after, "_Kinds_:")
	if !ok {
		t.Fatal("**First-class harness** has no _Kinds_ line")
	}
	line, _, _ := strings.Cut(kinds, "\n")
	var got []string
	for _, v := range strings.Split(line, "|") {
		got = append(got, strings.TrimSpace(v))
	}
	want := []string{string(FirstClass), string(Recognised)}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("DOMAIN.md kinds %v, catalog kinds %v", got, want)
	}
}

// The probe's outcome is the leader's, whoever else holds the pipe. Every
// leader fate against every kind of descendant.
func TestProbeOutcomeFollowsTheLeader(t *testing.T) {
	old := versionTimeout
	versionTimeout = 1500 * time.Millisecond
	t.Cleanup(func() { versionTimeout = old })
	// Each child records its pid only once it is in place, and the leader waits
	// for that: a leader that exited at once would have its group killed before
	// the child had left it, and the test would not be testing a detached child.
	detached := "perl -MPOSIX -e 'POSIX::setsid(); open(F, \">>\", $ENV{PIDS}); print F \"$$\\n\"; close F; sleep 60' &\n" +
		"while [ ! -s \"$PIDS\" ]; do sleep 0.02; done\n"
	inGroup := "sleep 60 &\necho $! >> \"$PIDS\"\n"
	for _, tc := range []struct {
		name, body  string
		wantVersion string
		wantErr     string
		within      time.Duration
	}{
		{"exits 0, alone", "echo 'codex-cli 1.0'\n", "codex-cli 1.0", "", time.Second},
		{"exits 0, in-group child holds stdout", inGroup + "echo 'codex-cli 1.0'\n", "codex-cli 1.0", "", time.Second},
		{"exits 0, detached child holds stdout", detached + "echo 'codex-cli 1.0'\n", "codex-cli 1.0", "", time.Second},
		{"exits 3", "echo oops >&2\nexit 3\n", "", "exit status 3", time.Second},
		{"exits 3, detached child holds stdout", detached + "exit 3\n", "", "exit status 3", time.Second},
		{"hangs", "sleep 60\n", "", "no answer", 3 * time.Second},
		{"hangs, detached child holds stdout", detached + "sleep 60\n", "", "no answer", 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pids := filepath.Join(dir, "pids")
			script := filepath.Join(dir, "codex")
			if err := os.WriteFile(script, []byte("#!/bin/sh\n"+tc.body), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PIDS", pids)
			t.Setenv("YAD_CODEX_PATH", script)
			t.Cleanup(func() {
				raw, _ := os.ReadFile(pids)
				for _, f := range strings.Fields(string(raw)) {
					if pid, _ := strconv.Atoi(f); pid > 0 {
						syscall.Kill(pid, syscall.SIGKILL)
					}
				}
			})
			h, _ := Lookup("codex")
			start := time.Now()
			d := detectOne(context.Background(), h)
			if took := time.Since(start); took > tc.within {
				t.Errorf("took %s, want under %s", took, tc.within)
			}
			if d.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", d.Version, tc.wantVersion)
			}
			if (tc.wantErr == "") != (d.Error == "") || !strings.Contains(d.Error, tc.wantErr) {
				t.Errorf("Error = %q, want %q", d.Error, tc.wantErr)
			}
		})
	}
}
