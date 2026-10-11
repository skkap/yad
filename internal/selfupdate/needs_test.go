package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/shellword/shellwordtest"
)

// profileDir is a profile's config directory that is still there: it holds a
// config.toml.
func profileDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// record writes a profile's record beside the harness's binary, as that
// profile's daemon would.
func (h *harness) record(t *testing.T, r Record) string {
	t.Helper()
	dir, err := NeedsDir(h.target)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRecord(dir, r); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, recordName(r.ConfigDir))
}

// Decision 0078: two profiles share one binary, and the one updating it is
// not the only one that runs it. A release that drops a major only the other
// profile's hub syncs over is refused, and the refusal names that profile.
// When that profile lets its hub go, the release is judged again — without a
// download while nothing has changed — and taken.
func TestARunnerRefusesAReleaseAnotherProfileOnItsBinaryCannotUse(t *testing.T) {
	h := newHarness(t)
	h.about.Protocols = []string{"2"}
	o := h.options()
	o.ConfigDir = profileDir(t)
	o.Needs = map[string][]string{"2": {"home"}}
	work := profileDir(t)
	h.record(t, Record{ConfigDir: work, Profile: "work", Protocols: map[string][]string{"1": {"zumino"}}, Version: "v0.8.0"})

	u := New(o)
	if u.Check(context.Background()) {
		t.Fatal("Check took a release the work profile's hub cannot sync with")
	}
	if got := h.installed(t); got != "v0.8.0" {
		t.Errorf("installed binary is %q, want the one both profiles run", got)
	}
	r := u.Status().Refused
	if r == nil {
		t.Fatalf("nothing refused; status %+v", u.Status())
	}
	for _, want := range []string{"no longer speaks protocol v1", "profile work's zumino syncs over", work, "it speaks v2", "yad upgrade --tag v0.9.0"} {
		if !strings.Contains(r.Reason, want) {
			t.Errorf("reason %q does not say %q", r.Reason, want)
		}
	}
	if strings.Contains(r.Reason, "home") {
		t.Errorf("reason %q names this runner's connection, which the release speaks for", r.Reason)
	}

	if u.Check(context.Background()) {
		t.Fatal("the second check took it")
	}
	if _, downloads := h.src.counts(); downloads != 1 {
		t.Errorf("%d downloads; the refused release is judged again from what it said, not fetched again", downloads)
	}

	// The work profile disconnects its hub; its daemon writes that down.
	h.record(t, Record{ConfigDir: work, Profile: "work", Protocols: map[string][]string{}, Version: "v0.8.0"})
	if !u.Check(context.Background()) {
		t.Fatalf("Check still refused a release nothing now needs more of; status %+v", u.Status().Refused)
	}
	if got := h.installed(t); got != "v0.9.0" {
		t.Errorf("installed binary is %q, want the release", got)
	}
	if u.Status().Refused != nil {
		t.Error("the refusal outlived the install")
	}
}

// A profile's own record is its own needs, as config.toml read when it was
// written: a connection made since the process started is one the release
// would sync over after the takeover. It is named as this runner's, not as
// another profile's.
func TestARunnersOwnRecordAddsAConnectionMadeSinceItStarted(t *testing.T) {
	h := newHarness(t)
	h.about.Protocols = []string{"2"}
	o := h.options()
	o.ConfigDir = profileDir(t)
	o.Needs = map[string][]string{"1": {"home"}}
	h.record(t, Record{ConfigDir: o.ConfigDir, Profile: "default", Protocols: map[string][]string{"1": {"home", "later"}}})

	u := New(o)
	if u.Check(context.Background()) {
		t.Fatal("Check took a release this runner's hubs cannot sync with")
	}
	r := u.Status().Refused
	if r == nil || !strings.Contains(r.Reason, "v1, which home, later sync over") || strings.Contains(r.Reason, "profile default") {
		t.Errorf("refused %+v, want this runner's two connections named as its own", r)
	}
}

// A record whose profile is gone — its config directory holds no
// config.toml — is passed over and removed: nothing will start that profile
// to need anything.
func TestARecordWhoseProfileIsGoneIsRemoved(t *testing.T) {
	h := newHarness(t)
	h.about.Protocols = []string{"2"}
	o := h.options()
	o.ConfigDir = profileDir(t)
	o.Needs = nil
	gone := filepath.Join(t.TempDir(), "profiles", "old")
	path := h.record(t, Record{ConfigDir: gone, Profile: "old", Protocols: map[string][]string{"1": {"zumino"}}})

	if !New(o).Check(context.Background()) {
		t.Fatalf("Check refused for a profile that no longer exists; log:\n%s", h.log)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the gone profile's record is still there (%v)", err)
	}
}

