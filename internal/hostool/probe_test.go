package hostool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	isolate(t, entry.EnvPath, writeTool(t, id, f))
}

// swap re-points a tool at new answers without forgetting what gh already
// said, which is what the next probe on a running runner looks like.
func swap(t *testing.T, id string, f tool) {
	t.Helper()
	entry, ok := Lookup(id)
	if !ok {
		t.Fatalf("no %s in the catalog", id)
	}
	t.Setenv(entry.EnvPath, writeTool(t, id, f))
}

func writeTool(t *testing.T, id string, f tool) string {
	t.Helper()
	dir := t.TempDir()
	for name, a := range map[string]answer{"version": f.version, "json": f.asJSON, "plain": f.plain} {
		for suffix, body := range map[string]string{".out": a.out, ".err": a.err} {
			if err := os.WriteFile(filepath.Join(dir, name+suffix), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// PATH is emptied by isolate, so everything the script runs is named in full.
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
	return path
}

// isolate empties PATH and points every other host tool at nothing, so a test
// says the same thing on a machine with git, gh and docker installed as on one
// without — and never probes the real gh, whose auth probe reaches the network.
func isolate(t *testing.T, envPath, path string) {
	t.Helper()
	forgetGHLogin()
	t.Cleanup(forgetGHLogin)
	absent := filepath.Join(t.TempDir(), "absent")
	t.Setenv("PATH", t.TempDir())
	for _, other := range Catalog() {
		// Not "": an empty override falls back to PATH.
		t.Setenv(other.EnvPath, absent)
	}
	t.Setenv(envPath, path)
}

// forgetGHLogin drops what the last probe of gh left behind, so one test's gh
// is never answered from another's. ageGHLogin backdates it, which is how a
// test reaches the staleness limits without waiting for them.
func forgetGHLogin() {
	remembered.Lock()
	defer remembered.Unlock()
	remembered.ghMemory = ghMemory{}
}

func ageGHLogin(by time.Duration) {
	remembered.Lock()
	defer remembered.Unlock()
	for _, t := range []*time.Time{&remembered.asked, &remembered.answered} {
		if !t.IsZero() {
			*t = t.Add(-by)
		}
	}
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
	if !d.Present || d.Version != "2.51.0" || d.Error != "" {
		t.Errorf("git = %+v", d)
	}
	if d.LoggedIn != nil || d.LoginHosts != nil {
		t.Errorf("git reported a login: %+v", d)
	}
}

// gh in each of the states a machine is really found in. The hosts and the
// boolean are the whole of the answer.
func TestGHLoginState(t *testing.T) {
	ghv := answer{out: ghVersion}
	noJSON := answer{err: ghNoJSONFlag, code: 1}
	for _, tc := range []struct {
		name      string
		gh        tool
		wantIn    bool
		wantHosts []string
	}{
		{"signed in", tool{ghv, answer{out: ghJSON}, answer{out: ghProse}}, true, []string{"github.com"}},
		{"signed out", tool{ghv, answer{out: `{"hosts":{}}`}, answer{err: ghSignedOut, code: 1}}, false, nil},
		{
			// `gh auth status --active` gives one entry per host, not one
			// entry overall, so a developer with a work GHE account and a
			// personal github.com one has both — and reporting either alone
			// would route the other's work away from a machine that can do it.
			"signed in to two hosts",
			tool{ghv, answer{out: `{"hosts":{"github.com":[{"state":"success","active":true,"login":"octocat"}],` +
				`"ghe.example.com":[{"state":"success","active":true,"login":"octocat"}]}}`}, answer{}},
			true, []string{"ghe.example.com", "github.com"},
		},
		{
			// One host working answers the question whatever is wrong with the
			// other.
			"one host works, one does not",
			tool{ghv, answer{out: `{"hosts":{"github.com":[{"state":"success","login":"octocat"}],` +
				`"ghe.example.com":[{"state":"timeout","login":"octocat"}]}}`}, answer{}},
			true, []string{"github.com"},
		},
		{"signed in, gh too old for --json", tool{ghv, noJSON, answer{out: ghProse}}, true, []string{"github.com"}},
		{"signed out, gh too old for --json", tool{ghv, noJSON, answer{err: ghSignedOut, code: 1}}, false, nil},
		{
			// The wording gh used before its accounts rework.
			"signed in, older wording",
			tool{ghv, noJSON, answer{out: "✓ Logged in to ghe.example.com as octocat (oauth_token)\n"}},
			true, []string{"ghe.example.com"},
		},
		{
			"signed in to two hosts, gh too old for --json",
			tool{ghv, noJSON, answer{out: "github.com\n  ✓ Logged in to github.com account octocat (keyring)\n" +
				"ghe.example.com\n  ✓ Logged in to ghe.example.com account octocat (keyring)\n"}},
			true, []string{"ghe.example.com", "github.com"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			install(t, "gh", tc.gh)
			d := probe(t, "gh")
			if !d.Present || d.Error != "" {
				t.Fatalf("gh = %+v", d)
			}
			if d.LoggedIn == nil || *d.LoggedIn != tc.wantIn || !slices.Equal(d.LoginHosts, tc.wantHosts) {
				t.Errorf("logged in %v hosts %q, want %v %q", d.LoggedIn, d.LoginHosts, tc.wantIn, tc.wantHosts)
			}
		})
	}
}

// A login gh could not confirm is not a login the machine does not have: the
// runner did not find out, and says so rather than guessing.
func TestGHUnconfirmedLoginIsNotAnAnswer(t *testing.T) {
	// gh's two non-success states. error is not safely "signed out" either: it
	// covers an expired token and a host gh could not reach alike, and the two
	// are told apart only by the message beside it, which is not read.
	for _, state := range []string{"timeout", "error"} {
		t.Run(state, func(t *testing.T) {
			install(t, "gh", tool{
				version: answer{out: ghVersion},
				asJSON:  answer{out: `{"hosts":{"ghe.example.com":[{"state":"` + state + `","login":"octocat"}]}}`},
			})
			d := probe(t, "gh")
			if d.LoggedIn != nil {
				t.Errorf("LoggedIn = %v, want nothing claimed", *d.LoggedIn)
			}
			// gh gives a revoked token and an unreachable host the same
			// state, so the report must not pick one — and must name the
			// command that tells them apart.
			if !strings.Contains(d.Error, "could not confirm") {
				t.Errorf("Error = %q, want it to say the login was not confirmed", d.Error)
			}
			if !strings.Contains(d.Error, "gh auth status") {
				t.Errorf("Error = %q, want the command that distinguishes the causes", d.Error)
			}
			for _, guess := range []string{"could not reach the host", "the token is expired", "the token has been revoked"} {
				if strings.Contains(d.Error, guess) {
					t.Errorf("Error = %q, want it not to assert one cause", d.Error)
				}
			}
		})
	}
}

// Asking gh is a network call, so the answer is remembered: the probe runs
// every few seconds and the login changes when somebody signs in or out.
func TestGHLoginIsRememberedBetweenProbes(t *testing.T) {
	install(t, "gh", tool{version: answer{out: ghVersion}, asJSON: answer{out: ghJSON}})
	if d := probe(t, "gh"); d.LoggedIn == nil || !*d.LoggedIn {
		t.Fatalf("first probe: %+v", d)
	}
	// The same gh, now unable to answer at all. The remembered answer stands:
	// a link that was down for one probe is not news that the login changed,
	// and a flapping document would make every hub re-read it.
	swap(t, "gh", tool{version: answer{out: ghVersion}, asJSON: answer{code: 1}, plain: answer{code: 1}})
	d := probe(t, "gh")
	if d.LoggedIn == nil || !*d.LoggedIn || !slices.Equal(d.LoginHosts, []string{"github.com"}) {
		t.Errorf("second probe = %+v, want the remembered answer", d)
	}
	if d.Error != "" {
		t.Errorf("Error = %q, want the remembered answer reported without one", d.Error)
	}
	// With nothing remembered, the same silent gh is reported as unknown.
	forgetGHLogin()
	if d := probe(t, "gh"); d.LoggedIn != nil || d.Error == "" {
		t.Errorf("with nothing remembered = %+v, want no claim and an error", d)
	}
}

// Remembering absorbs a transient; it must not outlive the thing it remembers.
// A token revoked this morning is gh answering `error` — which it also answers
// for a host it could not reach — so a login nothing has confirmed for half an
// hour stops being advertised.
func TestARememberedLoginIsNotServedForever(t *testing.T) {
	for _, tc := range []struct {
		name string
		gh   tool
	}{
		{"the token was revoked", tool{
			version: answer{out: ghVersion},
			asJSON:  answer{out: `{"hosts":{"github.com":[{"state":"error","login":"octocat"}]}}`},
		}},
		{"gh cannot be reached at all", tool{
			version: answer{out: ghVersion},
			asJSON:  answer{code: 1},
			plain:   answer{code: 1},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			install(t, "gh", tool{version: answer{out: ghVersion}, asJSON: answer{out: ghJSON}})
			if d := probe(t, "gh"); d.LoggedIn == nil || !*d.LoggedIn {
				t.Fatalf("first probe: %+v", d)
			}
			swap(t, "gh", tc.gh)

			// Just past the point where the answer is asked for again, and
			// still inside the window a transient is allowed.
			ageGHLogin(ghLoginTTL + time.Minute)
			if d := probe(t, "gh"); d.LoggedIn == nil || !*d.LoggedIn {
				t.Errorf("while the failure could still be a blip = %+v, want the remembered answer", d)
			}

			// Past the window. The runner stops claiming what it cannot check.
			ageGHLogin(ghLoginStale)
			d := probe(t, "gh")
			if d.LoggedIn != nil {
				t.Errorf("LoggedIn = %v, want nothing claimed once the answer is stale", *d.LoggedIn)
			}
			if d.LoginHosts != nil {
				t.Errorf("LoginHosts = %q, want none once the answer is stale", d.LoginHosts)
			}
			if d.Error == "" {
				t.Error("no Error once the answer is stale; a hub is told nothing is known")
			}
		})
	}
}

// Remembered is not forever: an owner who signs out is advertised as signed
// out once the answer is stale, without restarting the runner.
func TestGHLoginIsAskedAgainWhenStale(t *testing.T) {
	install(t, "gh", tool{version: answer{out: ghVersion}, asJSON: answer{out: ghJSON}})
	if d := probe(t, "gh"); d.LoggedIn == nil || !*d.LoggedIn {
		t.Fatalf("first probe: %+v", d)
	}
	old := ghLoginTTL
	ghLoginTTL = -1 // every answer is stale the moment it is given
	t.Cleanup(func() { ghLoginTTL = old })

	swap(t, "gh", tool{version: answer{out: ghVersion}, asJSON: answer{out: `{"hosts":{}}`}})
	d := probe(t, "gh")
	if d.LoggedIn == nil || *d.LoggedIn || d.LoginHosts != nil {
		t.Errorf("after signing out = %+v, want the new answer", d)
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
// may reach a hub — AGENTS.md, and the reason the fixtures carry both.
func TestGHReportCarriesNothingButTheHost(t *testing.T) {
	ghv := answer{out: ghVersion}
	noJSON := answer{err: ghNoJSONFlag, code: 1}
	for name, f := range map[string]tool{
		"signed in, json":   {ghv, answer{out: ghJSON}, answer{out: ghProse}},
		"signed in, prose":  {ghv, noJSON, answer{out: ghProse}},
		"signed out, json":  {ghv, answer{out: `{"hosts":{}}`}, answer{err: ghSignedOut, code: 1}},
		"signed out, prose": {ghv, noJSON, answer{err: ghSignedOut, code: 1}},
		"a token gh cannot use": {ghv,
			answer{out: `{"hosts":{"github.com":[{"state":"error","login":"octocat"}]}}`}, answer{}},
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
		if !d.Present || d.Version != "29.1.3" || d.Error != "" {
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
		// Errors carry the next action (AGENTS.md).
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
	// Each fake is given the command it hangs with, which marks the moment it
	// starts hanging before it sleeps. PATH is empty, so sleep is named in full.
	hangsAtOnce := func(hang string) string { return hang + "\n" }
	// Answers the version and hangs on everything else — the shape that
	// matters, since the second probe is the one waiting on a network or a
	// daemon.
	answersVersion := func(banner string) func(string) string {
		return func(hang string) string {
			return "case \"$1\" in\n  --version) echo '" + banner + "' ;;\n  *) " + hang + " ;;\nesac\n"
		}
	}
	// The same, with a version that takes longer to come than the hang is
	// given: what a loaded machine does to the spawn of a fresh script, done on
	// purpose. The version is still read, because only the probe that hangs is
	// bounded tightly — one bound over both is what made the case above red
	// under the full suite (DEV-100).
	slowVersion := func(banner string) func(string) string {
		return func(hang string) string {
			return "case \"$1\" in\n  --version) /bin/sleep 1; echo '" + banner + "' ;;\n  *) " + hang + " ;;\nesac\n"
		}
	}
	// Only the probe that hangs is shortened, and it may have the shortest
	// timeout there is: however slow the machine, a probe that has not
	// answered by the deadline is the answer. The one before it keeps
	// answerBudget, since it has to outlast a spawn on a loaded machine.
	const hang = 250 * time.Millisecond
	for _, tc := range []struct {
		name, id string
		body     func(hang string) string
		// hangsOn is the seam of the probe that never answers: the version's
		// when the tool hangs at once.
		hangsOn     *time.Duration
		wantErr     string
		wantVersion string
	}{
		{"git hangs on --version", "git", hangsAtOnce, &VersionTimeoutForTests, "no answer to `git --version`", ""},
		{"gh hangs on --version", "gh", hangsAtOnce, &VersionTimeoutForTests, "no answer to `gh --version`", ""},
		{"gh hangs on auth status", "gh", answersVersion("gh version 2.98.0"), &StatusTimeoutForTests, "gh auth status", "2.98.0"},
		{"docker hangs on the daemon", "docker", answersVersion("Docker version 29.1.3"), &StatusTimeoutForTests, "did not answer", "29.1.3"},
		{"gh answers its version slowly, then hangs on auth status", "gh", slowVersion("gh version 2.98.0"), &StatusTimeoutForTests, "gh auth status", "2.98.0"},
		{"docker answers its version slowly, then hangs on the daemon", "docker", slowVersion("Docker version 29.1.3"), &StatusTimeoutForTests, "did not answer", "29.1.3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shorten(t, tc.hangsOn, hang)
			hung := filepath.Join(t.TempDir(), "hung")

			d := hanging(t, tc.id, tc.body(": > '"+hung+"'; /bin/sleep 60"))
			// Timed from the moment the fake began to hang, not from the
			// start of the probe: the spawns before it are the machine's, and
			// on a loaded one they alone have taken longer than this bound
			// (DEV-100). Three seconds of slack for the kill and the reap, and
			// still under the shipped five, so a probe that bounded its hang
			// with the other seam, or with the shipped value, is caught here
			// rather than passing a bound wide enough for anything. A fake
			// that never marked was cut off before it could hang, which leaves
			// nothing to time.
			if fi, err := os.Stat(hung); err == nil {
				if held := time.Since(fi.ModTime()); held > hang+3*time.Second {
					t.Errorf("hung for %s; the %s timeout is not bounding the probe", held, hang)
				}
			}
			// The report names the timeout it waited, and the only reason to
			// trust the bound above is that the two are the same value.
			if !strings.Contains(d.Error, hang.String()) {
				t.Errorf("Error = %q, want it to name the %s it waited", d.Error, hang)
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

// A binary that is there but will not run is reported broken, not dropped —
// and the report says so without quoting the machine it is on.
func TestBrokenBinaryIsReported(t *testing.T) {
	dir := t.TempDir() // a directory is not an executable
	isolate(t, "YAD_GIT_PATH", dir)
	d := probe(t, "git")
	if !d.Present || d.Error == "" {
		t.Fatalf("git = %+v, want it present and broken", d)
	}
	// supervise.Start wraps this as `start <path>: fork/exec <path>: ...`, and
	// on a Mac that path is under /Users/<name>. DEV-31 says the owner's name
	// never travels, and HostTools' own comment says never a path.
	for _, leak := range []string{dir, "fork/exec", "/Users/"} {
		if strings.Contains(d.Error, leak) {
			t.Errorf("Error carries %q: %q", leak, d.Error)
		}
	}
	if !strings.Contains(d.Error, "check on this machine") {
		t.Errorf("Error = %q, want the next action", d.Error)
	}
}

// What a tool prints on its way out is unbounded text nobody vetted — a loader
// error naming a home directory, a proxy URL with a password in it. None of it
// is forwarded.
func TestToolOutputIsNeverQuotedInTheReport(t *testing.T) {
	const secret = "https://user:hunter2@proxy.internal/"
	install(t, "git", tool{version: answer{err: "fatal: unable to access " + secret + "\n", code: 128}})
	d := probe(t, "git")
	if d.Error == "" {
		t.Fatalf("git = %+v, want the failure reported", d)
	}
	for _, leak := range []string{secret, "hunter2", "fatal:", "exit status"} {
		if strings.Contains(d.Error, leak) {
			t.Errorf("Error carries %q: %q", leak, d.Error)
		}
	}
	if !strings.Contains(d.Error, "run it on this machine") {
		t.Errorf("Error = %q, want the next action", d.Error)
	}
}

func TestGHFromJSON(t *testing.T) {
	for _, tc := range []struct {
		name, raw          string
		wantHosts          []string
		wantUnsure, wantOK bool
	}{
		{"signed in", ghJSON, []string{"github.com"}, false, true},
		{"no hosts at all", `{"hosts":{}}`, nil, false, true},
		{"null hosts", `{"hosts":null}`, nil, false, false},
		{"not json", ghNoJSONFlag, nil, false, false},
		{"empty", "", nil, false, false},
		{"a token gh cannot use", `{"hosts":{"github.com":[{"state":"error"}]}}`, nil, true, true},
		{"a host gh could not reach in time", `{"hosts":{"ghe.example.com":[{"state":"timeout"}]}}`, nil, true, true},
		{
			"one host works, another does not",
			`{"hosts":{"github.com":[{"state":"success"}],"ghe.example.com":[{"state":"error"}]}}`,
			[]string{"github.com"}, false, true,
		},
		{
			// Sorted, because a map has no order and an answer that moved
			// between probes would move the capability fingerprint with it.
			"two hosts come back sorted",
			`{"hosts":{"github.com":[{"state":"success"}],"acme.example.com":[{"state":"success"}]}}`,
			[]string{"acme.example.com", "github.com"}, false, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hosts, unsure, ok := ghFromJSON([]byte(tc.raw))
			if !slices.Equal(hosts, tc.wantHosts) || unsure != tc.wantUnsure || ok != tc.wantOK {
				t.Errorf("ghFromJSON = %q, %v, %v; want %q, %v, %v", hosts, unsure, ok, tc.wantHosts, tc.wantUnsure, tc.wantOK)
			}
		})
	}
}

func TestGHFromProse(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      []string
	}{
		{"current wording", ghProse, []string{"github.com"}},
		{"the wording before gh's accounts rework", "✓ Logged in to ghe.example.com as octocat (oauth_token)", []string{"ghe.example.com"}},
		{"two hosts, sorted", "Logged in to github.com account octocat\nLogged in to acme.example.com account octocat", []string{"acme.example.com", "github.com"}},
		{"the same host twice", "Logged in to github.com account octocat\nLogged in to github.com account someone-else", []string{"github.com"}},
		{"signed out", ghSignedOut, nil},
		{"empty", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ghFromProse(tc.raw); !slices.Equal(got, tc.want) {
				t.Errorf("ghFromProse = %q, want %q", got, tc.want)
			}
		})
	}
}

// A probe whose deadline fires reports the timeout, never the exit status of
// the child its own kill produced. Whether it timed out is supervise.Run's to
// say, and supervise.TestRunTimedOutIsTheLeadersFate proves it says so every
// time; this is the same pair of children seen through the host-tool report,
// which is where DEV-69 found them going red.
func TestATimedOutProbeIsNeverReportedAsAnExit(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		// PATH is empty inside isolate, so sleep is named in full.
		{"closes stdout, then hangs", "exec 1>&-\n/bin/sleep 60\n"},
		{"hangs holding stdout", "/bin/sleep 60\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shorten(t, &VersionTimeoutForTests, 250*time.Millisecond)

			d := hanging(t, "git", tc.body)
			if !strings.Contains(d.Error, "no answer to `git --version`") {
				t.Errorf("Error = %q, want the timeout rather than the child's fate", d.Error)
			}
		})
	}
}
