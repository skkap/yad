package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skkap/yad/protocol/hubapi"
	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/config"
	"github.com/skkap/yad/internal/control"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/shellword/shellwordtest"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// `yad disconnect` end to end (DEV-81, decision 0069): the hub first, then
// config.toml and the credential, then the daemon — told live, or ending
// what is left at its next start.

// paths is the machine's runner profile, as the commands resolve it.
func (m *machine) paths() config.Paths {
	m.t.Helper()
	m.t.Setenv("YAD_CONFIG_DIR", m.p.config)
	m.t.Setenv("YAD_DATA_DIR", m.p.data)
	p, err := config.Resolve("")
	if err != nil {
		m.t.Fatal(err)
	}
	return p
}

// connections is what config.toml lists.
func (m *machine) connections() []string {
	m.t.Helper()
	cfg, err := config.Load(m.paths())
	if err != nil {
		m.t.Fatal(err)
	}
	var names []string
	for _, c := range cfg.Connections {
		names = append(names, c.Name)
	}
	return names
}

func (m *machine) credential() (string, bool) {
	b, err := os.ReadFile(filepath.Join(m.p.config, "credentials", "home"))
	return strings.TrimSpace(string(b)), err == nil
}

// retired says whether the hub refuses the credential as one it does not
// know, which is what a deregister leaves.
func (m *machine) retired(cred string) bool {
	m.t.Helper()
	cfgURL := m.hubURL()
	c, err := hubclient.New(cfgURL, cred)
	if err != nil {
		m.t.Fatal(err)
	}
	id, err := m.paths().RunnerID()
	if err != nil {
		m.t.Fatal(err)
	}
	_, err = c.Sync(context.Background(), id, v1.SyncRequest{})
	return hubclient.Code(err) == v1.CodeUnauthorized
}

// hubURL is the URL the runner dials the hub by, from its first connection.
func (m *machine) hubURL() string {
	m.t.Helper()
	if m.runnerURL == "" {
		m.t.Fatal("no runner-side hub URL")
	}
	return m.runnerURL
}

// finished submits a run, waits for its result, and returns its session as
// the runner holds it.
func (m *machine) finished(runID string) db.Session {
	m.t.Helper()
	m.submit(runID)
	if code, out, errs := m.watch(runID); code != 0 {
		m.t.Fatalf("watch exit %d: %s\n%s", code, errs, out)
	}
	s := m.runnerStore()
	r := localRun(m.t, s, runID)
	sess, err := s.GetSession(context.Background(), db.GetSessionParams{Connection: "home", ID: r.SessionID})
	if err != nil || sess.Workdir == "" {
		m.t.Fatalf("session %+v, %v: a finished run's session has a workdir", sess, err)
	}
	return sess
}

// ended waits for the connection's session to be closed by the owner, owed
// to nobody, and its workdir gone.
func (m *machine) ended(sess db.Session) {
	m.t.Helper()
	s := m.runnerStore()
	eventually(m.t, "the removed hub's session is closed and its workdir reclaimed", func() bool {
		now, err := s.GetSession(context.Background(), db.GetSessionParams{Connection: "home", ID: sess.ID})
		_, statErr := os.Stat(sess.Workdir)
		return err == nil && now.State == "closed" && now.ReclaimedAt.Valid && now.ReportedAt.Valid && errors.Is(statErr, os.ErrNotExist)
	})
	now, _ := s.GetSession(context.Background(), db.GetSessionParams{Connection: "home", ID: sess.ID})
	if now.CloseReason.String != string(v1.SessionClosedByOwner) {
		m.t.Errorf("the session closed for %q, want closed by the owner", now.CloseReason.String)
	}
}

func (m *machine) gone(cred string) {
	m.t.Helper()
	if names := m.connections(); len(names) != 0 {
		m.t.Errorf("config.toml still lists %v", names)
	}
	if _, ok := m.credential(); ok {
		m.t.Error("the credential file is still there")
	}
	if !m.retired(cred) {
		m.t.Error("the hub still takes the credential")
	}
}

