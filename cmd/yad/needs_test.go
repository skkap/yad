package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/selfupdate"
)

// needsProfile is a profile whose config.toml has these connections, and a
// binary for it in a directory of its own.
func needsProfile(t *testing.T, conns ...string) (config.Paths, config.Config, string) {
	t.Helper()
	p := config.Paths{Profile: "work", Config: t.TempDir(), Data: t.TempDir()}
	cfg := saveConnections(t, p, conns...)
	bin := filepath.Join(t.TempDir(), "yad")
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	return p, cfg, bin
}

func saveConnections(t *testing.T, p config.Paths, conns ...string) config.Config {
	t.Helper()
	cfg := config.Default()
	for _, c := range conns {
		cfg.Connections = append(cfg.Connections, config.Connection{Name: c, URL: "https://" + c + ".example"})
	}
	if err := config.Save(p, cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func readNeeds(t *testing.T, bin string, p config.Paths) (selfupdate.Record, bool) {
	t.Helper()
	dir, err := selfupdate.NeedsDir(bin)
	if err != nil {
		t.Fatal(err)
	}
	r, ok, err := selfupdate.ReadRecord(dir, p.Config)
	if err != nil {
		t.Fatal(err)
	}
	return r, ok
}

// Decision 0078: every daemon records beside its binary what its hubs need of
// it — at start, and again when config.toml's connections change or the
// record goes missing — so a profile updating the binary can see it.
func TestADaemonRecordsItsNeedsBesideItsBinary(t *testing.T) {
	p, cfg, bin := needsProfile(t, "home")
	// A `yad connect` lands after the daemon loaded config.toml and before
	// it wrote its first record: the record is the file's, not the load's.
	saveConnections(t, p, "home", "early")
	var log bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := recordNeeds(ctx, p, cfg, bin, slog.New(slog.NewTextHandler(&log, nil)))

	got, ok := readNeeds(t, bin, p)
	if !ok || got.Profile != "work" || strings.Join(got.Protocols["1"], ",") != "home,early" || got.Written.IsZero() {
		t.Fatalf("the record at start is %+v (there: %v), want profile work's home and early on v1", got, ok)
	}

	saveConnections(t, p, "home", "zumino")
	r.refresh()
	if got, _ := readNeeds(t, bin, p); strings.Join(got.Protocols["1"], ",") != "home,zumino" {
		t.Errorf("after a connection was added, the record says %v", got.Protocols)
	}

	saveConnections(t, p)
	r.refresh()
	if got, _ := readNeeds(t, bin, p); got.Protocols == nil || len(got.Protocols) != 0 {
		t.Errorf("after the last hub went, the record says %v, want nothing needed", got.Protocols)
	}

	dir, _ := selfupdate.NeedsDir(bin)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	r.refresh()
	if _, ok := readNeeds(t, bin, p); !ok {
		t.Error("a record removed from under a running daemon was not written again")
	}
	if r.Unrecorded() != "" || strings.Contains(log.String(), "level=WARN") {
		t.Errorf("unrecorded %q; log:\n%s", r.Unrecorded(), log.String())
	}
}

// Absence is data: a daemon that can replace its binary but cannot write its
// record beside it — a needs directory left read-only — warns once in the log
// and in `yad status`, and runs as before, since a profile of this user that
// updates the binary would not see its needs. It writes the record once it
// can.
func TestADaemonThatCannotRecordItsNeedsWarnsAndCarriesOn(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	p, cfg, bin := needsProfile(t, "home")
	needsDir := filepath.Join(filepath.Dir(bin), selfupdate.NeedsDirName)
	if err := os.Mkdir(needsDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(needsDir, 0o755) })
	var log bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := recordNeeds(ctx, p, cfg, bin, slog.New(slog.NewTextHandler(&log, nil)))

	why := r.Unrecorded()
	if why == "" || !strings.Contains(why, filepath.Base(needsDir)) {
		t.Fatalf("unrecorded %q, want why, naming %s", why, needsDir)
	}
	r.refresh()
	if n := strings.Count(log.String(), "level=WARN"); n != 1 {
		t.Errorf("%d warnings over two attempts, want one; log:\n%s", n, log.String())
	}

	var out bytes.Buffer
	now := time.Now()
	printStatus(&out, p, control.Status{Started: now, NeedsUnrecorded: why}, now)
	if !strings.Contains(out.String(), "this profile's protocol needs are not recorded beside the binary it runs — another profile that updates it would not see them") {
		t.Errorf("status does not say the needs are unrecorded:\n%s", out.String())
	}

	if err := os.Chmod(needsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	r.refresh()
	if r.Unrecorded() != "" {
		t.Errorf("still unrecorded once the directory is writable: %s", r.Unrecorded())
	}
	if _, ok := readNeeds(t, bin, p); !ok {
		t.Error("no record once the directory is writable")
	}
}

// A binary only root may replace — a root-owned yad in /usr/local/bin, as the
// work-machine kit installs it with self-update off — is one no runner that
// would read the record can update, so a record not written there is no
// warning: a note in the log, and nothing in `yad status`. /bin/sh stands in
// for it: a binary in a directory root owns and nobody else may write.
func TestADaemonBesideABinaryOnlyRootMayReplaceIsQuietAboutItsNeeds(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root may write beside /bin/sh")
	}
	const rootsOnly = "/bin/sh"
	if selfupdate.CanReplace(rootsOnly) || !selfupdate.OnlyRoot(rootsOnly) {
		t.Skipf("%s is not in a directory only root may write on this machine", rootsOnly)
	}
	p, cfg, _ := needsProfile(t, "home")
	var log bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := recordNeeds(ctx, p, cfg, rootsOnly, slog.New(slog.NewTextHandler(&log, nil)))
	r.refresh()

	if why := r.Unrecorded(); why != "" {
		t.Errorf("unrecorded %q for `yad status`, want nothing beside a binary only root may replace", why)
	}
	if strings.Contains(log.String(), "level=WARN") {
		t.Errorf("warned beside a binary only root may replace:\n%s", log.String())
	}
	if n := strings.Count(log.String(), "only root may replace that binary"); n != 1 {
		t.Errorf("%d notes over two attempts, want one; log:\n%s", n, log.String())
	}
}

// A binary this user cannot replace, in a directory a user other than root
// owns, is one that user's runner may update — and it would need this
// profile's needs. Not writing them stays a warning.
func TestADaemonBesideAnotherUsersBinaryStillWarns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	p, cfg, bin := needsProfile(t, "home")
	binDir := filepath.Dir(bin)
	if err := os.Chmod(binDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(binDir, 0o755) })
	var log bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := recordNeeds(ctx, p, cfg, bin, slog.New(slog.NewTextHandler(&log, nil)))

	if r.Unrecorded() == "" || !strings.Contains(log.String(), "level=WARN") {
		t.Errorf("unrecorded %q and log:\n%s\nwant a warning beside a binary its non-root owner may update", r.Unrecorded(), log.String())
	}
}
