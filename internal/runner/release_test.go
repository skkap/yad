package runner

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/skkap/yad/protocol/v1"

	"github.com/skkap/yad/internal/account"
	"github.com/skkap/yad/internal/adapter"
	"github.com/skkap/yad/internal/adapter/fake"
	"github.com/skkap/yad/internal/capability"
	"github.com/skkap/yad/internal/config"
	hubdb "github.com/skkap/yad/internal/hub/store/db"
	"github.com/skkap/yad/internal/hubclient"
	"github.com/skkap/yad/internal/store"
	"github.com/skkap/yad/internal/store/db"
)

// Releasing a session (decision 0081), through yad hub in process with two
// runners on it: a session released by one opens on the other from its
// archive, under the same native id, with the conversation the first left.

// side is one runner of the two: its own profile, store, loop, executor and
// releases, registered with the env's hub.
type side struct {
	paths config.Paths
	store *store.Store
	loop  *Loop
	exec  *Exec
	rel   *Releases
	hub   *hubclient.Client
	// home is its Claude account's home, whose projects/ links to the
	// runner's shared transcripts.
	home string
}

// newSide registers a runner with the env's hub. Its Claude account is
// "work", so its conversations are in its own data directory.
func newSide(t *testing.T, e *env, ad adapter.Adapter, conn ...string) *side {
	t.Helper()
	name := "hub"
	if len(conn) > 0 {
		name = conn[0]
	}
	ctx := context.Background()
	p := config.Paths{Profile: "test", Config: t.TempDir(), Data: t.TempDir()}
	st, err := store.Open(ctx, p.StateDB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, err := p.RunnerID()
	if err != nil {
		t.Fatal(err)
	}
	doc := drivableDoc(id, 2)
	anon, err := hubclient.New(e.url, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := anon.Register(ctx, e.token(t), v1.RegisterRequest{Capabilities: doc})
	if err != nil {
		t.Fatal(err)
	}
	c, err := hubclient.New(e.url, res.RunnerCredential)
	if err != nil {
		t.Fatal(err)
	}
	home, err := account.Ensure(p.Data, "claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	cfg := accountConfig("work")
	accounts := NewAccounts(p.Data, account.ListsOf(cfg))
	x := &Exec{
		Store: st, Adapters: NewRegistry(ad), Config: cfg, Accounts: accounts, Data: p.Data, Paths: p,
		Binary: func(h string) (string, bool) { return "/nonexistent/" + h, true },
		Grace:  20 * time.Millisecond, TermGrace: 20 * time.Millisecond,
	}
	rel := &Releases{Store: st, Data: p.Data, Adapters: x.Adapters, Workdirs: nil, Accounts: accounts,
		Capabilities: func() v1.Capabilities { return doc }}
	x.Archives = rel.Hub
	l := &Loop{
		Connection: name, RunnerID: id, Hub: c, Store: st, Pool: NewPool(doc.Capacity),
		Capabilities: func() v1.Capabilities { return doc }, Executor: x, Clock: &fakeClock{now: time.Now()},
		Rand: func() float64 { return 0.5 }, Accounts: accounts, ReleaseSessions: true, Releases: rel,
	}
	rel.Join(name, c, nil)
	s := &side{paths: p, store: st, loop: l, exec: x, rel: rel, hub: c, home: home}
	x.init()
	rel.Workdirs = x.Workdirs
	return s
}

// sync is one sync of the side's loop, its runs waited for and their
// results delivered.
func (s *side) sync(t *testing.T) v1.SyncResponse {
	t.Helper()
	res := mustSync(t, s.loop)
	s.exec.Wait()
	NewReporter(s.loop.Connection, s.hub, s.store, nil).Flush(context.Background())
	return res
}

func (s *side) session(t *testing.T, id string) db.Session {
	t.Helper()
	sess, err := s.store.GetSession(context.Background(), db.GetSessionParams{Connection: s.loop.Connection, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func (s *side) result(t *testing.T, runID string) v1.Result {
	t.Helper()
	r, err := s.store.GetRun(context.Background(), db.GetRunParams{Connection: s.loop.Connection, ID: runID})
	if err != nil {
		t.Fatalf("run %s: %v", runID, err)
	}
	if !v1.RunState(r.State).IsTerminal() {
		t.Fatalf("run %s is %s", runID, r.State)
	}
	return v1.Result{State: v1.RunState(r.State), Error: &v1.RunError{Message: r.Reason.String}}
}

// conversation is the fake harness's: what the session was told, kept in
// the projects/ directory of the home the run ran in, as Claude keeps its
// transcripts.
type conversation struct {
	native  atomic.Value // the last native id a turn pinned
	starts  []adapter.Spec
	recalls []string
}

// harness is a fake Claude that keeps a conversation per native id: a new
// one gets an id of its own, a resume reads what the id's conversation was
// told before and is refused when there is none, and every turn appends its
// instruction.
func (c *conversation) harness(t *testing.T, then func(adapter.Spec)) *fake.Adapter {
	n := 0
	return &fake.Adapter{ID: "claude", Next: func(spec adapter.Spec) fake.Script {
		c.starts = append(c.starts, spec)
		root := filepath.Join(spec.Home, "projects")
		native := spec.NativeSessionID
		before := ""
		if native == "" {
			n++
			native = "0000000" + string(rune('0'+n)) + "-aaaa-bbbb-cccc-dddddddddddd"
		} else {
			b, err := os.ReadFile(fake.ConversationPath(root, native))
			if err != nil {
				err := &v1.RunError{Class: adapter.ClassSessionNotFound, Message: "No conversation found with session ID: " + native}
				return fake.Script{Outcome: adapter.Outcome{State: v1.RunFailed, Error: err, NativeSessionID: native}}
			}
			before = string(b)
		}
		c.recalls = append(c.recalls, before)
		if err := fake.WriteConversation(root, native, before+spec.Brief.Instruction+"\n"); err != nil {
			t.Error(err)
		}
		c.native.Store(native)
		if then != nil {
			then(spec)
		}
		return fake.Script{Outcome: adapter.Outcome{State: v1.RunSucceeded, FinalText: "earlier: " + before, NativeSessionID: native}}
	}}
}

func askRelease(t *testing.T, e *env, session string, discard bool) {
	t.Helper()
	n, err := e.hubStore.RequestSessionRelease(context.Background(), hubdb.RequestSessionReleaseParams{
		Now: sql.NullInt64{Int64: time.Now().UnixMilli(), Valid: true}, DiscardLocal: boolInt(discard), ID: session,
	})
	if err != nil || n != 1 {
		t.Fatalf("release of %s not asked for: %d, %v", session, n, err)
	}
}

func hubSessionRow(t *testing.T, e *env, id string) hubdb.Session {
	t.Helper()
	s, err := e.hubStore.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// release takes a's release of session through: the control, the archive's
// build and upload, and the report.
func (s *side) release(t *testing.T) {
	t.Helper()
	s.sync(t) // hears release_session
	s.rel.Sweep(context.Background())
	s.sync(t) // reports how it went
}

func TestAReleasedSessionResumesOnAnotherRunner(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	e := newEnv(t)
	conv := &conversation{}
	a := newSide(t, e, conv.harness(t, nil))
	b := newSide(t, e, conv.harness(t, nil), "other")

	first := testRun("r1", "s1")
	first.Brief.Instruction = "remember the word quokka"
	e.enqueue(t, first)
	a.sync(t)
	a.sync(t)
	native := conv.native.Load().(string)
	if s := a.session(t, "s1"); s.NativeID.String != native {
		t.Fatalf("a's session pinned %q, want %q", s.NativeID.String, native)
	}

	askRelease(t, e, "s1", false)
	// A run submitted meanwhile waits: the session is being released.
	second := continued("r2", "s1")
	second.Brief.Instruction = "what was the word?"
	e.enqueue(t, second)
	a.release(t)

	if s := a.session(t, "s1"); s.State != "closed" || s.CloseReason.String != "released" {
		t.Fatalf("a's session after the release: %s (%s)", s.State, s.CloseReason.String)
	}
	if _, err := os.Stat(fake.ConversationPath(account.TranscriptDir(a.paths.Data, "claude"), native)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a kept the released conversation (%v)", err)
	}
	if hs := hubSessionRow(t, e, "s1"); hs.RunnerID.Valid || !hs.ReleasedAt.Valid {
		t.Fatalf("the hub's session after the release: runner %v, released %v", hs.RunnerID, hs.ReleasedAt)
	}
	if got := e.hubState(t, "r2"); got != "queued" {
		t.Fatalf("the waiting run is %s, want queued for any runner", got)
	}

	// b takes it: offered opening the session with the archive, it fetches
	// it and resumes the conversation under a's native id.
	res := b.sync(t)
	if len(res.Runs) != 1 || res.Runs[0].Session.Archive == nil || !res.Runs[0].Session.New {
		t.Fatalf("b was offered %+v", res.Runs)
	}
	b.sync(t)
	if got := b.result(t, "r2"); got.State != v1.RunSucceeded {
		t.Fatalf("b's run ended %s: %s", got.State, got.Error.Message)
	}
	last := conv.starts[len(conv.starts)-1]
	if last.NativeSessionID != native || last.Home != b.home {
		t.Errorf("b resumed %q in %s; want %q in its own home %s", last.NativeSessionID, last.Home, native, b.home)
	}
	if got := conv.recalls[len(conv.recalls)-1]; got != "remember the word quokka\n" {
		t.Errorf("b's turn found the conversation %q", got)
	}
	if s := b.session(t, "s1"); s.NativeID.String != native || s.Archive.Valid {
		t.Errorf("b's session: native %q, archive still pending %v", s.NativeID.String, s.Archive.Valid)
	}
	// The run reached running on b, which opened it: the archive goes.
	b.sync(t)
	if _, err := e.hubStore.GetSessionArchive(context.Background(), "s1"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("the hub kept the archive once a run opened from it ran (%v)", err)
	}
	if hs := hubSessionRow(t, e, "s1"); hs.RunnerID.String != b.loop.RunnerID {
		t.Errorf("the session is bound to %q, want b", hs.RunnerID.String)
	}
}

// git runs one git command in dir for a test, failing it on an error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A session whose workdir holds work its remote does not is refused, and
// stays open on its runner as it was, unless the hub says discard_local; a
// session whose work is pushed goes, and the next runner checks out the
// branch the first worked on.
func TestAReleaseRefusesWorkTheRemoteDoesNotHave(t *testing.T) {
	for _, tc := range []struct {
		name    string
		work    func(t *testing.T, dir string)
		discard bool
		code    string
	}{
		{"changes not committed", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "notes.md"), []byte("half done"), 0o644)
		}, false, v1.ReleaseLocalChanges},
		{"commits not pushed", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "notes.md"), []byte("done"), 0o644)
			git(t, dir, "add", "notes.md")
			git(t, dir, "commit", "-q", "-m", "notes")
		}, false, v1.ReleaseUnpushed},
		{"commits not pushed, discarded", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "notes.md"), []byte("done"), 0o644)
			git(t, dir, "add", "notes.md")
			git(t, dir, "commit", "-q", "-m", "notes")
		}, true, ""},
		{"pushed", func(t *testing.T, dir string) {
			os.WriteFile(filepath.Join(dir, "notes.md"), []byte("done"), 0o644)
			git(t, dir, "add", "notes.md")
			git(t, dir, "commit", "-q", "-m", "notes")
			git(t, dir, "push", "-q", "origin", "HEAD")
		}, false, ""},
		{"nothing committed", func(t *testing.T, dir string) {}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			e := newEnv(t)
			root := t.TempDir()
			bare := gitRepo(t, root, "#!/bin/sh\ntrue\n")
			conv := &conversation{}
			worked := false
			a := newSide(t, e, conv.harness(t, func(spec adapter.Spec) {
				if !worked {
					worked = true
					tc.work(t, spec.Workdir)
				}
			}))
			a.exec.Config.Workdirs.Roots = []string{root}
			a.exec.Workdirs.Roots = []string{root}
			first := testRun("r1", "s1")
			first.Sources = []v1.Source{{Git: &v1.GitSource{URL: bare}}}
			e.enqueue(t, first)
			a.sync(t)
			a.sync(t)

			askRelease(t, e, "s1", tc.discard)
			a.release(t)
			hs := hubSessionRow(t, e, "s1")
			if tc.code != "" {
				var why v1.ReleaseError
				if !hs.ReleaseError.Valid || json.Unmarshal([]byte(hs.ReleaseError.String), &why) != nil || why.Code != tc.code || why.NextAction == "" {
					t.Fatalf("the hub heard %q, want a refusal %s with a next action", hs.ReleaseError.String, tc.code)
				}
				if s := a.session(t, "s1"); s.State != "open" || hs.RunnerID.String != a.loop.RunnerID || hs.ReleaseRequestedAt.Valid {
					t.Errorf("a refused release left the session %s on runner %q, asked %v", s.State, hs.RunnerID.String, hs.ReleaseRequestedAt.Valid)
				}
				return
			}
			if hs.RunnerID.Valid || !hs.ReleasedAt.Valid || hs.ReleaseError.Valid {
				t.Fatalf("the session was not released: runner %v, error %q", hs.RunnerID, hs.ReleaseError.String)
			}
			// The next runner builds the workdir from the remote, on the
			// branch the first worked on.
			b := newSide(t, e, conv.harness(t, nil), "other")
			b.exec.Config.Workdirs.Roots = []string{root}
			b.exec.Workdirs.Roots = []string{root}
			e.enqueue(t, continued("r2", "s1"))
			b.sync(t)
			b.sync(t)
			if got := b.result(t, "r2"); got.State != v1.RunSucceeded {
				t.Fatalf("b's run ended %s: %s", got.State, got.Error.Message)
			}
			dir := conv.starts[len(conv.starts)-1].Workdir
			if branch := git(t, dir, "symbolic-ref", "--short", "HEAD"); branch != "yad/hub/s1" {
				t.Errorf("b checked out %q, want the branch a worked on", branch)
			}
			_, err := os.Stat(filepath.Join(dir, "notes.md"))
			if pushed := tc.name == "pushed"; pushed != (err == nil) {
				t.Errorf("b's workdir has notes.md: %v; want it only when it was pushed", err == nil)
			}
		})
	}
}