// With the daemon running: the hub retires the runner, config.toml and the
// credential lose the connection, and the daemon lets it go at once — its
// session closes and its workdir goes — and stays up with nothing to sync.
func TestE2EDisconnectWithTheDaemonRunning(t *testing.T) {
	m := newMachine(t, claudeE2E)
	d := m.daemon()
	sess := m.finished("e2e-1")
	cred, _ := m.credential()

	out := m.ok("disconnect", "home")
	for _, want := range []string{"has retired this runner", "is removed from", `has let "home" go`, "closed 1 session(s)", "no hub is connected now"} {
		if !strings.Contains(out, want) {
			t.Errorf("disconnect does not say %q:\n%s", want, out)
		}
	}
	shellwordtest.CheckEnv(t, onlyCommand(t, out, "yad --profile default daemon restart"), dirsEnv(t),
		"yad", "--profile", "default", "daemon", "restart")
	m.gone(cred)
	m.ended(sess)
	status := m.ok("status")
	if strings.Contains(status, "\nconnections\n") || !strings.Contains(status, "no connections") {
		t.Errorf("status still shows the removed connection:\n%s", status)
	}
	select {
	case code := <-d.done:
		t.Fatalf("the daemon exited %d when its last connection was removed:\n%s", code, d.out.String())
	default:
	}
}

// With no daemon: nothing in the state database is touched — the CLI never
// writes it (decision 0043) — and the next start ends what the connection
// left, reclaiming its workdir (DEV-74).
func TestE2EDisconnectWithNoDaemon(t *testing.T) {
	m := newMachine(t, claudeE2E)
	d := m.daemon()
	sess := m.finished("e2e-1")
	d.halt(t)
	cred, _ := m.credential()

	out := m.ok("disconnect", "home")
	if !strings.Contains(out, "no daemon is running") || !strings.Contains(out, "has retired this runner") {
		t.Errorf("disconnect with no daemon:\n%s", out)
	}
	shellwordtest.CheckEnv(t, onlyCommand(t, out, "yad --profile default daemon start"), dirsEnv(t),
		"yad", "--profile", "default", "daemon", "start")
	m.gone(cred)
	now, err := m.runnerStore().GetSession(context.Background(), db.GetSessionParams{Connection: "home", ID: sess.ID})
	if err != nil || now.State != "open" {
		t.Fatalf("with no daemon the session is %q (%v); only a daemon writes the state database", now.State, err)
	}

	m.daemon()
	m.ended(sess)
}

// Runs in progress refuse the disconnect and are named, with the retry and
// --now as commands that run as printed; nothing is retired. --now stops
// them, and the hub records them lost.
func TestE2EDisconnectRefusesRunsInProgress(t *testing.T) {
	m := newMachine(t, claudeE2E)
	m.gated()
	m.submit("e2e-held")
	d := m.daemon()
	m.waitAtGate()

	code, _, errs := m.p.yad("", "disconnect", "home")
	if code != 1 || !strings.Contains(errs, "e2e-held (running") || !strings.Contains(errs, "Nothing was retired or removed") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile default disconnect home --now"), dirsEnv(t),
		"yad", "--profile", "default", "disconnect", "home", "--now")
	shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile default status"), dirsEnv(t),
		"yad", "--profile", "default", "status")
	if names := m.connections(); len(names) != 1 {
		t.Errorf("a refused disconnect changed config.toml: %v", names)
	}
	if run, err := m.client().Run(context.Background(), "e2e-held"); err != nil || run.State.Terminal() {
		t.Fatalf("a refused disconnect ended the run at the hub: %+v %v", run, err)
	}

	out := m.ok("disconnect", "home", "--now")
	if !strings.Contains(out, "stopped 1 run(s) of it — e2e-held") {
		t.Errorf("disconnect --now:\n%s\ndaemon:\n%s", out, d.out.String())
	}
	run, err := m.client().Run(context.Background(), "e2e-held")
	if err != nil || run.State != hubapi.RunState(v1.RunLost) {
		t.Errorf("the hub has the run %+v (%v), want lost", run, err)
	}
	s := m.runnerStore()
	eventually(t, "the run in hand is stopped", func() bool {
		return localRun(t, s, "e2e-held").State == string(v1.RunCancelled)
	})
	// Its result is the hub's no longer: nothing is left owed.
	eventually(t, "nothing is owed to the removed hub", func() bool {
		o, err1 := s.OutboxDepth(context.Background())
		sp, err2 := s.SpoolDepth(context.Background())
		return err1 == nil && err2 == nil && o == 0 && sp == 0
	})
	time.Sleep(100 * time.Millisecond)
	if run, _ := m.client().Run(context.Background(), "e2e-held"); run.State != hubapi.RunState(v1.RunLost) {
		t.Errorf("after the stop the hub has the run %s; a deregister's lost stands", run.State)
	}
}

