package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skkap/yad/internal/buildinfo"
	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/selfupdate"
	"github.com/skkap/yad/internal/shellword"
	"github.com/skkap/yad/internal/upgrade"
)

// What a runner re-executed as yad by these tests reads, since no fake clock
// or seam reaches a child process.
const (
	// e2eVersion stamps the child as a release.
	e2eVersion = "E2E_YAD_VERSION"
	// e2eReleases is where the child fetches releases: a server in the test.
	e2eReleases = "E2E_RELEASES_URL"
	// e2eUpdateFast shortens the self-update's schedule to fit a test.
	e2eUpdateFast = "E2E_UPDATE_FAST"
)

// selfUpdateChild applies them, in a child about to run as yad.
func selfUpdateChild() {
	if v := os.Getenv(e2eVersion); v != "" {
		buildinfo.Version = v
	}
	if u := os.Getenv(e2eReleases); u != "" {
		releasesURL = u
	}
	if os.Getenv(e2eUpdateFast) != "" {
		selfUpdateTimings.first, selfUpdateTimings.every = 100*time.Millisecond, 100*time.Millisecond
		selfUpdateTimings.idleWait, selfUpdateTimings.poll = time.Hour, 20*time.Millisecond
	}
}

// `yad version --json` is what a runner asks a downloaded release before it
// takes it (decision 0071), so what this build prints must read back as what
// it is — and its plain line must read back too, since that is all a release
// older than the flag prints.
func TestVersionJSONNamesTheProtocols(t *testing.T) {
	code, out, errs := yad(t, "version", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	about, err := selfupdate.ParseAbout([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if about.Version != buildinfo.Version || strings.Join(about.Protocols, ",") != "1" {
		t.Errorf("version --json = %+v, want this build speaking v1", about)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil || raw["protocols"] == nil {
		t.Errorf("version --json lacks the protocols field: %s", out)
	}

	code, out, _ = yad(t, "version")
	if code != 0 {
		t.Fatal(code)
	}
	if plain, err := selfupdate.ParseAbout([]byte(out)); err != nil || plain.Version != buildinfo.Version {
		t.Errorf("the plain line %q reads as %+v, %v", out, plain, err)
	}
	if code, _, _ := yad(t, "version", "--yes"); code == 0 {
		t.Error("an unknown flag to version was taken")
	}
}

func TestConnectionProtocols(t *testing.T) {
	if got := connectionProtocols(config.Config{}); got != nil {
		t.Errorf("a runner with no hub needs %v", got)
	}
	cfg := config.Config{Connections: []config.Connection{{Name: "home"}, {Name: "zumino"}}}
	got := connectionProtocols(cfg)
	if len(got) != 1 || strings.Join(got["1"], ",") != "home,zumino" {
		t.Errorf("connectionProtocols = %v, want both connections on v1", got)
	}
}

// `yad status` shows the self-update: the last check or why it failed, a
// release refused and why, and one waiting to take over.
func TestStatusShowsTheSelfUpdate(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)
	at := func(d time.Duration) *time.Time { t := now.Add(d); return &t }
	for _, c := range []struct {
		name string
		u    control.Update
		want []string
	}{
		{"not yet", control.Update{NextCheck: at(10 * time.Minute)}, []string{"self-update on — not checked yet; next check in 10m0s"}},
		{"checked", control.Update{LastCheck: at(-2 * time.Hour), Latest: "v0.8.0", NextCheck: at(4 * time.Hour)},
			[]string{"checked 2h0m0s ago, newest release v0.8.0; next check in 4h0m0s"}},
		{"failed", control.Update{LastCheck: at(-time.Minute), LastError: "could not reach github.com\nretry", NextCheck: at(time.Hour)},
			[]string{"the last check, 1m0s ago, failed: could not reach github.com retry"}},
		{"refused", control.Update{LastCheck: at(-time.Minute), Latest: "v1.0.0",
			Refused: &control.RefusedUpdate{Tag: "v1.0.0", Reason: "release v1.0.0 no longer speaks protocol v1", At: *at(-time.Minute)}},
			[]string{"refused v1.0.0 1m0s ago: release v1.0.0 no longer speaks protocol v1"}},
		{"pending", control.Update{LastCheck: at(-5 * time.Minute), Latest: "v0.9.0",
			Pending: &control.PendingUpdate{Tag: "v0.9.0", Since: *at(-5 * time.Minute), By: *at(24 * time.Hour)}},
			[]string{"v0.9.0 installed 5m0s ago in place of this binary; the runner becomes it at its first idle moment, or drains for it at " + now.Add(24*time.Hour).Format(time.DateTime)}},
		{"swapping", control.Update{LastCheck: at(-5 * time.Minute), Latest: "v0.9.0",
			Pending: &control.PendingUpdate{Tag: "v0.9.0", Since: *at(-5 * time.Minute), By: *at(24 * time.Hour), Swapping: true, Reason: "the runner is idle"}},
			[]string{"taking over — the runner is idle", "re-executes as v0.9.0"}},
		{"off", control.Update{Off: `this build carries no release version ("dev")`}, []string{"self-update on, and does nothing on this build: this build"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var b bytes.Buffer
			printStatus(&b, config.Paths{}, control.Status{Started: now, Update: &c.u}, now)
			for _, want := range c.want {
				if !strings.Contains(b.String(), want) {
					t.Errorf("status lacks %q:\n%s", want, b.String())
				}
			}
		})
	}
	var b bytes.Buffer
	printStatus(&b, config.Paths{}, control.Status{Started: now}, now)
	if strings.Contains(b.String(), "self-update") {
		t.Errorf("status speaks of a self-update config.toml leaves off:\n%s", b.String())
	}
}

// releaseServer serves one release the way github.com does, in process: the
// /releases/latest redirect, the tag's page, and its assets with a
// checksums.txt. Until published is set it has none, as a repository with no
// release redirects /releases/latest to /releases.
func releaseServer(t *testing.T, tag, asset string, binary []byte, published *atomic.Bool) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(binary)
	base := "/" + upgrade.DefaultRepo + "/releases/"
	files := map[string][]byte{
		base + "download/" + tag + "/" + asset:                 binary,
		base + "download/" + tag + "/" + upgrade.ChecksumsName: fmt.Appendf(nil, "%s  %s\n", hex.EncodeToString(sum[:]), asset),
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !published.Load():
			http.Redirect(w, r, strings.TrimSuffix(base, "/"), http.StatusFound)
		case r.URL.Path == base+"latest":
			http.Redirect(w, r, base+"tag/"+tag, http.StatusFound)
		case r.URL.Path == base+"tag/"+tag:
		case files[r.URL.Path] != nil:
			w.Write(files[r.URL.Path])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (m *machine) setAutoUpdate() {
	m.t.Helper()
	p := config.Paths{Config: m.p.config, Data: m.p.data}
	cfg, err := config.Load(p)
	if err != nil {
		m.t.Fatal(err)
	}
	cfg.Update.Auto = true
	if err := config.Save(p, cfg); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) status() (control.Status, bool) {
	code, out, _ := m.p.yad("", "status", "--json")
	var st control.Status
	return st, code == 0 && json.Unmarshal([]byte(out), &st) == nil
}

// The owner's fourth decision, end to end with a real process: a runner with
// self-update on finds a newer release, installs it in place of the binary it
// was started from, keeps the run it holds going to its end, and at the
// first idle moment becomes the new release in the same process — the pid a
// service manager watches never exits. What it held in state.db is there
// after, and it takes work again as the new release.
//
// The release is a script: asked `version --json` it answers for v0.2.0, and
// otherwise it runs this test binary as yad, stamped v0.2.0.
func TestE2ESelfUpdateTakesOverInPlace(t *testing.T) {
	h := claudeE2E
	m := newMachine(t, h)
	m.setAutoUpdate()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// A copy is what the update replaces: never the test binary itself.
	bin := filepath.Join(t.TempDir(), "yad")
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	release := "#!/bin/sh\n" +
		"if [ \"$1\" = version ]; then echo '{\"version\":\"v0.2.0\",\"protocols\":[\"1\"]}'; exit 0; fi\n" +
		"export " + childYad + "=1 " + e2eVersion + "=v0.2.0\n" +
		"exec " + shellword.Quote(self) + " \"$@\"\n"
	asset, err := upgrade.AssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	var published atomic.Bool
	srv := releaseServer(t, "v0.2.0", asset, []byte(release), &published)

	m.gated()
	m.submit("e2e-before")
	c := m.childDaemonAt(bin, e2eVersion+"=v0.1.0", e2eReleases+"="+srv.URL, e2eUpdateFast+"=1")
	pid := c.cmd.Process.Pid
	m.waitAtGate()
	// Published only now, so the release finds the runner busy: until then
	// each check finds nothing, which is a warning and the next check.
	c.said(t, "published no release yet")
	published.Store(true)
	eventually(t, "the release is installed in place of the binary", func() bool {
		got, _ := os.ReadFile(bin)
		return string(got) == release
	})
	// Busy: the release waits, and the runner goes on as it was.
	st, ok := m.status()
	if !ok || st.Version != "v0.1.0" || st.Update == nil || st.Update.Pending == nil || st.Update.Pending.Tag != "v0.2.0" || st.Update.Pending.Swapping {
		t.Fatalf("status while busy: %+v (update %+v)", st, st.Update)
	}
	time.Sleep(200 * time.Millisecond)
	if st, _ := m.status(); st.Update == nil || st.Update.Pending == nil || st.Update.Pending.Swapping {
		t.Fatalf("took over with a run held: %+v", st.Update)
	}

	m.open()
	if code, _, errs := m.watch("e2e-before"); code != 0 {
		t.Fatalf("the run held across the update did not succeed: %s", errs)
	}
	c.said(t, "daemon re-executing as the new release")
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if st, ok := m.status(); ok && st.Version == "v0.2.0" && st.PID == pid {
			break
		}
		if time.Now().After(deadline) {
			st, ok := m.status()
			t.Fatalf("the process did not come back as v0.2.0 (status %v: pid %d version %s, want pid %d)\nits output:\n%s\nits log:\n%s", ok, st.PID, st.Version, pid, c.out.String(), c.logged())
		}
	}
	select {
	case err := <-c.done:
		t.Fatalf("the runner exited rather than re-executing: %v\n%s", err, c.logged())
	default:
	}
	eventually(t, "the new release has set up", func() bool { st, ok := m.status(); return ok && st.Ready })
	if st, _ := m.status(); st.Update == nil || st.Update.Pending != nil || st.Sessions != 1 {
		t.Errorf("after the takeover: sessions %d, update %+v; want the session kept and nothing pending", st.Sessions, st.Update)
	}

	m.submit("e2e-after")
	if code, _, errs := m.watch("e2e-after"); code != 0 {
		t.Fatalf("the new release did not take work: %s\n%s", errs, c.logged())
	}
}