// A session on a directory of the machine is never released, and nor is one
// with no conversation yet; a release asked for while a run is held in the
// session waits for it to end.
func TestAReleaseOfASessionThatCannotGo(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	e := newEnv(t)
	conv := &conversation{}
	a := newSide(t, e, conv.harness(t, nil))
	dir := t.TempDir()
	a.exec.Config.Workdirs.Roots = []string{dir}
	a.exec.Workdirs.Roots = []string{dir}
	onPath := testRun("r1", "on-path")
	onPath.Sources = []v1.Source{{Path: dir}}
	e.enqueue(t, onPath)
	a.sync(t)
	a.sync(t)
	askRelease(t, e, "on-path", false)
	a.release(t)
	if hs := hubSessionRow(t, e, "on-path"); !strings.Contains(hs.ReleaseError.String, v1.ReleasePathSource) || hs.RunnerID.String != a.loop.RunnerID {
		t.Errorf("a path source's release: %q", hs.ReleaseError.String)
	}

	// A session with a run held: asked, and nothing built until it ends.
	if err := a.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "busy", Harness: "claude", Workdir: dir}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.CreateRun(ctx, db.CreateRunParams{Connection: "hub", ID: "held", SessionID: "busy", Harness: "claude", Spec: "{}"}); err != nil {
		t.Fatal(err)
	}
	a.rel.Request(ctx, "hub", "busy", false)
	a.rel.Sweep(ctx)
	if r, err := a.store.GetRelease(ctx, db.GetReleaseParams{Connection: "hub", SessionID: "busy"}); err != nil || r.SettledAt.Valid || r.Archive.Valid {
		t.Fatalf("a release with a run held went ahead: %+v, %v", r, err)
	}
	if err := a.store.SetRunState(ctx, db.SetRunStateParams{State: "succeeded", Connection: "hub", ID: "held"}); err != nil {
		t.Fatal(err)
	}
	a.rel.Sweep(ctx)
	// It has no conversation, so once the run is out of the way it is
	// refused for that, and not for the run.
	r, err := a.store.GetRelease(ctx, db.GetReleaseParams{Connection: "hub", SessionID: "busy"})
	if err != nil || !strings.Contains(r.Error.String, v1.ReleaseNoConversation) {
		t.Fatalf("the release after the run ended: %+v, %v", r, err)
	}
	// And a session it does not hold is refused at once.
	a.rel.Request(ctx, "hub", "nowhere", false)
	if r, err := a.store.GetRelease(ctx, db.GetReleaseParams{Connection: "hub", SessionID: "nowhere"}); err != nil || !strings.Contains(r.Error.String, v1.ReleaseUnknown) {
		t.Errorf("an unknown session's release: %+v, %v", r, err)
	}
}