// A hub that cannot be reached retires nothing, and nothing here is removed;
// the error says so with the retry and --force as commands. Once the hub is
// back, the same command finishes.
func TestE2EDisconnectWithTheHubUnreachable(t *testing.T) {
	m := newMachine(t, claudeE2E)
	cred, _ := m.credential()
	all := func(*http.Request) bool { return true }
	m.cut.Store(&all)

	code, _, errs := m.p.yad("", "disconnect", "home")
	if code != 1 || !strings.Contains(errs, "could not be reached") || !strings.Contains(errs, "Nothing was retired or removed") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile default disconnect home --force"), dirsEnv(t),
		"yad", "--profile", "default", "disconnect", "home", "--force")
	if names := m.connections(); len(names) != 1 {
		t.Errorf("config.toml lost the connection: %v", names)
	}
	if _, ok := m.credential(); !ok {
		t.Error("the credential is gone with the hub never told")
	}

	m.cut.Store(nil)
	if m.retired(cred) {
		t.Fatal("the hub retired the runner while it could not be reached")
	}
	m.ok("disconnect", "home")
	m.gone(cred)
}

// A disconnect that stopped part-way is finished by running it again: after
// the hub has retired the runner, the hub's own answer that it no longer
// knows the credential lets the removal go on; a credential file left with
// no connection naming it is deleted; and a daemon still syncing a
// connection config.toml no longer lists is told.
func TestE2EDisconnectAgainFinishesAPartialOne(t *testing.T) {
	t.Run("the hub already retired it", func(t *testing.T) {
		m := newMachine(t, claudeE2E)
		cred, _ := m.credential()
		id, _ := m.paths().RunnerID()
		c, _ := hubclient.New(m.hubURL(), cred)
		if err := c.Deregister(context.Background(), id, "an earlier disconnect"); err != nil {
			t.Fatal(err)
		}
		out := m.ok("disconnect", "home")
		if !strings.Contains(out, "no longer knew this runner's credential") {
			t.Errorf("disconnect:\n%s", out)
		}
		m.gone(cred)
	})
	t.Run("config.toml done, the daemon not told", func(t *testing.T) {
		m := newMachine(t, claudeE2E)
		m.daemon()
		sess := m.finished("e2e-1")
		// What a disconnect leaves when it stops between config.toml and
		// the credential, or before the daemon: here the entry went by
		// hand, so the hub was never told either and the loop keeps
		// syncing — the daemon learns only from the retry.
		if _, err := config.Update(context.Background(), m.paths(), func(c *config.Config) (bool, error) {
			c.Connections = nil
			return true, nil
		}); err != nil {
			t.Fatal(err)
		}
		out := m.ok("disconnect", "home")
		for _, want := range []string{"credential file it left behind is deleted", "its hub was never told", `has let "home" go`} {
			if !strings.Contains(out, want) {
				t.Errorf("disconnect does not say %q:\n%s", want, out)
			}
		}
		if _, ok := m.credential(); ok {
			t.Error("the credential left behind is still there")
		}
		m.ended(sess)
		// Finished: a third time says it was, and changes nothing.
		out = m.ok("disconnect", "home")
		if !strings.Contains(out, `no longer lists "home"`) || !strings.Contains(out, `had already let "home" go`) {
			t.Errorf("a third disconnect:\n%s", out)
		}
	})
}

