package hostool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixtures are what the real tools print, account name and all: half of
// these tests exist to prove that the name is in what gh says and never in what
// YAD keeps. Nothing here runs a real git, gh or docker, and nothing reaches
// the network.
const (
	ghVersion     = "gh version 2.98.0 (2026-08-20)\nhttps://github.com/cli/cli/releases/tag/v2.98.0\n"
	ghJSON        = `{"hosts":{"github.com":[{"state":"success","active":true,"host":"github.com","login":"octocat","tokenSource":"keyring","scopes":"gist, read:org, repo","gitProtocol":"ssh"}]}}`
	ghSignedOut   = "You are not logged into any GitHub hosts. To log in, run: gh auth login\n"
	ghNoJSONFlag  = "unknown flag: --json\n"
	dockerVersion = "Docker version 29.1.3, build f52814d\n"
	dockerNoDaemn = "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n"
)

const ghProse = `github.com
  ✓ Logged in to github.com account octocat (keyring)
  - Active account: true
  - Git operations protocol: ssh
  - Token: gho_************************************
  - Token scopes: 'gist', 'read:org', 'repo'
`

// answer is what a fake tool prints and how it exits for one probe.
type answer struct {
	out, err string
	code     int
}

// tool is a host tool made of canned answers — a shell script printing files
// the test wrote.
//
// A script rather than the test binary re-executed, which is how a fake harness
// is built elsewhere: these tests spawn two children each, and a
// race-instrumented Go binary takes about a second just to start.
type tool struct {
	version answer // `--version`
	asJSON  answer // the second probe asked with --json: gh's auth status
	plain   answer // the second probe asked without: an older gh, and docker's daemon
}