// flaky fails its first upload, as a network that drops mid-transfer does.
type flaky struct {
	ArchiveHub
	failed bool
}

func (f *flaky) PutArchive(ctx context.Context, session, sha string, size int64, body io.Reader) (v1.SessionArchive, error) {
	if !f.failed {
		f.failed = true
		io.CopyN(io.Discard, body, size/2)
		return v1.SessionArchive{}, errors.New("connection reset by peer")
	}
	return f.ArchiveHub.PutArchive(ctx, session, sha, size, body)
}

// An upload that fails is tried again, the archive built once, and the
// session stays open here until the hub has it.
func TestAnInterruptedUploadIsRetried(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	e := newEnv(t)
	conv := &conversation{}
	a := newSide(t, e, conv.harness(t, nil))
	f := &flaky{ArchiveHub: a.hub}
	a.rel.Join("hub", f, nil)
	now := time.Now()
	a.rel.Now = func() time.Time { return now }
	e.enqueue(t, testRun("r1", "s1"))
	a.sync(t)
	a.sync(t)
	askRelease(t, e, "s1", false)
	a.sync(t)
	a.rel.Sweep(ctx)
	r, err := a.store.GetRelease(ctx, db.GetReleaseParams{Connection: "hub", SessionID: "s1"})
	if err != nil || r.SettledAt.Valid || r.Attempts != 1 || !r.Archive.Valid {
		t.Fatalf("after a failed upload: %+v, %v", r, err)
	}
	if s := a.session(t, "s1"); s.State != "open" {
		t.Fatalf("the session closed before the hub had its archive: %s", s.State)
	}
	built := r.Sha256.String
	now = now.Add(time.Minute)
	a.rel.Sweep(ctx)
	a.sync(t)
	if hs := hubSessionRow(t, e, "s1"); hs.RunnerID.Valid || !hs.ReleasedAt.Valid {
		t.Fatalf("the retried upload did not release the session")
	}
	ar, err := e.hubStore.GetSessionArchive(ctx, "s1")
	if err != nil || ar.Sha256 != built {
		t.Errorf("the hub holds %q, want the archive built once, %s (%v)", ar.Sha256, built, err)
	}
}