// A daemon that does not answer: the hub has retired the runner and the
// connection is out of config.toml, and the error says the daemon was not
// told, what happens meanwhile, and the commands that finish it.
func TestE2EDisconnectWithADaemonThatDoesNotAnswer(t *testing.T) {
	m := newMachine(t, claudeE2E)
	cred, _ := m.credential()
	p := m.paths()
	control.AskTimeoutForTests = 200 * time.Millisecond
	t.Cleanup(func() { control.AskTimeoutForTests = 0 })
	wedged, err := control.Claim(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { wedged.Close() })
	// Holding the lock with no socket to reach: running, and not answering.
	if err := os.Remove(p.Socket()); err != nil {
		t.Fatal(err)
	}

	code, out, errs := m.p.yad("", "disconnect", "home")
	if code != 1 || !strings.Contains(out, "has retired this runner") || !strings.Contains(errs, "the running daemon was not told") ||
		!strings.Contains(errs, `its loop for "home" stops at its next sync`) {
		t.Fatalf("exit %d: %s\n%s", code, out, errs)
	}
	shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile default disconnect home"), dirsEnv(t),
		"yad", "--profile", "default", "disconnect", "home")
	shellwordtest.CheckEnv(t, onlyCommand(t, errs, "yad --profile default daemon restart"), dirsEnv(t),
		"yad", "--profile", "default", "daemon", "restart")
	m.gone(cred)
}

// Finding 1 of the parked branch: a credential that cannot be read is never
// taken for a dead one. The hub is not asked, nothing is removed, and --force
// removes the connection saying the runner is still registered there.
func TestE2EDisconnectNeverCallsAnUnreadCredentialDead(t *testing.T) {
	m := newMachine(t, claudeE2E)
	cred, _ := m.credential()
	file := filepath.Join(m.p.config, "credentials", "home")
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := m.p.yad("", "disconnect", "home")
	if code != 1 || !strings.Contains(errs, "was not asked to retire this runner") || !strings.Contains(errs, "its credential could not be read") ||
		strings.Contains(errs, "already") {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if names := m.connections(); len(names) != 1 {
		t.Errorf("config.toml lost the connection: %v", names)
	}
	out := m.ok("disconnect", "home", "--force")
	if !strings.Contains(out, "still has this runner") || strings.Contains(out, "has retired") {
		t.Errorf("disconnect --force:\n%s", out)
	}
	if m.retired(cred) {
		t.Error("the hub retired a runner nobody asked it to")
	}
	if names := m.connections(); len(names) != 0 {
		t.Errorf("--force left config.toml listing %v", names)
	}
}

// With no daemon, only a run parked on a usage limit or a start time is in
// progress — the next start resumes it — and a run a crashed process held
// is already lost (decision 0030). Read from the state database read-only.
func TestInProgressWithNoDaemonCountsOnlyParkedRuns(t *testing.T) {
	p := newProfile(t)
	p.yad("", "version")
	paths, err := config.Resolve("")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := store.Open(ctx, paths.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	for _, id := range []string{"s1", "s2"} {
		if err := st.CreateSession(ctx, db.CreateSessionParams{Connection: "home", ID: id, Harness: "claude", CreatedAt: now, LastUsedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range [][2]string{{"parked", "s1"}, {"crashed", "s2"}} {
		if err := st.CreateRun(ctx, db.CreateRunParams{Connection: "home", ID: r[0], SessionID: r[1], Harness: "claude", Model: "m", Spec: "{}", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetRunWaiting(ctx, db.SetRunWaitingParams{ResumesAt: sql.NullInt64{Int64: now + 1000, Valid: true}, UpdatedAt: now, Connection: "home", ID: "parked"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	runs, where, err := inProgress(ctx, paths, "home")
	if err != nil || len(runs) != 1 || !strings.HasPrefix(runs[0], "parked (waiting") || where != "sessions" {
		t.Errorf("in progress %q (listed by %q), %v; want the parked run alone", runs, where, err)
	}
}