// A record no one can read may be a live profile's, and what it needs cannot
// be told: no release is taken, and nothing is downloaded, while it is there.
// It is a failed check rather than a refusal — the next check, once the
// record is removed or written again, goes ahead — and the error carries the
// command that removes it.
func TestAnUnreadableRecordStopsTheCheck(t *testing.T) {
	for _, c := range []struct{ name, body string }{
		{"not json", "{half a record"},
		{"no protocols field", `{"config_dir": "/home/a/.config/yad", "profile": "default"}`},
		{"relative config dir", `{"config_dir": "yad", "protocols": {}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			o := h.options()
			o.ConfigDir = profileDir(t)
			dir, err := NeedsDir(h.target)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			// A path with a space and a quote, so the command offered has
			// to quote it to remove the right file.
			path := filepath.Join(dir, "it's broken.json")
			if err := os.WriteFile(path, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}

			u := New(o)
			if u.Check(context.Background()) {
				t.Fatal("Check took a release with a record it could not read")
			}
			st := u.Status()
			if st.Refused != nil || !strings.Contains(st.LastError, path) {
				t.Fatalf("status %+v, want a failed check naming %s", st, path)
			}
			cmds := shellwordtest.Commands(st.LastError, "rm ")
			if len(cmds) != 1 {
				t.Fatalf("error %q offers %d rm commands, want one", st.LastError, len(cmds))
			}
			shellwordtest.Check(t, cmds[0], "rm", path)
			if _, downloads := h.src.counts(); downloads != 0 {
				t.Errorf("%d downloads with a record unread", downloads)
			}

			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if !u.Check(context.Background()) {
				t.Fatalf("the check after the record was removed took nothing; status %+v", u.Status())
			}
		})
	}
}

// What the daemon writes reads back as written, beside the file the binary's
// symlink names — the file a release is renamed over — and readable by any
// user who runs that binary: it holds no secret.
func TestARecordIsWrittenBesideTheRealBinary(t *testing.T) {
	real := filepath.Join(t.TempDir(), "yad")
	if err := os.WriteFile(real, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "yad")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	dir, err := NeedsDir(link)
	if err != nil {
		t.Fatal(err)
	}
	realDir, _ := filepath.EvalSymlinks(filepath.Dir(real))
	if want := filepath.Join(realDir, NeedsDirName); dir != want {
		t.Errorf("NeedsDir(link) = %s, want beside the real binary, %s", dir, want)
	}

	cfg := profileDir(t)
	at := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	in := Record{ConfigDir: cfg, Profile: "work", Protocols: map[string][]string{"1": {"zumino"}}, Version: "v0.9.0", Written: at}
	if err := WriteRecord(dir, in); err != nil {
		t.Fatal(err)
	}
	out, ok, err := ReadRecord(dir, cfg)
	if err != nil || !ok {
		t.Fatalf("ReadRecord = %v, %v", ok, err)
	}
	if out.ConfigDir != cfg || out.Profile != "work" || strings.Join(out.Protocols["1"], ",") != "zumino" || out.Version != "v0.9.0" || !out.Written.Equal(at) {
		t.Errorf("read back %+v, want %+v", out, in)
	}
	fi, err := os.Stat(filepath.Join(dir, recordName(cfg)))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("record mode %v, want 0644", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("the needs directory holds %d entries, want the record alone — no temporary file left", len(entries))
	}

	// A profile with no hub still records that it needs nothing, never an
	// absent field, which a reader cannot tell from a shape it does not know.
	if err := WriteRecord(dir, Record{ConfigDir: cfg, Profile: "work"}); err != nil {
		t.Fatal(err)
	}
	if out, _, err := ReadRecord(dir, cfg); err != nil || out.Protocols == nil {
		t.Errorf("a hubless profile's record reads back %+v, %v", out, err)
	}
	if err := WriteRecord(dir, Record{ConfigDir: "relative"}); err == nil {
		t.Error("WriteRecord took a relative config directory")
	}
}