func install(t *testing.T, id string, f tool) {
	t.Helper()
	entry, ok := Lookup(id)
	if !ok {
		t.Fatalf("no %s in the catalog", id)
	}
	dir := t.TempDir()
	for name, a := range map[string]answer{"version": f.version, "json": f.asJSON, "plain": f.plain} {
		for suffix, body := range map[string]string{".out": a.out, ".err": a.err} {
			if err := os.WriteFile(filepath.Join(dir, name+suffix), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// PATH is emptied below, so everything the script runs is named in full.
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
"--version") a=%[1]s/version; c=%[2]d ;;
*--json*)    a=%[1]s/json;    c=%[3]d ;;
*)           a=%[1]s/plain;   c=%[4]d ;;
esac
/bin/cat "$a.out"
/bin/cat "$a.err" >&2
exit $c
`, dir, f.version.code, f.asJSON.code, f.plain.code)
	path := filepath.Join(dir, id)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	isolate(t, entry.EnvPath, path)
}

// isolate empties PATH and points every other host tool at nothing, so a test
// says the same thing on a machine with git, gh and docker installed as on one
// without — and never probes the real gh, whose auth probe reaches the network.
func isolate(t *testing.T, envPath, path string) {
	t.Helper()
	absent := filepath.Join(t.TempDir(), "absent")
	t.Setenv("PATH", t.TempDir())
	for _, other := range Catalog() {
		// Not "": an empty override falls back to PATH.
		t.Setenv(other.EnvPath, absent)
	}
	t.Setenv(envPath, path)
}

// probe runs detection and returns the one tool the test set up.
func probe(t *testing.T, id string) Detected {
	t.Helper()
	for _, d := range Detect(context.Background()) {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("%s missing from Detect", id)
	return Detected{}
}

// A machine with nothing installed still produces a full, ordered report:
// absence is a capability fact, not a failure.
func TestDetectReportsAbsentTools(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, entry := range Catalog() {
		t.Setenv(entry.EnvPath, "")
	}
	got := Detect(context.Background())
	if len(got) != len(Catalog()) {
		t.Fatalf("Detect returned %d entries, want %d", len(got), len(Catalog()))
	}
	for i, d := range got {
		if d.ID != Catalog()[i].ID {
			t.Errorf("entry %d is %q, want catalog order %q", i, d.ID, Catalog()[i].ID)
		}
		if d.Present || d.Error != "" || d.LoggedIn != nil {
			t.Errorf("%s with an empty PATH: %+v", d.ID, d)
		}
	}
}

// The catalog is the three tools DEV-31 settled on and no more: a fourth would
// cost every runner two spawns an interval and every hub a field to learn.
func TestCatalogIsTheThree(t *testing.T) {
	var ids []string
	for _, entry := range Catalog() {
		ids = append(ids, entry.ID)
	}
	if got := strings.Join(ids, ","); got != "git,gh,docker" {
		t.Errorf("catalog = %s, want git,gh,docker", got)
	}
}

// git has no login and no daemon: present, a version, and nothing else to say.
func TestGitIsVersionOnly(t *testing.T) {
	install(t, "git", tool{version: answer{out: "git version 2.51.0\n"}})
	d := probe(t, "git")
	if !d.Present || d.Version != "git version 2.51.0" || d.Error != "" {
		t.Errorf("git = %+v", d)
	}
	if d.LoggedIn != nil || d.LoginHost != "" {
		t.Errorf("git reported a login: %+v", d)
	}
}

// gh in each of the states a machine is really found in. The host and the
// boolean are the whole of the answer.
func TestGHLoginState(t *testing.T) {
	ghv := answer{out: ghVersion}
	noJSON := answer{err: ghNoJSONFlag, code: 1}
	for _, tc := range []struct {
		name     string
		gh       tool
		wantIn   bool
		wantHost string
	}{
		{"signed in", tool{ghv, answer{out: ghJSON}, answer{out: ghProse}}, true, "github.com"},
		{"signed out", tool{ghv, answer{out: `{"hosts":{}}`}, answer{err: ghSignedOut, code: 1}}, false, ""},
		{
			"the active host wins",
			tool{ghv, answer{out: `{"hosts":{"github.com":[{"state":"success","active":false,"login":"octocat"}],` +
				`"ghe.example.com":[{"state":"success","active":true,"login":"octocat"}]}}`}, answer{}},
			true, "ghe.example.com",
		},
		{
			// Without an active account the answer still has to be the same on
			// every probe: an unstable host would move the capability
			// fingerprint four times a minute for no change in the machine.
			"no active host, first by name",
			tool{ghv, answer{out: `{"hosts":{"github.com":[{"state":"success","active":false,"login":"octocat"}],` +
				`"acme.example.com":[{"state":"success","active":false,"login":"octocat"}]}}`}, answer{}},
			true, "acme.example.com",
		},
		{
			// gh exits non-zero here, and that is not what decides the answer.
			"a token gh cannot use is not a login",
			tool{ghv, answer{out: `{"hosts":{"github.com":[{"state":"failure","active":true,"login":"octocat"}]}}`, code: 1}, answer{}},
			false, "",
		},
		{"signed in, gh too old for --json", tool{ghv, noJSON, answer{out: ghProse}}, true, "github.com"},
		{"signed out, gh too old for --json", tool{ghv, noJSON, answer{err: ghSignedOut, code: 1}}, false, ""},
		{
			// The wording gh used before its accounts rework.
			"signed in, older wording",
			tool{ghv, noJSON, answer{out: "✓ Logged in to ghe.example.com as octocat (oauth_token)\n"}},
			true, "ghe.example.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			install(t, "gh", tc.gh)
			d := probe(t, "gh")
			if !d.Present || d.Error != "" {
				t.Fatalf("gh = %+v", d)
			}
			if d.LoggedIn == nil || *d.LoggedIn != tc.wantIn || d.LoginHost != tc.wantHost {
				t.Errorf("logged in %v host %q, want %v %q", d.LoggedIn, d.LoginHost, tc.wantIn, tc.wantHost)
			}
		})
	}
}

// A gh that says nothing at all is not a gh that is signed out: claiming so
// would route pull-request work away from a machine that may well do it.
func TestGHSilenceIsNotAnAnswer(t *testing.T) {
	install(t, "gh", tool{version: answer{out: ghVersion}, asJSON: answer{code: 1}, plain: answer{code: 1}})
	d := probe(t, "gh")
	if d.LoggedIn != nil {
		t.Errorf("LoggedIn = %v, want nothing claimed", *d.LoggedIn)
	}
	if !strings.Contains(d.Error, "gh auth status") {
		t.Errorf("Error = %q, want it to name the probe that said nothing", d.Error)
	}
}

// What gh prints names the account and shows a token, masked or not. Neither
// may reach a hub — CLAUDE.md, and the reason the fixtures carry both.
func TestGHReportCarriesNothingButTheHost(t *testing.T) {
	ghv := answer{out: ghVersion}
	noJSON := answer{err: ghNoJSONFlag, code: 1}
	for name, f := range map[string]tool{
		"signed in, json":   {ghv, answer{out: ghJSON}, answer{out: ghProse}},
		"signed in, prose":  {ghv, noJSON, answer{out: ghProse}},
		"signed out, json":  {ghv, answer{out: `{"hosts":{}}`}, answer{err: ghSignedOut, code: 1}},
		"signed out, prose": {ghv, noJSON, answer{err: ghSignedOut, code: 1}},
		"a token gh cannot use": {ghv,
			answer{out: `{"hosts":{"github.com":[{"state":"failure","active":true,"login":"octocat"}]}}`, code: 1}, answer{}},
		"nothing at all": {ghv, answer{code: 1}, answer{code: 1}},
	} {
		t.Run(name, func(t *testing.T) {
			install(t, "gh", f)
			raw, err := json.Marshal(probe(t, "gh"))
			if err != nil {
				t.Fatal(err)
			}
			for _, leak := range []string{"octocat", "gho_", "keyring", "scopes", "not logged into"} {
				if strings.Contains(string(raw), leak) {
					t.Errorf("the report carries %q: %s", leak, raw)
				}
			}
		})
	}
}

// Installed is not usable: a docker whose daemon is down runs a container
// exactly as well as no docker at all, so the daemon is what gets asked.
func TestDockerReportsTheDaemonNotTheBinary(t *testing.T) {
	t.Run("daemon up", func(t *testing.T) {
		install(t, "docker", tool{version: answer{out: dockerVersion}, plain: answer{out: "29.1.3\n"}})
		d := probe(t, "docker")
		if !d.Present || d.Version != "Docker version 29.1.3, build f52814d" || d.Error != "" {
			t.Errorf("docker = %+v", d)
		}
	})

	t.Run("daemon down", func(t *testing.T) {
		install(t, "docker", tool{version: answer{out: dockerVersion}, plain: answer{err: dockerNoDaemn, code: 1}})
		d := probe(t, "docker")
		if !d.Present || d.Version == "" {
			t.Errorf("a daemon that is down must not hide the client: %+v", d)
		}
		if !strings.Contains(d.Error, "not answering") {
			t.Errorf("Error = %q, want the daemon reported down", d.Error)
		}
		// Errors carry the next action (CLAUDE.md).
		if !strings.Contains(d.Error, "start ") {
			t.Errorf("Error = %q, want the way to start it", d.Error)
		}
		// The daemon's own message names its socket, and a DOCKER_HOST may
		// carry credentials in its URL; none of it travels.
		if strings.Contains(d.Error, "unix://") {
			t.Errorf("Error = %q, want no socket address", d.Error)
		}
	})

	// A docker whose configured host answers nothing prints nothing and exits
	// 0; an empty server version is still no daemon.
	t.Run("daemon silent", func(t *testing.T) {
		install(t, "docker", tool{version: answer{out: dockerVersion}, plain: answer{}})
		if d := probe(t, "docker"); !strings.Contains(d.Error, "not answering") {
			t.Errorf("Error = %q, want the daemon reported down", d.Error)
		}
	})
}

// hanging points a host tool at a script that never answers.
func hanging(t *testing.T, id, body string) Detected {
	t.Helper()
	entry, ok := Lookup(id)
	if !ok {
		t.Fatalf("no %s in the catalog", id)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, id)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	isolate(t, entry.EnvPath, path)
	return probe(t, id)
}

// The acceptance criterion, at the probe: a tool that never answers is reported
// within the timeout, whether it hangs on its version or on the second probe.
// That registration then still succeeds is the other half, in internal/runner.
func TestHangingProbeIsBoundedAndReported(t *testing.T) {
	// Answers the version and hangs on everything else — the shape that
	// matters, since the second probe is the one waiting on a network or a
	// daemon. PATH is empty, so sleep is named in full.
	answersVersion := func(banner string) string {
		return "case \"$1\" in\n  --version) echo '" + banner + "' ;;\n  *) /bin/sleep 60 ;;\nesac\n"
	}
	for _, tc := range []struct {
		name, id, body string
		wantErr        string
		wantVersion    string
	}{
		{"git hangs on --version", "git", "/bin/sleep 60\n", "no answer to git --version", ""},
		{"gh hangs on --version", "gh", "/bin/sleep 60\n", "no answer to gh --version", ""},
		{"gh hangs on auth status", "gh", answersVersion("gh version 2.98.0"), "gh auth status", "gh version 2.98.0"},
		{"docker hangs on the daemon", "docker", answersVersion("Docker version 29.1.3"), "did not answer", "Docker version 29.1.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := probeTimeout
			probeTimeout = 1500 * time.Millisecond
			t.Cleanup(func() { probeTimeout = old })

			start := time.Now()
			d := hanging(t, tc.id, tc.body)
			// Two probes at most, plus a moment to collect what each printed.
			if took := time.Since(start); took > 5*time.Second {
				t.Errorf("took %s; the timeout is not bounding the probe", took)
			}
			if !d.Present {
				t.Errorf("a tool that hangs is still installed: %+v", d)
			}
			if !strings.Contains(d.Error, tc.wantErr) {
				t.Errorf("Error = %q, want it to name %q", d.Error, tc.wantErr)
			}
			if d.Version != tc.wantVersion {
				t.Errorf("Version = %q, want %q", d.Version, tc.wantVersion)
			}
			// Nothing is claimed about a login the probe never got an answer on.
			if tc.id == "gh" && d.LoggedIn != nil {
				t.Errorf("LoggedIn = %v after a probe that never answered", *d.LoggedIn)
			}
		})
	}
}

// A binary that is there but will not run is reported broken, not dropped.
func TestBrokenBinaryIsReported(t *testing.T) {
	isolate(t, "YAD_GIT_PATH", t.TempDir()) // a directory is not an executable
	d := probe(t, "git")
	if !d.Present || d.Error == "" {
		t.Errorf("git = %+v, want it present and broken", d)
	}
}

func TestGHFromJSON(t *testing.T) {
	for _, tc := range []struct {
		name, raw      string
		wantHost       string
		wantIn, wantOK bool
	}{
		{"active account", ghJSON, "github.com", true, true},
		{"no hosts", `{"hosts":{}}`, "", false, true},
		{"null hosts", `{"hosts":null}`, "", false, false},
		{"not json", ghNoJSONFlag, "", false, false},
		{"empty", "", "", false, false},
		{"failure state only", `{"hosts":{"github.com":[{"state":"failure","active":true}]}}`, "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, in, ok := ghFromJSON([]byte(tc.raw))
			if host != tc.wantHost || in != tc.wantIn || ok != tc.wantOK {
				t.Errorf("ghFromJSON = %q, %v, %v; want %q, %v, %v", host, in, ok, tc.wantHost, tc.wantIn, tc.wantOK)
			}
		})
	}
}

func TestGHFromProse(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantHost  string
		wantIn    bool
	}{
		{"current wording", ghProse, "github.com", true},
		{"the wording before gh's accounts rework", "✓ Logged in to ghe.example.com as octocat (oauth_token)", "ghe.example.com", true},
		{"signed out", ghSignedOut, "", false},
		{"empty", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, in := ghFromProse(tc.raw)
			if host != tc.wantHost || in != tc.wantIn {
				t.Errorf("ghFromProse = %q, %v; want %q, %v", host, in, tc.wantHost, tc.wantIn)
			}
		})
	}
}