// An archive whose bytes are not the ones the run named fails the run
// archive_rejected, and the session's next run here tries again.
func TestAnArchiveThatDoesNotMatchIsRejected(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	ctx := context.Background()
	e := newEnv(t)
	conv := &conversation{}
	a := newSide(t, e, conv.harness(t, nil))
	b := newSide(t, e, conv.harness(t, nil))
	e.enqueue(t, testRun("r1", "s1"))
	a.sync(t)
	a.sync(t)
	askRelease(t, e, "s1", false)
	a.release(t)
	good, err := e.hubStore.SessionArchiveData(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	bad := slices.Clone(good)
	bad[len(bad)/2] ^= 0xff
	if _, err := e.hubStore.DB.ExecContext(ctx, `UPDATE session_archives SET data = ? WHERE session_id = ?`, bad, "s1"); err != nil {
		t.Fatal(err)
	}
	e.enqueue(t, continued("r2", "s1"))
	b.sync(t)
	b.sync(t)
	got, err := b.store.GetRun(ctx, db.GetRunParams{Connection: "hub", ID: "r2"})
	if err != nil || got.State != "failed" || !strings.Contains(got.Reason.String, "is not the one the run named") {
		t.Fatalf("a mismatched archive: %+v, %v", got, err)
	}
	if res := hubResult(t, e, "r2"); res.Error == nil || res.Error.Class != ClassArchiveRejected {
		t.Fatalf("the hub heard %+v", res.Error)
	}
	if s := b.session(t, "s1"); !s.Archive.Valid || s.NativeID.Valid {
		t.Fatalf("b's session after a rejected archive: archive %v, native %q", s.Archive.Valid, s.NativeID.String)
	}
	// The hub's archive put right, the session's next run opens it.
	if _, err := e.hubStore.DB.ExecContext(ctx, `UPDATE session_archives SET data = ? WHERE session_id = ?`, good, "s1"); err != nil {
		t.Fatal(err)
	}
	e.enqueue(t, continued("r3", "s1"))
	b.sync(t)
	b.sync(t)
	if got := b.result(t, "r3"); got.State != v1.RunSucceeded {
		t.Fatalf("the next run after the archive was put right: %s %s", got.State, got.Error.Message)
	}
}

// A runner that cannot open a session from an archive is never offered the
// run that would: one whose owner turned release off for its hub advertises
// release nowhere, and the run waits for a runner that does.
func TestARunOpeningAReleasedSessionGoesOnlyWhereReleaseIsAdvertised(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	e := newEnv(t)
	conv := &conversation{}
	a := newSide(t, e, conv.harness(t, nil))
	off := newSide(t, e, conv.harness(t, nil))
	off.loop.ReleaseSessions = false
	doc := off.loop.document()
	if slices.Contains(doc.ProtocolFeatures, capability.FeatureRelease) {
		t.Errorf("a connection with release off advertises it runner-wide: %v", doc.ProtocolFeatures)
	}
	for _, h := range doc.Harnesses {
		if slices.Contains(h.Features, capability.FeatureRelease) {
			t.Errorf("a connection with release off advertises it for %s: %v", h.ID, h.Features)
		}
	}
	if on := a.loop.document(); !capability.RunMayUse(on, "claude", capability.FeatureRelease) {
		t.Fatalf("a connection with release on does not advertise it: %v", on.ProtocolFeatures)
	}

	e.enqueue(t, testRun("r1", "s1"))
	a.sync(t)
	a.sync(t)
	askRelease(t, e, "s1", false)
	a.release(t)
	e.enqueue(t, continued("r2", "s1"))
	for range 2 {
		if res := off.sync(t); len(res.Runs) != 0 {
			t.Fatalf("a runner with release off was offered %+v", res.Runs)
		}
	}
	if got := e.hubState(t, "r2"); got != "queued" {
		t.Errorf("the run is %s, want queued for a runner that can open it", got)
	}
	// A release_session sent to it anyway is refused, so its hub stops.
	off.loop.releaseSession(context.Background(), v1.Control{Kind: v1.ControlReleaseSession, SessionID: "s9"})
	rs, err := off.rel.Reports(context.Background(), "hub")
	if err != nil || len(rs) != 1 || rs[0].Error == nil || rs[0].Error.Code != v1.ReleaseNotReleasable {
		t.Errorf("a release asked of a connection with release off: %+v, %v", rs, err)
	}
}

// A conversation by the archive's id already on the opening runner is that
// session's own — left by an import whose end was never recorded — and is
// replaced, unless an open session there holds it. A run naming sources the
// archive does not carry is refused: the opening runner has only the
// archive's word for what the session was built from.
func TestAnArchiveMeetsWhatIsAlreadyThere(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, b *side, native string)
		run     func(v1.Run) v1.Run
		want    string // "" for success, else a phrase of the failure
	}{
		{"a leftover of this session's", func(t *testing.T, b *side, native string) {
			if err := fake.WriteConversation(account.TranscriptDir(b.paths.Data, "claude"), native, "stale\n"); err != nil {
				t.Fatal(err)
			}
		}, nil, ""},
		{"another open session's", func(t *testing.T, b *side, native string) {
			ctx := context.Background()
			if err := fake.WriteConversation(account.TranscriptDir(b.paths.Data, "claude"), native, "somebody's\n"); err != nil {
				t.Fatal(err)
			}
			if err := b.store.CreateSession(ctx, db.CreateSessionParams{Connection: "hub", ID: "holder", Harness: "claude", Workdir: "/w"}); err != nil {
				t.Fatal(err)
			}
			if err := b.store.SetSessionNativeID(ctx, db.SetSessionNativeIDParams{NativeID: sql.NullString{String: native, Valid: true}, Connection: "hub", ID: "holder"}); err != nil {
				t.Fatal(err)
			}
		}, nil, "another session on this runner holds"},
		{"other sources", nil, func(r v1.Run) v1.Run {
			r.Sources = []v1.Source{{Git: &v1.GitSource{URL: "https://example.com/other.git"}}}
			return r
		}, "sources other than"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
			ctx := context.Background()
			e := newEnv(t)
			conv := &conversation{}
			a := newSide(t, e, conv.harness(t, nil))
			b := newSide(t, e, conv.harness(t, nil))
			e.enqueue(t, testRun("r1", "s1"))
			a.sync(t)
			a.sync(t)
			native := conv.native.Load().(string)
			askRelease(t, e, "s1", false)
			a.release(t)
			if tc.prepare != nil {
				tc.prepare(t, b, native)
			}
			next := continued("r2", "s1")
			if tc.run != nil {
				next = tc.run(next)
			}
			e.enqueue(t, next)
			b.sync(t)
			b.sync(t)
			got, err := b.store.GetRun(ctx, db.GetRunParams{Connection: "hub", ID: "r2"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if got.State != "succeeded" || conv.recalls[len(conv.recalls)-1] != "do r1\n" {
					t.Fatalf("the run ended %s (%s) and found %q", got.State, got.Reason.String, conv.recalls[len(conv.recalls)-1])
				}
				return
			}
			if got.State != "failed" || !strings.Contains(got.Reason.String, tc.want) {
				t.Fatalf("the run ended %s: %q; want a failure saying %q", got.State, got.Reason.String, tc.want)
			}
			if res := hubResult(t, e, "r2"); res.Error == nil || res.Error.Class != ClassArchiveRejected {
				t.Errorf("the hub heard %+v", res.Error)
			}
		})
	}
}
